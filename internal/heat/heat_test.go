package heat

import (
	"testing"

	"github.com/kjn/attndb/internal/core"
)

func TestMinMaxNormalize(t *testing.T) {
	got := MinMax{}.Normalize([]float64{2, 4, 3})
	want := []float64{0, 1, 0.5}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MinMax %v: got %v want %v", []float64{2, 4, 3}, got, want)
		}
	}
	// all-equal: a pass that can't discriminate gives everything weight 1
	for _, v := range (MinMax{}).Normalize([]float64{5, 5, 5}) {
		if v != 1 {
			t.Fatalf("all-equal min-max should be 1, got %v", v)
		}
	}
	// empty input must not panic
	if len((MinMax{}).Normalize(nil)) != 0 {
		t.Fatal("empty normalize should be empty")
	}
}

func TestRankNormalize(t *testing.T) {
	got := Rank{}.Normalize([]float64{0.1, 0.9, 0.5}) // ranks: idx1 best, idx2, idx0
	if !(got[1] > got[2] && got[2] > got[0]) {
		t.Fatalf("Rank ordering wrong: %v", got)
	}
}

// TestAccumulateStacking verifies the core model: flat deposits stack, the peak
// is where the most passes agree, and a degenerate tiny span does NOT win.
func TestAccumulateStacking(t *testing.T) {
	deps := []Deposit{
		{DocID: "d", Span: core.Span{Start: 0, End: 1000}, Weight: 1},  // whole-doc prior
		{DocID: "d", Span: core.Span{Start: 500, End: 700}, Weight: 1}, // paragraph
		{DocID: "d", Span: core.Span{Start: 560, End: 580}, Weight: 1}, // exact phrase
		{DocID: "d", Span: core.Span{Start: 40, End: 49}, Weight: 1},   // tiny heading
	}
	peaks := Accumulate(deps, 0.5)
	if len(peaks) == 0 {
		t.Fatal("no peaks")
	}
	top := peaks[0]
	if top.Score != 3 {
		t.Fatalf("top peak score: got %v want 3 (doc+para+phrase stacked)", top.Score)
	}
	// the winning region must contain the exact phrase
	if !(top.Span.Start <= 560 && top.Span.End >= 580) {
		t.Fatalf("top peak %v does not contain the phrase [560,580)", top.Span)
	}
	// the tiny heading must NOT be the top result on its own
	if top.Span.Start == 40 && top.Span.End == 49 {
		t.Fatal("tiny heading span won — density pathology regressed")
	}
}

// TestAccumulateMultiPeak checks that NMS surfaces multiple non-overlapping peaks.
func TestAccumulateMultiPeak(t *testing.T) {
	deps := []Deposit{
		{DocID: "d", Span: core.Span{Start: 0, End: 100}, Weight: 1},   // peak A
		{DocID: "d", Span: core.Span{Start: 500, End: 600}, Weight: 1}, // peak B (disjoint)
	}
	peaks := Accumulate(deps, 0.5)
	if len(peaks) != 2 {
		t.Fatalf("expected 2 disjoint peaks, got %d: %+v", len(peaks), peaks)
	}
	for _, p := range peaks {
		if p.Span.Overlaps(core.Span{Start: 100, End: 500}) {
			t.Fatalf("peak leaked into the gap: %v", p.Span)
		}
	}
}

func TestAccumulatePerDocument(t *testing.T) {
	deps := []Deposit{
		{DocID: "a", Span: core.Span{Start: 0, End: 10}, Weight: 1},
		{DocID: "b", Span: core.Span{Start: 0, End: 10}, Weight: 2},
	}
	peaks := Accumulate(deps, 0.5)
	if len(peaks) != 2 {
		t.Fatalf("expected one peak per doc, got %d", len(peaks))
	}
	if peaks[0].DocID != "b" {
		t.Fatalf("higher-weight doc should rank first, got %q", peaks[0].DocID)
	}
}
