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
	"runtime"
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

// ortVersion is the ONNX Runtime release the ./libs layout is fetched from; it
// appears in the shared library's filename on every platform.
const ortVersion = "1.27.0"

// defaultLibPath returns the ONNX Runtime shared library shipped alongside the
// native libs in ./libs (resolved relative to the working dir). The file
// extension and version placement are platform-specific — macOS names it
// libonnxruntime.<version>.dylib, Linux libonnxruntime.so.<version>. Override
// either with ATTNDB_ORT_LIB; the Makefile sets it explicitly.
func defaultLibPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "libs/libonnxruntime." + ortVersion + ".dylib"
	default:
		return "libs/libonnxruntime.so." + ortVersion
	}
}

var initOnce sync.Once
var initErr error

func ensureEnv() error {
	initOnce.Do(func() {
		lib := os.Getenv("ATTNDB_ORT_LIB")
		if lib == "" {
			lib = defaultLibPath()
		}
		ort.SetSharedLibraryPath(lib)
		initErr = ort.InitializeEnvironment()
	})
	return initErr
}

// buildSessionOptions returns a SessionOptions configured for the given
// execution provider. For "coreml", it appends the CoreML EP targeting the
// Metal GPU and enables verbose session logging so node-to-EP assignments are
// visible on stderr. It errors loud if the CoreML EP is not available in the
// loaded ONNX Runtime library — callers must not silently fall back to CPU.
// The caller is responsible for calling opts.Destroy() after the session is created.
func buildSessionOptions(provider string, noArena bool) (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("session options: %w", err)
	}
	// Disable MLAS's KleidiAI GEMM backend. On Apple Silicon (ORT ≥1.26) the
	// KleidiAI MatMul path allocates its GEMM working buffers with raw operator
	// new/malloc — bypassing the OrtAllocator interface — and pools them past
	// session lifetime, so each reconcile pass leaks ~14 GiB into libmalloc and
	// the daemon grows unbounded (see docs/proposal-onnx-memory.md, "ROOT CAUSE
	// FOUND"). Reverting to standard MLAS SGEMM keeps GEMM buffers bounded.
	if err := opts.AddSessionConfigEntry("mlas.disable_kleidiai", "1"); err != nil {
		opts.Destroy()
		return nil, fmt.Errorf("disable kleidiai: %w", err)
	}
	// noArena disables ORT's per-session CPU arena, so freed buffers go back to
	// the allocator each run instead of being pooled for the session's lifetime.
	// Used for the ephemeral ingest session, whose big whole-doc encodes are the
	// memory hog; the persistent query session keeps the arena (it is a
	// throughput win for the small, repeated query encodes). Combined with
	// closing the ingest session each reconcile pass, this bounds ingest's
	// working set. See docs/proposal-onnx-memory.md.
	if noArena {
		if err := opts.SetCpuMemArena(false); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("disable cpu arena: %w", err)
		}
	}
	if provider == "coreml" {
		// Verbose logging exposes which nodes ORT assigns to CoreMLExecutionProvider
		// vs CPUExecutionProvider. Watch stderr for "Node ... assigned to ...".
		if err := opts.SetLogSeverityLevel(ort.LoggingLevelVerbose); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("set log level: %w", err)
		}
		// CPUAndGPU = Metal GPU + CPU fallback. Use "All" to also include the ANE.
		if err := opts.AppendExecutionProviderCoreMLV2(map[string]string{
			"MLComputeUnits": "CPUAndGPU",
		}); err != nil {
			opts.Destroy()
			return nil, fmt.Errorf("CoreML EP unavailable (is the dylib built with CoreML support?): %w", err)
		}
	}
	return opts, nil
}

// ColBERT is a per-token (MultiVectorEncoder) over the exported model.
type ColBERT struct {
	tk       *tokenizers.Tokenizer
	sess     *ort.DynamicAdvancedSession
	skiplist map[uint32]bool
	padded   bool // pad inputs to fixed shapes (queryLen/docLen) for CoreML static graph
	mu       sync.Mutex
}

// NewColBERT loads the model and tokenizer from modelDir (expects model.onnx and
// tokenizer.json). provider is "cpu" or "coreml"; "coreml" enables the CoreML
// execution provider for Metal GPU acceleration.
func NewColBERT(modelDir, provider string) (*ColBERT, error) {
	if err := ensureEnv(); err != nil {
		return nil, fmt.Errorf("onnx env: %w", err)
	}
	tk, err := tokenizers.FromFile(modelDir + "/tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	opts, err := buildSessionOptions(provider, false) // ColBERT (300-token cap) isn't the memory hog
	if err != nil {
		tk.Close()
		return nil, err
	}
	defer opts.Destroy()
	sess, err := ort.NewDynamicAdvancedSession(modelDir+"/model.onnx",
		[]string{"input_ids", "attention_mask"}, []string{"output"}, opts)
	if err != nil {
		tk.Close()
		return nil, fmt.Errorf("load model: %w", err)
	}
	return &ColBERT{
		tk:       tk,
		sess:     sess,
		skiplist: buildSkiplist(tk),
		padded:   provider == "coreml",
	}, nil
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

	// For CoreML we pad to the fixed model caps so the graph compiles as a
	// static shape. Without this, CoreML can't partition the transformer
	// graph and silently falls back to CPU for all ops.
	fixedLen := 0
	if c.padded {
		if isDoc {
			fixedLen = docLen
		} else {
			fixedLen = queryLen
		}
	}
	data, err := c.run(ids, mask, fixedLen)
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
// If fixedLen > 0 the inputs are zero-padded (mask stays 0) to that length,
// giving CoreML a static shape it can compile.
func (c *ColBERT) run(ids, mask []int64, fixedLen int) ([]float32, error) {
	if fixedLen > len(ids) {
		pad := make([]int64, fixedLen-len(ids))
		ids = append(ids, pad...)   // pad token id 0 (ignored by attention)
		mask = append(mask, pad...) // 0 = masked out
	}
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
