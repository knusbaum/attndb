// Package db composes passes into a searchable database. Search collects each
// pass's scored intervals, normalizes them per pass, deposits them (weighted) on
// a per-document heatmap, and returns the accumulated peaks.
package db

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/heat"
	"github.com/kjn/attndb/internal/pass"
)

// DB is a multi-pass database.
type DB struct {
	passes       []pass.Pass
	norm         heat.Normalizer            // default normalizer (per-query min-max)
	passNorm     map[string]heat.Normalizer // per-pass override (calibrated affine)
	recallK      int
	peakFraction float64
}

// PassCalib is a pass's calibrated score range: Lo is the noise floor, Hi a
// strong-match level. Raw scores are mapped (s-Lo)/(Hi-Lo), clamped to [0,1].
type PassCalib struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// Calibration maps pass name -> calibrated range.
type Calibration map[string]PassCalib

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
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// SetCalibration installs calibrated affine normalizers per pass (replacing the
// default per-query min-max for those passes).
func (d *DB) SetCalibration(c Calibration) {
	d.passNorm = make(map[string]heat.Normalizer, len(c))
	for name, pc := range c {
		d.passNorm[name] = heat.Affine{Lo: pc.Lo, Hi: pc.Hi}
	}
}

func (d *DB) normalizerFor(pass string) heat.Normalizer {
	if n, ok := d.passNorm[pass]; ok {
		return n
	}
	return d.norm
}

// Ingest runs every pass over the documents.
func (d *DB) Ingest(ctx context.Context, docs []core.Document) error {
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
		norm := d.normalizerFor(p.Name()).Normalize(raw)
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
			DocID: pk.DocID,
			Span:  pk.Span,
			Score: float32(pk.Score),
		})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// Calibrate estimates a per-pass score range from sample (pseudo-)queries:
// HI = high percentile of the best score per query (strong matches), LO = median
// of all candidate scores (the middling/weak level). Because in-corpus matches
// cluster near HI while unrelated queries only reach the middling level, the
// affine map separates them. Run once at ingest.
func (d *DB) Calibrate(ctx context.Context, samples []string) (Calibration, error) {
	all := map[string][]float64{}
	tops := map[string][]float64{}
	for _, q := range samples {
		for _, p := range d.passes {
			cands, err := p.Deposits(ctx, core.Query{Text: q}, d.recallK)
			if err != nil {
				return nil, fmt.Errorf("calibrate %s: %w", p.Name(), err)
			}
			if len(cands) == 0 {
				continue
			}
			top := math.Inf(-1)
			for _, c := range cands {
				s := float64(c.Score)
				all[p.Name()] = append(all[p.Name()], s)
				if s > top {
					top = s
				}
			}
			tops[p.Name()] = append(tops[p.Name()], top)
		}
	}
	out := Calibration{}
	for _, p := range d.passes {
		a, t := all[p.Name()], tops[p.Name()]
		if len(a) == 0 || len(t) == 0 {
			continue
		}
		lo, hi := percentile(a, 0.5), percentile(t, 0.9)
		if hi <= lo {
			hi = lo + 1e-6
		}
		out[p.Name()] = PassCalib{Lo: lo, Hi: hi}
	}
	return out, nil
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[int(p*float64(len(s)-1))]
}
