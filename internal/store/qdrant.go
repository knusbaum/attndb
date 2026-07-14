package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"sync"

	"github.com/kjn/attndb/internal/core"
	"github.com/qdrant/go-client/qdrant"
)

// Qdrant is a Pool backed by a Qdrant collection. Single-vector pools use a
// cosine ANN collection; per-token pools use a multivector collection with the
// MaxSim comparator. The collection is created lazily on first Upsert, inferring
// single-vs-multi and dimensionality from the first record.
//
// NOTE: SearchMulti returns the matched chunk's span and MaxSim score but not its
// token vectors, so the per-token pass deposits the whole chunk span rather than
// a refined sub-span (it falls back gracefully). Token-level sub-span refinement
// over Qdrant — storing per-token offsets in payload and reading vectors back —
// is a tracked follow-up.
type Qdrant struct {
	client *qdrant.Client
	name   string

	mu      sync.Mutex
	created bool
}

// NewQdrant connects to a Qdrant server over gRPC and returns a Pool for the
// named collection.
func NewQdrant(host string, port int, name string) (*Qdrant, error) {
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   host,
		Port:                   port,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant connect: %w", err)
	}
	return &Qdrant{client: client, name: name}, nil
}

func (q *Qdrant) Name() string { return q.name }

// Close releases the underlying gRPC connection.
func (q *Qdrant) Close() error { return q.client.Close() }

func (q *Qdrant) ensure(ctx context.Context, rec Record) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.created {
		return nil
	}
	multi := rec.Tokens.Len() > 0
	var dim int
	switch {
	case multi:
		dim = len(rec.Tokens.Vecs[0])
	case rec.Vector != nil:
		dim = len(rec.Vector)
	default:
		return fmt.Errorf("qdrant %s: record has neither vector nor tokens", q.name)
	}

	exists, err := q.client.CollectionExists(ctx, q.name)
	if err != nil {
		return fmt.Errorf("qdrant exists %s: %w", q.name, err)
	}
	if !exists {
		params := &qdrant.VectorParams{Size: uint64(dim), Distance: qdrant.Distance_Cosine}
		if multi {
			params.MultivectorConfig = &qdrant.MultiVectorConfig{
				Comparator: qdrant.MultiVectorComparator_MaxSim,
			}
		}
		if err := q.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: q.name,
			VectorsConfig:  qdrant.NewVectorsConfig(params),
		}); err != nil {
			return fmt.Errorf("qdrant create %s: %w", q.name, err)
		}
	}
	q.created = true
	return nil
}

func (q *Qdrant) Upsert(ctx context.Context, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	if err := q.ensure(ctx, recs[0]); err != nil {
		return err
	}
	points := make([]*qdrant.PointStruct, 0, len(recs))
	for _, r := range recs {
		var vecs *qdrant.Vectors
		if r.Tokens.Len() > 0 {
			vecs = qdrant.NewVectorsMulti(r.Tokens.Vecs)
		} else {
			vecs = qdrant.NewVectorsDense(r.Vector)
		}
		pl := make(map[string]any, len(r.Payload)+2)
		for k, v := range r.Payload {
			pl[k] = v
		}
		pl["span_start"] = int64(r.Span.Start)
		pl["span_end"] = int64(r.Span.End)
		points = append(points, &qdrant.PointStruct{
			Id:      qdrant.NewIDNum(hashID(r.ID)),
			Vectors: vecs,
			Payload: qdrant.NewValueMap(pl),
		})
	}
	if _, err := q.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: q.name,
		Points:         points,
	}); err != nil {
		return fmt.Errorf("qdrant upsert %s: %w", q.name, err)
	}
	return nil
}

// Delete removes every point whose doc_id payload equals docID. It is a no-op
// if the collection does not exist yet (nothing has been ingested).
func (q *Qdrant) Delete(ctx context.Context, docID string) error {
	exists, err := q.client.CollectionExists(ctx, q.name)
	if err != nil {
		return fmt.Errorf("qdrant exists %s: %w", q.name, err)
	}
	if !exists {
		return nil
	}
	sel := qdrant.NewPointsSelectorFilter(toFilter(map[string]any{"doc_id": docID}))
	wait := true // block until applied so a following re-ingest/search sees the removal
	if _, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.name,
		Points:         sel,
		Wait:           &wait,
	}); err != nil {
		return fmt.Errorf("qdrant delete %s (doc_id=%s): %w", q.name, docID, err)
	}
	return nil
}

// Stamps scrolls the whole collection and returns each doc_id's mtime/sha. On
// the whole-doc pass (one point per doc) this is the index-of-record; on a
// multi-point pass it dedupes per doc_id (all a doc's points share the stamp).
func (q *Qdrant) Stamps(ctx context.Context) (map[string]DocStamp, error) {
	out := make(map[string]DocStamp)
	exists, err := q.client.CollectionExists(ctx, q.name)
	if err != nil {
		return nil, fmt.Errorf("qdrant exists %s: %w", q.name, err)
	}
	if !exists {
		return out, nil
	}
	limit := uint32(1024)
	it := q.client.ScrollAll(ctx, &qdrant.ScrollPoints{
		CollectionName: q.name,
		Limit:          &limit,
		WithPayload:    qdrant.NewWithPayload(true),
	})
	for {
		points, err := it.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("qdrant scroll %s: %w", q.name, err)
		}
		for _, p := range points {
			doc := payloadString(p.Payload, "doc_id")
			if doc == "" {
				continue
			}
			out[doc] = DocStamp{
				MTime: payloadInt(p.Payload, "mtime"),
				SHA:   payloadString(p.Payload, "sha"),
			}
		}
	}
	return out, nil
}

func (q *Qdrant) SearchSingle(ctx context.Context, vec []float32, k int, filters map[string]any) ([]Scored, error) {
	return q.query(ctx, qdrant.NewQueryDense(vec), k, filters)
}

func (q *Qdrant) SearchMulti(ctx context.Context, qv core.TokenVecs, k int, filters map[string]any) ([]Scored, error) {
	return q.query(ctx, qdrant.NewQueryMulti(qv.Vecs), k, filters)
}

func (q *Qdrant) query(ctx context.Context, query *qdrant.Query, k int, filters map[string]any) ([]Scored, error) {
	limit := uint64(k)
	points, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.name,
		Query:          query,
		Limit:          &limit,
		Filter:         toFilter(filters),
		WithPayload:    qdrant.NewWithPayload(true),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant query %s: %w", q.name, err)
	}
	out := make([]Scored, 0, len(points))
	for _, p := range points {
		out = append(out, Scored{
			Rec: Record{
				DocID: payloadString(p.Payload, "doc_id"),
				Span: core.Span{
					Start: int(payloadInt(p.Payload, "span_start")),
					End:   int(payloadInt(p.Payload, "span_end")),
				},
				Payload: flattenPayload(p.Payload),
			},
			Score: p.Score,
		})
	}
	return out, nil
}

// toFilter translates equality filters into a Qdrant must-match filter.
func toFilter(filters map[string]any) *qdrant.Filter {
	if len(filters) == 0 {
		return nil
	}
	conds := make([]*qdrant.Condition, 0, len(filters))
	for k, v := range filters {
		switch x := v.(type) {
		case string:
			conds = append(conds, qdrant.NewMatchKeyword(k, x))
		case bool:
			conds = append(conds, qdrant.NewMatchBool(k, x))
		case int:
			conds = append(conds, qdrant.NewMatchInt(k, int64(x)))
		case int64:
			conds = append(conds, qdrant.NewMatchInt(k, x))
		}
	}
	return &qdrant.Filter{Must: conds}
}

func payloadString(p map[string]*qdrant.Value, key string) string {
	if v, ok := p[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

func payloadInt(p map[string]*qdrant.Value, key string) int64 {
	if v, ok := p[key]; ok {
		return v.GetIntegerValue()
	}
	return 0
}

func flattenPayload(p map[string]*qdrant.Value) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		switch x := v.Kind.(type) {
		case *qdrant.Value_StringValue:
			out[k] = x.StringValue
		case *qdrant.Value_IntegerValue:
			out[k] = x.IntegerValue
		case *qdrant.Value_BoolValue:
			out[k] = x.BoolValue
		case *qdrant.Value_DoubleValue:
			out[k] = x.DoubleValue
		}
	}
	return out
}

func hashID(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
