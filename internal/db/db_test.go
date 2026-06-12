package db

import (
	"context"
	"strings"
	"testing"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/encode"
	"github.com/kjn/attndb/internal/pass"
	"github.com/kjn/attndb/internal/store"
)

// TestSearchEndToEnd exercises ingest → deposit → normalize → accumulate →
// extract with stub encoders and in-memory pools, and checks that a query
// localizes to the relevant section rather than the whole document.
func TestSearchEndToEnd(t *testing.T) {
	single := encode.NewStubSingle(64)
	multi := encode.NewStubMulti(64)
	d := New([]pass.Pass{
		pass.NewPerTokenPass("tok", chunk.BySection(256, 32), multi, store.NewMemory("tok")),
		pass.NewSingleVectorPass("para", chunk.ByParagraph(), single, store.NewMemory("para")),
		pass.NewSingleVectorPass("doc", chunk.WholeDoc(), single, store.NewMemory("doc")),
	})

	doc := core.Document{
		ID: "d",
		Text: "# Spec\n\n## Caching\n\nResponses are cached for sixty seconds in memory.\n\n" +
			"## Retries\n\nClients retry with exponential backoff and jitter on failure.\n",
	}
	ctx := context.Background()
	if err := d.Ingest(ctx, []core.Document{doc}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	results, err := d.Search(ctx, core.Query{Text: "retry backoff on failure"}, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	top := results[0]
	if !strings.Contains(top.Snippet, "backoff") {
		t.Fatalf("top result not the retries section: %q", top.Snippet)
	}
	// must be localized, not the entire document
	if top.Span.Start == 0 && top.Span.End == len(doc.Text) {
		t.Fatalf("result was the whole document, not localized: %v", top.Span)
	}
}

func TestSearchNoResults(t *testing.T) {
	d := New(nil)
	results, err := d.Search(context.Background(), core.Query{Text: "anything"}, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results from empty DB, got %d", len(results))
	}
}
