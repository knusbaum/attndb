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
	deposits, _, err := d.gather(ctx, q)
	if err != nil {
		return nil, err
	}
	return d.fuse(deposits, limit), nil
}

// gather runs every pass, normalizes its candidates per pass, and turns them
// into weighted heatmap deposits. It is the single source of truth shared by
// Search (which discards the per-pass detail) and Explain (which surfaces it).
func (d *DB) gather(ctx context.Context, q core.Query) ([]heat.Deposit, []PassExplain, error) {
	var deposits []heat.Deposit
	ex := make([]PassExplain, 0, len(d.passes))
	for _, p := range d.passes {
		cands, err := p.Deposits(ctx, q, d.recallK)
		if err != nil {
			return nil, nil, fmt.Errorf("deposits %s: %w", p.Name(), err)
		}
		w := p.Weight()
		pe := PassExplain{Pass: p.Name(), Weight: w}
		if len(cands) > 0 {
			raw := make([]float64, len(cands))
			for i, c := range cands {
				raw[i] = float64(c.Score)
			}
			norm := d.normalizerFor(p.Name()).Normalize(raw)
			for i, c := range cands {
				contrib := w * norm[i]
				deposits = append(deposits, heat.Deposit{DocID: c.DocID, Span: c.Span, Weight: contrib})
				pe.Cands = append(pe.Cands, ExplainCand{
					DocID: c.DocID, Span: c.Span, Raw: raw[i], Norm: norm[i], Contribution: contrib,
				})
			}
		}
		ex = append(ex, pe)
	}
	return deposits, ex, nil
}

// fuse accumulates deposits into per-document peaks and returns the top results.
func (d *DB) fuse(deposits []heat.Deposit, limit int) []core.Result {
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
	return results
}

// PassExplain is one pass's contribution to a query: its weight and the
// candidates it recalled (with raw, normalized, and weighted scores).
type PassExplain struct {
	Pass   string
	Weight float64
	Cands  []ExplainCand
}

// ExplainCand is a single recalled candidate with its score at each stage.
type ExplainCand struct {
	DocID        string
	Span         core.Span
	Raw          float64 // pass's native likeness (cosine / MaxSim)
	Norm         float64 // after the pass normalizer (0..1)
	Contribution float64 // Weight * Norm — what actually lands on the heatmap
}

// Explanation is the per-pass breakdown plus the fused results for one query.
type Explanation struct {
	Passes []PassExplain
	Peaks  []core.Result
}

// Explain runs a query like Search but returns the per-pass candidate breakdown
// alongside the fused results, so retrieval can be debugged and tuned.
func (d *DB) Explain(ctx context.Context, q core.Query, limit int) (*Explanation, error) {
	deposits, ex, err := d.gather(ctx, q)
	if err != nil {
		return nil, err
	}
	return &Explanation{Passes: ex, Peaks: d.fuse(deposits, limit)}, nil
}

// Calibrate estimates a per-pass score range from two query sets that anchor the
// ends independently: HI = high percentile of the best score per POSITIVE query
// (in-corpus text — what a strong match looks like), LO = high percentile of the
// best score per NEGATIVE query (random unrelated queries — the noise ceiling).
// Calibrating LO from unrelated queries instead of from the positives' own
// candidates avoids an artificially narrow range: if every probe is exact corpus
// text, even "middling" candidates score high and the floor lands above what real
// paraphrased queries reach, zeroing the pass. Run once at ingest.
func (d *DB) Calibrate(ctx context.Context, positives, negatives []string) (Calibration, error) {
	// tops[set][pass] = best candidate score per query in that set.
	collect := func(samples []string) (map[string][]float64, error) {
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
					if s := float64(c.Score); s > top {
						top = s
					}
				}
				tops[p.Name()] = append(tops[p.Name()], top)
			}
		}
		return tops, nil
	}
	posTops, err := collect(positives)
	if err != nil {
		return nil, err
	}
	negTops, err := collect(negatives)
	if err != nil {
		return nil, err
	}
	out := Calibration{}
	for _, p := range d.passes {
		pos := posTops[p.Name()]
		if len(pos) == 0 {
			continue
		}
		hi := percentile(pos, 0.9)               // strong-match level (in-corpus text)
		lo := percentile(negTops[p.Name()], 0.9) // noise ceiling (unrelated queries); 0 if none
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
