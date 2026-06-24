package db

import (
	"context"
	"math"
	"testing"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/encode"
	"github.com/kjn/attndb/internal/pass"
	"github.com/kjn/attndb/internal/store"
)

// testDB builds a 3-pass DB over stub encoders + in-memory pools and ingests one
// multi-section document. opts are forwarded to New (e.g. WithRecallK).
func testDB(t *testing.T, opts ...Option) *DB {
	t.Helper()
	single := encode.NewStubSingle(64)
	multi := encode.NewStubMulti(64)
	d := New([]pass.Pass{
		pass.NewPerTokenPass("tok", chunk.BySection(256, 32), multi, store.NewMemory("tok")),
		pass.NewSingleVectorPass("para", chunk.ByParagraph(), single, store.NewMemory("para")),
		pass.NewSingleVectorPass("doc", chunk.WholeDoc(), single, store.NewMemory("doc"), pass.WithWeight(0.25)),
	}, opts...)
	doc := core.Document{
		ID: "d",
		Text: "# Spec\n\n## Caching\n\nResponses are cached for sixty seconds in memory.\n\n" +
			"## Retries\n\nClients retry with exponential backoff and jitter on failure.\n",
	}
	if err := d.Ingest(context.Background(), []core.Document{doc}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return d
}

// TestCalibrateAnchorsHiOnPositivesLoOnNegatives verifies the positive/negative
// split: HI is derived only from the positive queries, LO only from the
// negatives. With no negatives, LO must be 0.
func TestCalibrateAnchorsHiOnPositivesLoOnNegatives(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	positives := []string{"responses are cached in memory", "clients retry with backoff"}
	negsA := []string{"banana volcano saxophone"}
	negsB := []string{"unrelated query about cooking pasta"}

	noNeg, err := d.Calibrate(ctx, positives, nil)
	if err != nil {
		t.Fatalf("calibrate (no negatives): %v", err)
	}
	withNegA, err := d.Calibrate(ctx, positives, negsA)
	if err != nil {
		t.Fatalf("calibrate (negs A): %v", err)
	}
	withNegB, err := d.Calibrate(ctx, positives, negsB)
	if err != nil {
		t.Fatalf("calibrate (negs B): %v", err)
	}

	if len(noNeg) == 0 {
		t.Fatal("expected calibration entries")
	}
	for name, pc := range noNeg {
		// No negatives -> LO floored at 0.
		if pc.Lo != 0 {
			t.Errorf("pass %q: Lo with no negatives = %v, want 0", name, pc.Lo)
		}
		// HI depends only on the positives, so it must be identical regardless of
		// which negatives we pass.
		if withNegA[name].Hi != pc.Hi || withNegB[name].Hi != pc.Hi {
			t.Errorf("pass %q: Hi changed with negatives (%v vs A=%v B=%v); Hi must depend only on positives",
				name, pc.Hi, withNegA[name].Hi, withNegB[name].Hi)
		}
		// Range must stay ordered.
		if withNegA[name].Hi <= withNegA[name].Lo {
			t.Errorf("pass %q: Hi (%v) <= Lo (%v)", name, withNegA[name].Hi, withNegA[name].Lo)
		}
	}
}

// TestExplainMatchesSearch verifies Explain returns one breakdown per pass, that
// each candidate's Contribution == Weight*Norm, and that its fused Peaks match
// what Search returns for the same query.
func TestExplainMatchesSearch(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	q := core.Query{Text: "retry backoff on failure"}

	exp, err := d.Explain(ctx, q, 5)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(exp.Passes) != 3 {
		t.Fatalf("expected 3 pass breakdowns, got %d", len(exp.Passes))
	}
	for _, pe := range exp.Passes {
		for _, c := range pe.Cands {
			if math.Abs(c.Contribution-pe.Weight*c.Norm) > 1e-9 {
				t.Errorf("pass %q: contribution %v != weight*norm (%v*%v)", pe.Pass, c.Contribution, pe.Weight, c.Norm)
			}
		}
	}

	got, err := d.Search(ctx, q, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != len(exp.Peaks) {
		t.Fatalf("Explain peaks (%d) != Search results (%d)", len(exp.Peaks), len(got))
	}
	for i := range got {
		if got[i].DocID != exp.Peaks[i].DocID || got[i].Span != exp.Peaks[i].Span || got[i].Score != exp.Peaks[i].Score {
			t.Errorf("result %d differs: search=%+v explain=%+v", i, got[i], exp.Peaks[i])
		}
	}
}

// TestRecallKLimitsCandidates verifies WithRecallK caps how many candidates each
// pass contributes, observable through Explain.
func TestRecallKLimitsCandidates(t *testing.T) {
	d := testDB(t, WithRecallK(1))
	exp, err := d.Explain(context.Background(), core.Query{Text: "retry backoff on failure"}, 5)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for _, pe := range exp.Passes {
		if len(pe.Cands) > 1 {
			t.Errorf("pass %q returned %d candidates with recallK=1", pe.Pass, len(pe.Cands))
		}
	}
}
