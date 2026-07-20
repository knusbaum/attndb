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
	sv, err := onnx.NewSingleVector(modelDir+"/single", provider, false) // query session: default allocator
	if err != nil {
		return nil, nil, err
	}
	return cb, sv, nil
}

// onnxSingleEncoder builds a standalone single-vector encoder plus a close func
// that destroys its ONNX session and returns the native memory to the OS. Used
// for the daemon's ephemeral per-reconcile ingest session — envAlloc=true so its
// large buffers go through the mmap allocator and munmap back to the OS on Close.
func onnxSingleEncoder(modelDir, provider string) (core.SingleVectorEncoder, func() error, error) {
	sv, err := onnx.NewSingleVector(modelDir+"/single", provider, true)
	if err != nil {
		return nil, nil, err
	}
	return sv, sv.Close, nil
}
