//go:build onnx

package onnx

import (
	"context"
	"fmt"
	"sync"

	"github.com/daulet/tokenizers"
	"github.com/kjn/attndb/internal/core"
	ort "github.com/yalue/onnxruntime_go"
)

// singleDim is the embedding dimension of gte-modernbert-base.
const singleDim = 768

// maxSingleLen caps the input length fed to the single-vector model (its native
// max is 8192; whole-document chunks are truncated to a coarse gestalt).
const maxSingleLen = 8192

// SingleVector is a dedicated single-vector encoder (core.SingleVectorEncoder)
// over a sentence-transformers model exported with pooling + L2 normalization
// baked into the ONNX graph, so the output is already one normalized vector.
type SingleVector struct {
	tk   *tokenizers.Tokenizer
	sess *ort.DynamicAdvancedSession
	mu   sync.Mutex
}

// NewSingleVector loads the model and tokenizer from modelDir (expects model.onnx
// and tokenizer.json).
func NewSingleVector(modelDir string) (*SingleVector, error) {
	if err := ensureEnv(); err != nil {
		return nil, fmt.Errorf("onnx env: %w", err)
	}
	tk, err := tokenizers.FromFile(modelDir + "/tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSession(modelDir+"/model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"sentence_embedding"}, nil)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("load model: %w", err)
	}
	return &SingleVector{tk: tk, sess: sess}, nil
}

func (s *SingleVector) Dim() int { return singleDim }

func (s *SingleVector) Close() error {
	s.tk.Close()
	return s.sess.Destroy()
}

func (s *SingleVector) EncodeDocs(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := s.encode(t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (s *SingleVector) EncodeQuery(_ context.Context, query string) ([]float32, error) {
	return s.encode(query)
}

func (s *SingleVector) encode(text string) ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// addSpecialTokens=true: the model expects [CLS] … [SEP]. No ColBERT markers.
	enc := s.tk.EncodeWithOptions(text, true)
	n := len(enc.IDs)
	if n > maxSingleLen {
		n = maxSingleLen
	}
	ids := make([]int64, n)
	mask := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = int64(enc.IDs[i])
		mask[i] = 1
	}

	shape := ort.NewShape(1, int64(n))
	idT, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer idT.Destroy()
	mT, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer mT.Destroy()

	outputs := []ort.Value{nil}
	if err := s.sess.Run([]ort.Value{idT, mT}, outputs); err != nil {
		return nil, err
	}
	out := outputs[0].(*ort.Tensor[float32])
	defer out.Destroy()

	src := out.GetData() // [1, singleDim], pooled but NOT normalized by this model
	vec := make([]float32, singleDim)
	copy(vec, src[:singleDim])
	return core.Normalize(vec), nil
}
