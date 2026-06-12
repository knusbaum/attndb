package core

import "math"

// Normalize scales v to unit L2 norm in place and returns it. A zero vector is
// left unchanged.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// Dot returns the dot product of two equal-length vectors.
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// Cosine returns the cosine similarity of a and b. Inputs are assumed (but not
// required) to be L2-normalized; it normalizes defensively.
func Cosine(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// MaxSim computes the ColBERT late-interaction score between a query and a
// document, both as token vectors: for each query token, take its highest
// similarity to any document token, and sum. It also returns, for each query
// token, the index of the best-matching document token (for span refinement).
func MaxSim(q, d TokenVecs) (score float32, matched []int) {
	matched = make([]int, len(q.Vecs))
	for i, qv := range q.Vecs {
		best := float32(-1)
		bestJ := -1
		for j, dv := range d.Vecs {
			s := Cosine(qv, dv)
			if s > best {
				best, bestJ = s, j
			}
		}
		if bestJ >= 0 {
			score += best
		}
		matched[i] = bestJ
	}
	return score, matched
}
