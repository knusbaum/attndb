// Package heat accumulates each pass's scored intervals onto a per-document
// byte-axis "heatmap" and extracts the peaks. It replaces the earlier
// scope/localizing + RRF scheme: width is handled continuously. A deposit adds
// its weight uniformly across its span (flat), so a broad match acts as a low
// uniform prior over its region while precise matches stack on top into peaks —
// and the peaks, where independent passes agree, are the results.
//
// (Flat — not weight/length — because pure length-division rewards shortness
// without limit, so degenerate tiny spans like a "## Status" heading explode to
// the top. Stacking gives "precise beats diffuse" without that pathology.)
package heat

import (
	"sort"

	"github.com/kjn/attndb/internal/core"
)

// Normalizer maps a pass's raw likeness scores onto a common 0..1 scale so that
// heterogeneous passes (cosine vs MaxSim) contribute comparably. It is applied
// per pass, per query.
type Normalizer interface {
	Normalize(scores []float64) []float64
}

// MinMax stretches scores to [0,1] via (x-min)/(max-min). This both bounds the
// range and restores contrast for passes whose scores bunch up near the top
// (notably MaxSim, a sum of per-token maxima). If every score is equal the pass
// can't discriminate, so all weights become 1.
type MinMax struct{}

func (MinMax) Normalize(scores []float64) []float64 {
	out := make([]float64, len(scores))
	if len(scores) == 0 {
		return out
	}
	lo, hi := scores[0], scores[0]
	for _, s := range scores {
		lo = min(lo, s)
		hi = max(hi, s)
	}
	rng := hi - lo
	for i, s := range scores {
		if rng == 0 {
			out[i] = 1
		} else {
			out[i] = (s - lo) / rng
		}
	}
	return out
}

// Affine applies a fixed scale (s-Lo)/(Hi-Lo), clamped to [0,1]. Unlike MinMax it
// is NOT per-query: Lo/Hi are calibrated once (the pass's noise floor and
// strong-match level), so a query with no real match scores near 0 instead of
// being stretched to 1. This is what gives the system a "no confident match"
// signal.
type Affine struct{ Lo, Hi float64 }

func (a Affine) Normalize(scores []float64) []float64 {
	out := make([]float64, len(scores))
	rng := a.Hi - a.Lo
	for i, s := range scores {
		if rng <= 0 {
			continue // degenerate calibration → contribute nothing
		}
		v := (s - a.Lo) / rng
		switch {
		case v < 0:
			v = 0
		case v > 1:
			v = 1
		}
		out[i] = v
	}
	return out
}

// Rank ignores magnitude and uses position only (best=1, descending). The robust
// fallback when a pass's score distribution is pathological.
type Rank struct{}

func (Rank) Normalize(scores []float64) []float64 {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	out := make([]float64, len(scores))
	n := float64(len(scores))
	for rank, i := range idx {
		out[i] = (n - float64(rank)) / n
	}
	return out
}

// Deposit is a normalized scored interval: Weight is the height it adds at every
// byte position across Span (flat). The heatmap value at a position is the sum of
// the weights of all deposits covering it.
type Deposit struct {
	DocID  string
	Span   core.Span
	Weight float64
}

// Peak is an extracted result region with its peak density as the score.
type Peak struct {
	DocID string
	Span  core.Span
	Score float64
}

// Accumulate builds the per-document heatmap from deposits and returns peaks
// sorted by density (highest first). peakFraction (0..1) controls how far a peak
// region grows around its maximum: bytes are included while their density stays
// at least peakFraction of the peak.
func Accumulate(deps []Deposit, peakFraction float64) []Peak {
	if peakFraction <= 0 || peakFraction > 1 {
		peakFraction = 0.5
	}
	byDoc := map[string][]Deposit{}
	for _, d := range deps {
		if d.Span.Len() > 0 {
			byDoc[d.DocID] = append(byDoc[d.DocID], d)
		}
	}
	var peaks []Peak
	for doc, ds := range byDoc {
		for _, seg := range extractPeaks(densitySegments(ds), peakFraction) {
			peaks = append(peaks, Peak{DocID: doc, Span: seg.span, Score: seg.density})
		}
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].Score > peaks[j].Score })
	return peaks
}

type segment struct {
	span    core.Span
	density float64
}

// densitySegments sweeps the deposit endpoints and returns the piecewise-constant
// accumulated density along the byte axis.
func densitySegments(ds []Deposit) []segment {
	type event struct {
		pos   int
		delta float64
	}
	events := make([]event, 0, 2*len(ds))
	for _, d := range ds {
		// flat: the deposit adds Weight at every covered position
		events = append(events, event{d.Span.Start, d.Weight}, event{d.Span.End, -d.Weight})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	var segs []segment
	var cur float64
	for i := 0; i < len(events); {
		pos := events[i].pos
		// emit the segment ending at this position
		if i > 0 && cur > 0 && events[i-1].pos < pos {
			segs = append(segs, segment{core.Span{Start: events[i-1].pos, End: pos}, cur})
		}
		for i < len(events) && events[i].pos == pos {
			cur += events[i].delta
			i++
		}
	}
	return segs
}

// extractPeaks finds result regions by non-max suppression: repeatedly take the
// tallest not-yet-claimed segment, grow a region around it while height stays at
// least peakFraction of that local maximum, claim it, and continue. This yields
// multiple non-overlapping peaks per document, ranked by height.
func extractPeaks(segs []segment, peakFraction float64) []segment {
	order := make([]int, len(segs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return segs[order[a]].density > segs[order[b]].density })

	claimed := make([]bool, len(segs))
	var peaks []segment
	for _, idx := range order {
		if claimed[idx] {
			continue
		}
		maxD := segs[idx].density
		thr := peakFraction * maxD
		lo, hi := idx, idx
		for lo-1 >= 0 && !claimed[lo-1] &&
			segs[lo-1].span.End == segs[lo].span.Start && segs[lo-1].density >= thr {
			lo--
		}
		for hi+1 < len(segs) && !claimed[hi+1] &&
			segs[hi].span.End == segs[hi+1].span.Start && segs[hi+1].density >= thr {
			hi++
		}
		for k := lo; k <= hi; k++ {
			claimed[k] = true
		}
		peaks = append(peaks, segment{
			span:    core.Span{Start: segs[lo].span.Start, End: segs[hi].span.End},
			density: maxD,
		})
	}
	return peaks
}
