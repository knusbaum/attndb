package core

import (
	"math"
	"testing"
)

func TestCosine(t *testing.T) {
	cases := []struct {
		a, b []float32
		want float32
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1},
		{[]float32{1, 0}, []float32{0, 1}, 0},
		{[]float32{1, 0}, []float32{-1, 0}, -1},
		{[]float32{0, 0}, []float32{1, 0}, 0}, // zero vector
	}
	for _, c := range cases {
		if got := Cosine(c.a, c.b); math.Abs(float64(got-c.want)) > 1e-6 {
			t.Errorf("Cosine(%v,%v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	v := Normalize([]float32{3, 4})
	if math.Abs(float64(v[0]-0.6)) > 1e-6 || math.Abs(float64(v[1]-0.8)) > 1e-6 {
		t.Fatalf("Normalize([3,4])=%v want [0.6 0.8]", v)
	}
	z := Normalize([]float32{0, 0})
	if z[0] != 0 || z[1] != 0 {
		t.Fatalf("Normalize zero vector changed it: %v", z)
	}
}

func TestMaxSim(t *testing.T) {
	q := TokenVecs{Vecs: [][]float32{{1, 0}, {0, 1}}}
	d := TokenVecs{Vecs: [][]float32{{1, 0}, {0, 1}, {1, 0}}}
	score, matched := MaxSim(q, d)
	if math.Abs(float64(score-2)) > 1e-6 {
		t.Fatalf("MaxSim score=%v want 2", score)
	}
	if len(matched) != 2 || matched[0] != 0 || matched[1] != 1 {
		t.Fatalf("MaxSim matched=%v want [0 1]", matched)
	}
}
