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

// DocStamp is a document's change-detection stamp, read back from stored
// payloads. Zero values mean the field was absent (e.g. indexed before stamping
// existed) — the reconciler treats that as "changed" and re-ingests.
type DocStamp struct {
	MTime int64
	SHA   string
}

// Pool is a collection of records supporting ANN (single-vector) and MaxSim
// (multivector) search. Searches take equality filters over payload fields
// (e.g. {"status": "approved"}); each backend translates them natively (a
// predicate in Memory, field conditions in Qdrant). An empty/nil filter matches
// all.
type Pool interface {
	Name() string
	Upsert(ctx context.Context, recs []Record) error
	// Delete removes every record belonging to docID. It is a no-op if the
	// document has no records. Used to re-index a changed file (delete then
	// upsert) and to drop a deleted one.
	Delete(ctx context.Context, docID string) error
	// Stamps returns, per doc_id, the change-detection stamp recorded in this
	// pool's payloads. On a one-point-per-doc pool (the whole-doc pass) it is the
	// index-of-record the reconciler diffs the filesystem against.
	Stamps(ctx context.Context) (map[string]DocStamp, error)
	// SearchSingle returns the top-k records by cosine similarity to vec.
	SearchSingle(ctx context.Context, vec []float32, k int, filters map[string]any) ([]Scored, error)
	// SearchMulti returns the top-k records by MaxSim to the query token vectors.
	SearchMulti(ctx context.Context, q core.TokenVecs, k int, filters map[string]any) ([]Scored, error)
}
