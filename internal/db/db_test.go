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
	snippet := doc.Text[top.Span.Start:top.Span.End]
	if !strings.Contains(snippet, "backoff") {
		t.Fatalf("top result not the retries section: %q", snippet)
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

// TestCalibrationSwapRace runs Search concurrently with SetCalibration to prove
// the calibration swap is data-race-free (run with -race).
func TestCalibrationSwapRace(t *testing.T) {
	single := encode.NewStubSingle(64)
	multi := encode.NewStubMulti(64)
	d := New([]pass.Pass{
		pass.NewPerTokenPass("tok", chunk.BySection(256, 32), multi, store.NewMemory("tok")),
		pass.NewSingleVectorPass("para", chunk.ByParagraph(), single, store.NewMemory("para")),
		pass.NewSingleVectorPass("doc", chunk.WholeDoc(), single, store.NewMemory("doc")),
	})
	ctx := context.Background()
	d.Ingest(ctx, []core.Document{{ID: "d", Text: "# T\n\nRetries use backoff.\n"}})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			d.SetCalibration(Calibration{
				"tok":  {Lo: 0.1, Hi: 0.9},
				"para": {Lo: 0.2, Hi: 0.8},
				"doc":  {Lo: 0.3, Hi: 0.7},
			})
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		if _, err := d.Search(ctx, core.Query{Text: "backoff"}, 3); err != nil {
			t.Fatalf("search: %v", err)
		}
	}
	<-done
}
