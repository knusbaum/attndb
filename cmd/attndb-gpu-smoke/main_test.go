//go:build onnx

package main

import (
	"math"
	"testing"
)

func TestVectorValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    []float32
		dim  int
		ok   bool
	}{
		{"valid", []float32{0.6, 0.8}, 2, true},
		{"wrong dimension", []float32{1}, 2, false},
		{"empty", nil, 2, false},
		{"zero", []float32{0, 0}, 2, false},
		{"nan", []float32{1, float32(math.NaN())}, 2, false},
		{"infinite", []float32{1, float32(math.Inf(1))}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := vector(tc.v, tc.dim); (err == nil) != tc.ok {
				t.Fatalf("vector validation error = %v, want valid=%v", err, tc.ok)
			}
		})
	}
}
