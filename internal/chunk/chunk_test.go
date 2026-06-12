package chunk

import (
	"testing"

	"github.com/kjn/attndb/internal/core"
)

// spanInvariant: for every chunker, doc.Text[chunk.Span] must equal chunk.Text.
// This is the contract that lets differently-chunked passes share one coordinate
// system.
func TestSpanInvariant(t *testing.T) {
	doc := core.Document{
		ID:   "d",
		Text: "# Title\n\n## Alpha\n\nbody of alpha here\n\n## Beta\n\nbody of beta here\n",
	}
	chunkers := map[string]Chunker{
		"WholeDoc":    WholeDoc(),
		"ByParagraph": ByParagraph(),
		"FixedWindow": FixedWindow(3, 1),
		"BySection":   BySection(64, 8),
	}
	for name, c := range chunkers {
		chunks := c.Chunk(doc)
		if len(chunks) == 0 {
			t.Errorf("%s produced no chunks", name)
		}
		for _, ch := range chunks {
			if got := doc.Text[ch.Span.Start:ch.Span.End]; got != ch.Text {
				t.Errorf("%s: doc[%d:%d]=%q != chunk.Text=%q", name, ch.Span.Start, ch.Span.End, got, ch.Text)
			}
		}
	}
}

func TestByParagraph(t *testing.T) {
	doc := core.Document{ID: "d", Text: "alpha\n\nbeta\n\ngamma"}
	chunks := ByParagraph().Chunk(doc)
	if len(chunks) != 3 {
		t.Fatalf("got %d paragraphs want 3", len(chunks))
	}
	if chunks[0].Text != "alpha" || chunks[2].Text != "gamma" {
		t.Fatalf("paragraph bodies wrong: %q .. %q", chunks[0].Text, chunks[2].Text)
	}
}

func TestBySectionHeadingPath(t *testing.T) {
	doc := core.Document{ID: "d", Text: "# Title\n\n## Alpha\n\nbody of alpha\n\n## Beta\n\nbody of beta\n"}
	chunks := BySection(64, 8).Chunk(doc)
	var foundAlpha, foundBeta bool
	for _, c := range chunks {
		last := ""
		if n := len(c.HeadingPath); n > 0 {
			last = c.HeadingPath[n-1]
		}
		if last == "Alpha" && contains(c.Text, "body of alpha") {
			foundAlpha = true
			if c.HeadingPath[0] != "Title" {
				t.Errorf("Alpha heading path missing Title root: %v", c.HeadingPath)
			}
		}
		if last == "Beta" && contains(c.Text, "body of beta") {
			foundBeta = true
		}
	}
	if !foundAlpha || !foundBeta {
		t.Fatalf("missing sections: alpha=%v beta=%v (%+v)", foundAlpha, foundBeta, chunks)
	}
}

func TestFixedWindowOverlap(t *testing.T) {
	doc := core.Document{ID: "d", Text: "w1 w2 w3 w4"}
	chunks := FixedWindow(2, 1).Chunk(doc)
	if len(chunks) != 3 {
		t.Fatalf("got %d windows want 3", len(chunks))
	}
	if chunks[0].Text != "w1 w2" {
		t.Fatalf("first window=%q want %q", chunks[0].Text, "w1 w2")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
