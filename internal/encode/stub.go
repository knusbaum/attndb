// Package encode provides encoder implementations. The stub encoders here are
// deterministic hash-based embeddings: they share signal across identical tokens
// (so search ranks plausibly) but involve no ML. They let the full pipeline run
// before the real ONNX encoders are wired in. Real encoders will implement the
// same core.SingleVectorEncoder / core.MultiVectorEncoder interfaces.
package encode

import (
	"context"
	"hash/fnv"
	"strings"

	"github.com/kjn/attndb/internal/core"
)

// StubSingle is a deterministic single-vector encoder (bag-of-token-hashes).
type StubSingle struct{ dim int }

func NewStubSingle(dim int) *StubSingle { return &StubSingle{dim: dim} }

func (s *StubSingle) Dim() int { return s.dim }

func (s *StubSingle) EncodeDocs(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = bagVector(t, s.dim)
	}
	return out, nil
}

func (s *StubSingle) EncodeQuery(_ context.Context, q string) ([]float32, error) {
	return bagVector(q, s.dim), nil
}

// StubMulti is a deterministic per-token encoder. Each whitespace token gets a
// hash-seeded vector and its byte offset within the text.
type StubMulti struct{ dim int }

func NewStubMulti(dim int) *StubMulti { return &StubMulti{dim: dim} }

func (s *StubMulti) Dim() int { return s.dim }

func (s *StubMulti) EncodeDocs(_ context.Context, texts []string) ([]core.TokenVecs, error) {
	out := make([]core.TokenVecs, len(texts))
	for i, t := range texts {
		out[i] = tokenVecs(t, s.dim)
	}
	return out, nil
}

func (s *StubMulti) EncodeQuery(_ context.Context, q string) (core.TokenVecs, error) {
	return tokenVecs(q, s.dim), nil
}

// --- helpers ---

// tokenVecs splits text on whitespace and returns a hash-seeded vector and byte
// span per token.
func tokenVecs(text string, dim int) core.TokenVecs {
	var tv core.TokenVecs
	start := -1
	emit := func(s, e int) {
		tv.Vecs = append(tv.Vecs, tokenVector(text[s:e], dim))
		tv.Offsets = append(tv.Offsets, core.Span{Start: s, End: e})
	}
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if start >= 0 {
				emit(start, i)
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		emit(start, len(text))
	}
	return tv
}

// bagVector averages the per-token vectors of a text and normalizes.
func bagVector(text string, dim int) []float32 {
	v := make([]float32, dim)
	n := 0
	for _, tok := range strings.Fields(text) {
		tv := tokenVector(tok, dim)
		for i := range v {
			v[i] += tv[i]
		}
		n++
	}
	return core.Normalize(v)
}

// tokenVector deterministically maps a token to a unit vector via an FNV-seeded
// linear congruential generator.
func tokenVector(tok string, dim int) []float32 {
	h := fnv.New64a()
	h.Write([]byte(strings.ToLower(tok)))
	state := h.Sum64() | 1
	v := make([]float32, dim)
	for i := range v {
		// xorshift-style step for a stable pseudo-random sequence
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		v[i] = float32(int64(state>>11))/float32(1<<52) - 1
	}
	return core.Normalize(v)
}
