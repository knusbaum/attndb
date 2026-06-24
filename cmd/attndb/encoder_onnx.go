//go:build onnx

package main

import (
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/encode/onnx"
)

// onnxEncoders builds the real encoders: GTE-ModernColBERT for per-token passes
// and a dedicated gte-modernbert-base single-vector embedder (in <modelDir>/single)
// for the pooled passes. provider is "cpu" or "coreml".
func onnxEncoders(modelDir, provider string) (core.MultiVectorEncoder, core.SingleVectorEncoder, error) {
	cb, err := onnx.NewColBERT(modelDir, provider)
	if err != nil {
		return nil, nil, err
	}
	sv, err := onnx.NewSingleVector(modelDir+"/single", provider)
	if err != nil {
		return nil, nil, err
	}
	return cb, sv, nil
}
