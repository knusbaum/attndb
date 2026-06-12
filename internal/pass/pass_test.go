package pass

import (
	"testing"

	"github.com/kjn/attndb/internal/core"
)

// TestToDocCoords verifies that token offsets are translated from encoded-text
// coordinates (which include the heading-trail prefix) back to document
// coordinates, and that heading-prefix tokens are dropped.
func TestToDocCoords(t *testing.T) {
	c := core.Chunk{
		Span:        core.Span{Start: 100, End: 110},
		Text:        "abcdefghij",
		HeadingPath: []string{"H"},
	}
	enc := encodeText(c) // "H\n\nabcdefghij", prefix = 3
	if enc != "H\n\nabcdefghij" {
		t.Fatalf("encodeText=%q", enc)
	}
	tv := core.TokenVecs{
		Vecs:    [][]float32{{1}, {2}, {3}},
		Offsets: []core.Span{{Start: 0, End: 1}, {Start: 3, End: 4}, {Start: 12, End: 13}},
	}
	out := toDocCoords(tv, c, enc)
	if out.Len() != 2 {
		t.Fatalf("expected 2 body tokens (heading dropped), got %d", out.Len())
	}
	want := []core.Span{{Start: 100, End: 101}, {Start: 109, End: 110}}
	for i, w := range want {
		if out.Offsets[i] != w {
			t.Errorf("offset %d = %v want %v", i, out.Offsets[i], w)
		}
	}
	if out.Vecs[0][0] != 2 || out.Vecs[1][0] != 3 {
		t.Errorf("vecs not aligned with kept tokens: %v", out.Vecs)
	}
}

func TestMatchedSpan(t *testing.T) {
	d := core.TokenVecs{Offsets: []core.Span{{Start: 100, End: 101}, {Start: 109, End: 110}}}
	if got := matchedSpan(d, []int{0, 1}); got != (core.Span{Start: 100, End: 110}) {
		t.Errorf("matchedSpan all = %v want {100,110}", got)
	}
	if got := matchedSpan(d, []int{1}); got != (core.Span{Start: 109, End: 110}) {
		t.Errorf("matchedSpan [1] = %v want {109,110}", got)
	}
	if got := matchedSpan(d, []int{-1}); got != (core.Span{}) {
		t.Errorf("matchedSpan no-match = %v want zero span", got)
	}
}
