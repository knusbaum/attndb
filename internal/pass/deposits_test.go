package pass

import (
	"context"
	"testing"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/store"
)

// fakeMulti is a MultiVectorEncoder whose query encoding emits a fixed number of
// token vectors, so a test can control the MaxSim length-normalization divisor.
type fakeMulti struct{ nQueryTokens int }

func (fakeMulti) Dim() int { return 1 }
func (fakeMulti) EncodeDocs(_ context.Context, texts []string) ([]core.TokenVecs, error) {
	return make([]core.TokenVecs, len(texts)), nil
}
func (f fakeMulti) EncodeQuery(_ context.Context, _ string) (core.TokenVecs, error) {
	var tv core.TokenVecs
	for i := 0; i < f.nQueryTokens; i++ {
		tv.Vecs = append(tv.Vecs, []float32{1})
		tv.Offsets = append(tv.Offsets, core.Span{Start: i, End: i + 1})
	}
	return tv, nil
}

// fakePool returns one multivector hit with a fixed (sum-style) MaxSim score.
type fakePool struct{ score float32 }

func (fakePool) Name() string                                 { return "fake" }
func (fakePool) Upsert(context.Context, []store.Record) error { return nil }
func (fakePool) SearchSingle(context.Context, []float32, int, map[string]any) ([]store.Scored, error) {
	return nil, nil
}
func (p fakePool) SearchMulti(context.Context, core.TokenVecs, int, map[string]any) ([]store.Scored, error) {
	return []store.Scored{{
		Rec:   store.Record{DocID: "d", Span: core.Span{Start: 0, End: 10}},
		Score: p.score,
	}}, nil
}

// TestPerTokenLengthNormalization verifies that the per-token pass divides the
// raw MaxSim score (a sum over query tokens) by the query token count, yielding a
// length-invariant mean. Regression guard for the calibration-scale bug.
func TestPerTokenLengthNormalization(t *testing.T) {
	const rawSum = 6.0
	for _, nTok := range []int{1, 2, 3, 6} {
		p := NewPerTokenPass("tok", chunk.WholeDoc(), fakeMulti{nQueryTokens: nTok}, fakePool{score: rawSum})
		cands, err := p.Deposits(context.Background(), core.Query{Text: "q"}, 10)
		if err != nil {
			t.Fatalf("nTok=%d: deposits: %v", nTok, err)
		}
		if len(cands) != 1 {
			t.Fatalf("nTok=%d: expected 1 candidate, got %d", nTok, len(cands))
		}
		want := float32(rawSum) / float32(nTok)
		if cands[0].Score != want {
			t.Errorf("nTok=%d: score = %v, want %v (rawSum/nTok)", nTok, cands[0].Score, want)
		}
	}
}

// TestPerTokenEmptyQueryNoDivByZero guards the divisor when a query produces no
// token vectors: the score must pass through unchanged, not divide by zero.
func TestPerTokenEmptyQueryNoDivByZero(t *testing.T) {
	p := NewPerTokenPass("tok", chunk.WholeDoc(), fakeMulti{nQueryTokens: 0}, fakePool{score: 5})
	cands, err := p.Deposits(context.Background(), core.Query{Text: "q"}, 10)
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	if len(cands) != 1 || cands[0].Score != 5 {
		t.Fatalf("empty-query score should pass through as 5, got %+v", cands)
	}
}

// TestPassWeightPlumbing verifies WithWeight reaches Weight() for both pass kinds.
func TestPassWeightPlumbing(t *testing.T) {
	pt := NewPerTokenPass("tok", chunk.WholeDoc(), fakeMulti{nQueryTokens: 1}, fakePool{}, WithWeight(0.25))
	if pt.Weight() != 0.25 {
		t.Errorf("per-token weight = %v, want 0.25", pt.Weight())
	}
	sv := NewSingleVectorPass("doc", chunk.WholeDoc(), nil, nil, WithWeight(0.5))
	if sv.Weight() != 0.5 {
		t.Errorf("single-vector weight = %v, want 0.5", sv.Weight())
	}
	// default weight is 1
	def := NewSingleVectorPass("para", chunk.ByParagraph(), nil, nil)
	if def.Weight() != 1 {
		t.Errorf("default weight = %v, want 1", def.Weight())
	}
}
