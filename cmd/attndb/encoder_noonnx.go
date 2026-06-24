//go:build !onnx

package main

import (
	"fmt"

	"github.com/kjn/attndb/internal/core"
)

// onnxEncoders is unavailable in the default build. Rebuild with the onnx tag:
//
//	CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx ./cmd/attndb
func onnxEncoders(modelDir, provider string) (core.MultiVectorEncoder, core.SingleVectorEncoder, error) {
	return nil, nil, fmt.Errorf("onnx encoder not compiled in; rebuild with -tags onnx (and CGO_LDFLAGS=-L./libs)")
}
