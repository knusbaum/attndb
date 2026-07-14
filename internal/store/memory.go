package store

import (
	"context"
	"sort"
	"sync"

	"github.com/kjn/attndb/internal/core"
)

// Memory is an in-memory Pool using brute-force search. It exists to exercise
// the full pipeline without a running Qdrant; QdrantPool will implement the same
// interface for real workloads.
type Memory struct {
	name string
	mu   sync.RWMutex
	recs map[string]Record
}

// NewMemory returns an empty in-memory pool.
func NewMemory(name string) *Memory {
	return &Memory{name: name, recs: map[string]Record{}}
}

// matchesFilters reports whether payload satisfies all equality filters.
func matchesFilters(payload map[string]any, filters map[string]any) bool {
	for k, v := range filters {
		if payload[k] != v {
			return false
		}
	}
	return true
}

func (m *Memory) Name() string { return m.name }

func (m *Memory) Upsert(_ context.Context, recs []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range recs {
		m.recs[r.ID] = r
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, docID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.recs {
		if r.DocID == docID {
			delete(m.recs, id)
		}
	}
	return nil
}

func (m *Memory) Stamps(_ context.Context) (map[string]DocStamp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]DocStamp)
	for _, r := range m.recs {
		st := out[r.DocID]
		if v, ok := r.Payload["mtime"].(int64); ok {
			st.MTime = v
		}
		if v, ok := r.Payload["sha"].(string); ok {
			st.SHA = v
		}
		out[r.DocID] = st
	}
	return out, nil
}

func (m *Memory) SearchSingle(_ context.Context, vec []float32, k int, filters map[string]any) ([]Scored, error) {
	return m.topK(k, func(r Record) (float32, bool) {
		if r.Vector == nil {
			return 0, false
		}
		return core.Cosine(vec, r.Vector), true
	}, filters), nil
}

func (m *Memory) SearchMulti(_ context.Context, q core.TokenVecs, k int, filters map[string]any) ([]Scored, error) {
	return m.topK(k, func(r Record) (float32, bool) {
		if r.Tokens.Len() == 0 {
			return 0, false
		}
		s, _ := core.MaxSim(q, r.Tokens)
		return s, true
	}, filters), nil
}

func (m *Memory) topK(k int, score func(Record) (float32, bool), filters map[string]any) []Scored {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var scored []Scored
	for _, r := range m.recs {
		if !matchesFilters(r.Payload, filters) {
			continue
		}
		s, ok := score(r)
		if !ok {
			continue
		}
		scored = append(scored, Scored{Rec: r, Score: s})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	return scored
}
