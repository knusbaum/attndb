package core

import "context"

// SingleVectorEncoder produces one dense vector per text. Used by single-vector
// passes (paragraph-level, document-level, ...).
type SingleVectorEncoder interface {
	// Dim is the output dimensionality.
	Dim() int
	// EncodeDocs batch-encodes document chunk texts.
	EncodeDocs(ctx context.Context, texts []string) ([][]float32, error)
	// EncodeQuery encodes a query string.
	EncodeQuery(ctx context.Context, query string) ([]float32, error)
}

// MultiVectorEncoder produces per-token vectors (ColBERT-style). Used by
// per-token passes that score with MaxSim.
type MultiVectorEncoder interface {
	Dim() int
	// EncodeDocs returns one TokenVecs per input text; token Offsets are byte
	// ranges within that text.
	EncodeDocs(ctx context.Context, texts []string) ([]TokenVecs, error)
	EncodeQuery(ctx context.Context, query string) (TokenVecs, error)
}
