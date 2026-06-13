//go:build onnx

// Package onnx implements the real GTE-ModernColBERT encoder over ONNX Runtime.
// It is behind the `onnx` build tag because it links native libraries
// (libonnxruntime via onnxruntime_go, libtokenizers via CGo); the rest of attndb
// builds and tests without them.
//
// Build/run:
//
//	CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx ./...
//
// The ONNX graph already applies the 768→128 projection and per-token L2
// normalization, so this code only does ColBERT tokenization (markers, length
// caps, document punctuation skiplist) and tensor plumbing.
package onnx

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/daulet/tokenizers"
	"github.com/kjn/attndb/internal/core"
	ort "github.com/yalue/onnxruntime_go"
)

// Special token ids from the exported tokenizer (config_sentence_transformers.json).
const (
	clsID = 50281
	sepID = 50282
	qID   = 50368 // "[Q] "
	dID   = 50369 // "[D] "

	queryLen = 48  // includes CLS/[Q]/SEP
	docLen   = 300 // includes CLS/[D]/SEP
	dim      = 128
)

// punctuation is ColBERT's document skiplist (config skiplist_words).
const punctuation = `!"#$%&'()*+,-./:;<=>?@[\]^_` + "`" + `{|}~`

// defaultLibPath is the Python-bundled ONNX Runtime; override with ATTNDB_ORT_LIB.
const defaultLibPath = "/home/kjn/.pyenv/versions/3.11.13/lib/python3.11/site-packages/onnxruntime/capi/libonnxruntime.so.1.20.1"

var initOnce sync.Once
var initErr error

func ensureEnv() error {
	initOnce.Do(func() {
		lib := os.Getenv("ATTNDB_ORT_LIB")
		if lib == "" {
			lib = defaultLibPath
		}
		ort.SetSharedLibraryPath(lib)
		initErr = ort.InitializeEnvironment()
	})
	return initErr
}

// ColBERT is a per-token (MultiVectorEncoder) over the exported model.
type ColBERT struct {
	tk       *tokenizers.Tokenizer
	sess     *ort.DynamicAdvancedSession
	skiplist map[uint32]bool
	mu       sync.Mutex // serialize tokenizer + session use
}

// NewColBERT loads the model and tokenizer from modelDir (expects model.onnx and
// tokenizer.json).
func NewColBERT(modelDir string) (*ColBERT, error) {
	if err := ensureEnv(); err != nil {
		return nil, fmt.Errorf("onnx env: %w", err)
	}
	tk, err := tokenizers.FromFile(modelDir + "/tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSession(modelDir+"/model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"output"}, nil)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("load model: %w", err)
	}
	return &ColBERT{tk: tk, sess: sess, skiplist: buildSkiplist(tk)}, nil
}

func (c *ColBERT) Dim() int { return dim }

func (c *ColBERT) Close() error {
	c.tk.Close()
	return c.sess.Destroy()
}

func (c *ColBERT) EncodeDocs(_ context.Context, texts []string) ([]core.TokenVecs, error) {
	out := make([]core.TokenVecs, len(texts))
	for i, t := range texts {
		tv, err := c.encode(t, true)
		if err != nil {
			return nil, err
		}
		out[i] = tv
	}
	return out, nil
}

func (c *ColBERT) EncodeQuery(_ context.Context, query string) (core.TokenVecs, error) {
	return c.encode(query, false)
}

// encode runs one text through the model and returns its per-token vectors with
// byte offsets (into text). Document mode prepends [D] and drops punctuation
// tokens; query mode prepends [Q] and keeps everything.
func (c *ColBERT) encode(text string, isDoc bool) (core.TokenVecs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	enc := c.tk.EncodeWithOptions(text, false, tokenizers.WithReturnOffsets())
	marker, maxLen := int64(qID), queryLen
	if isDoc {
		marker, maxLen = int64(dID), docLen
	}
	nContent := len(enc.IDs)
	if cap := maxLen - 3; nContent > cap { // reserve CLS, marker, SEP
		nContent = cap
	}

	ids := make([]int64, 0, nContent+3)
	ids = append(ids, clsID, marker)
	for i := 0; i < nContent; i++ {
		ids = append(ids, int64(enc.IDs[i]))
	}
	ids = append(ids, sepID)

	mask := make([]int64, len(ids))
	for i := range mask {
		mask[i] = 1
	}

	data, err := c.run(ids, mask)
	if err != nil {
		return core.TokenVecs{}, err
	}

	// content tokens occupy positions 2 .. 2+nContent-1 in the output
	var tv core.TokenVecs
	for i := 0; i < nContent; i++ {
		id := enc.IDs[i]
		if isDoc && c.skiplist[id] {
			continue
		}
		pos := i + 2
		vec := make([]float32, dim)
		copy(vec, data[pos*dim:(pos+1)*dim])
		off := enc.Offsets[i]
		tv.Vecs = append(tv.Vecs, vec)
		tv.Offsets = append(tv.Offsets, core.Span{Start: int(off[0]), End: int(off[1])})
	}
	return tv, nil
}

// run executes the session and returns the flattened [seq*dim] output.
func (c *ColBERT) run(ids, mask []int64) ([]float32, error) {
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
	if err := c.sess.Run([]ort.Value{idT, mT}, outputs); err != nil {
		return nil, err
	}
	out := outputs[0].(*ort.Tensor[float32])
	defer out.Destroy()
	// copy out of the tensor before it is destroyed
	src := out.GetData()
	cp := make([]float32, len(src))
	copy(cp, src)
	return cp, nil
}

// buildSkiplist maps each punctuation character to its token id(s), replicating
// pylate's document skiplist.
func buildSkiplist(tk *tokenizers.Tokenizer) map[uint32]bool {
	skip := map[uint32]bool{}
	for _, r := range punctuation {
		enc := tk.EncodeWithOptions(string(r), false)
		for _, id := range enc.IDs {
			skip[id] = true
		}
	}
	return skip
}
