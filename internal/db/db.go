// Package db composes passes into a searchable database. Search collects each
// pass's scored intervals, normalizes them per pass, deposits them (weighted) on
// a per-document heatmap, and returns the accumulated peaks.
package db

import (
	"context"
	"fmt"
	"sort"

	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/heat"
	"github.com/kjn/attndb/internal/pass"
)

// DB is a multi-pass database.
type DB struct {
	passes       []pass.Pass
	norm         heat.Normalizer
	recallK      int
	peakFraction float64
	docText      map[string]string
}

// Option configures a DB.
type Option func(*DB)

// WithNormalizer sets the per-pass score normalizer (default heat.MinMax).
func WithNormalizer(n heat.Normalizer) Option { return func(d *DB) { d.norm = n } }

// WithRecallK sets how many deposits each pass contributes (default 50).
func WithRecallK(k int) Option { return func(d *DB) { d.recallK = k } }

// WithPeakFraction sets how far peak regions grow around their maximum (default 0.5).
func WithPeakFraction(f float64) Option { return func(d *DB) { d.peakFraction = f } }

// New composes passes into a DB.
func New(passes []pass.Pass, opts ...Option) *DB {
	d := &DB{
		passes:       passes,
		norm:         heat.MinMax{},
		recallK:      50,
		peakFraction: 0.5,
		docText:      map[string]string{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Ingest runs every pass over the documents and remembers their text for snippet
// rendering.
func (d *DB) Ingest(ctx context.Context, docs []core.Document) error {
	for _, doc := range docs {
		d.docText[doc.ID] = doc.Text
	}
	for _, p := range d.passes {
		if err := p.Ingest(ctx, docs); err != nil {
			return fmt.Errorf("ingest pass %s: %w", p.Name(), err)
		}
	}
	return nil
}

// Search collects deposits from every pass, normalizes per pass, accumulates the
// heatmap, and returns ranked peaks.
func (d *DB) Search(ctx context.Context, q core.Query, limit int) ([]core.Result, error) {
	var deposits []heat.Deposit
	for _, p := range d.passes {
		cands, err := p.Deposits(ctx, q, d.recallK)
		if err != nil {
			return nil, fmt.Errorf("deposits %s: %w", p.Name(), err)
		}
		if len(cands) == 0 {
			continue
		}
		raw := make([]float64, len(cands))
		for i, c := range cands {
			raw[i] = float64(c.Score)
		}
		norm := d.norm.Normalize(raw)
		w := p.Weight()
		for i, c := range cands {
			deposits = append(deposits, heat.Deposit{
				DocID:  c.DocID,
				Span:   c.Span,
				Weight: w * norm[i],
			})
		}
	}

	peaks := heat.Accumulate(deposits, d.peakFraction)
	results := make([]core.Result, 0, len(peaks))
	for _, pk := range peaks {
		results = append(results, core.Result{
			DocID:   pk.DocID,
			Span:    pk.Span,
			Score:   float32(pk.Score),
			Snippet: d.snippet(pk.DocID, pk.Span),
		})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (d *DB) snippet(docID string, span core.Span) string {
	text, ok := d.docText[docID]
	if !ok || span.Start < 0 || span.End > len(text) || span.Start >= span.End {
		return ""
	}
	s := text[span.Start:span.End]
	const max = 240
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
