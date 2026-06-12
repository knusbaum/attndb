// Package store abstracts vector storage behind a Pool interface so the backend
// (Qdrant for real, in-memory for tests/slices) is swappable. Each pass owns one
// Pool.
package store

import (
	"context"

	"github.com/kjn/attndb/internal/core"
)

// Record is a single stored unit: either a single vector or a multivector
// (token vectors), tagged with its document region and payload.
type Record struct {
	ID      string
	DocID   string
	Span    core.Span
	Vector  []float32      // set for single-vector pools
	Tokens  core.TokenVecs // set for multivector pools
	Payload map[string]any
}

// Scored is a record with a similarity score from a search.
type Scored struct {
	Rec   Record
	Score float32
}

// Pool is a collection of records supporting ANN (single-vector) and MaxSim
// (multivector) search. Searches take equality filters over payload fields
// (e.g. {"status": "approved"}); each backend translates them natively (a
// predicate in Memory, field conditions in Qdrant). An empty/nil filter matches
// all.
type Pool interface {
	Name() string
	Upsert(ctx context.Context, recs []Record) error
	// SearchSingle returns the top-k records by cosine similarity to vec.
	SearchSingle(ctx context.Context, vec []float32, k int, filters map[string]any) ([]Scored, error)
	// SearchMulti returns the top-k records by MaxSim to the query token vectors.
	SearchMulti(ctx context.Context, q core.TokenVecs, k int, filters map[string]any) ([]Scored, error)
}
