//go:build onnx

package onnx

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"

	"github.com/daulet/tokenizers"
	"github.com/kjn/attndb/internal/core"
	ort "github.com/yalue/onnxruntime_go"
)

// singleDim is the embedding dimension of gte-modernbert-base.
const singleDim = 768

// maxSingleLen caps the input length fed to the single-vector model. Its native
// max is 8192, but attention is O(n²), so a single whole-doc encode near the cap
// has a multi-GB working set (≈9–17 GiB at 8192) that dominates the daemon's
// memory high-water. Lower it to bound peak/steady memory (a smaller cap = a
// coarser doc-level vector; the paragraph pass still carries fine detail).
// Override with ATTNDB_MAX_SINGLE_LEN.
var maxSingleLen = envInt("ATTNDB_MAX_SINGLE_LEN", 8192)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// coreMlSingleLen is the fixed sequence length used in CoreML mode. CoreML
// requires a static graph shape; 512 covers typical paragraph/section chunks
// while keeping the compiled model tractable.
const coreMlSingleLen = 512

// SingleVector is a dedicated single-vector encoder (core.SingleVectorEncoder)
// over a sentence-transformers model exported with pooling + L2 normalization
// baked into the ONNX graph, so the output is already one normalized vector.
type SingleVector struct {
	tk      *tokenizers.Tokenizer
	sess    *ort.DynamicAdvancedSession
	seqCap  int
	lengths []int // padded sequence lengths, ascending
	mu      sync.Mutex
}

// NewSingleVector loads the model and tokenizer from modelDir (expects model.onnx
// and tokenizer.json). noArena disables ORT's CPU
// arena (used for the ephemeral ingest session, so its large per-encode buffers
// are not pooled for the session's lifetime).
func NewSingleVector(modelDir, provider string, noArena bool) (*SingleVector, error) {
	if err := ensureEnv(); err != nil {
		return nil, fmt.Errorf("onnx env: %w", err)
	}
	tk, err := tokenizers.FromFile(modelDir + "/tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	modelPath := modelDir + "/model.onnx"
	opts, err := buildSessionOptions(provider, modelPath, noArena)
	if err != nil {
		tk.Close()
		return nil, err
	}
	defer opts.Destroy()
	sess, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask"}, []string{"sentence_embedding"}, opts)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("load model: %w", err)
	}
	encoder := &SingleVector{tk: tk, sess: sess, seqCap: maxSingleLen}
	if provider == "coreml" {
		encoder.seqCap = coreMlSingleLen
		encoder.lengths = []int{coreMlSingleLen}
	}
	if provider == "migraphx" {
		// Keep at most four compiled programs resident and use the smallest
		// shape that fits each input. The last shape preserves the token cap.
		for _, length := range []int{128, 512, 2048} {
			if length < encoder.seqCap {
				encoder.lengths = append(encoder.lengths, length)
			}
		}
		encoder.lengths = append(encoder.lengths, encoder.seqCap)
		for _, length := range encoder.lengths {
			ids, mask := make([]int64, length), make([]int64, length)
			mask[0] = 1
			if _, err := encoder.run(ids, mask); err != nil {
				encoder.Close()
				return nil, fmt.Errorf("warm up pooled encoder on MIGraphX at length %d: %w", length, err)
			}
		}
	}
	return encoder, nil
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
	n := min(len(enc.IDs), s.seqCap)
	allocLen := n
	if len(s.lengths) > 0 {
		allocLen = s.lengths[sort.SearchInts(s.lengths, n)]
	}
	ids := make([]int64, allocLen)
	mask := make([]int64, allocLen)
	for i := 0; i < n; i++ {
		ids[i] = int64(enc.IDs[i])
		mask[i] = 1
	}
	// ids[n:] and mask[n:] are already zero (pad token, masked out)
	return s.run(ids, mask)
}

func (s *SingleVector) run(ids, mask []int64) ([]float32, error) {
	shape := ort.NewShape(1, int64(len(ids)))
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
