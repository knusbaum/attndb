package store

import (
	"context"
	"testing"
)

func TestMemoryDelete(t *testing.T) {
	m := NewMemory("t")
	ctx := context.Background()
	recs := []Record{
		{ID: "a:doc1:0-1", DocID: "doc1", Vector: []float32{1, 0}, Payload: map[string]any{"doc_id": "doc1"}},
		{ID: "a:doc1:1-2", DocID: "doc1", Vector: []float32{0, 1}, Payload: map[string]any{"doc_id": "doc1"}},
		{ID: "a:doc2:0-1", DocID: "doc2", Vector: []float32{1, 1}, Payload: map[string]any{"doc_id": "doc2"}},
	}
	if err := m.Upsert(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "doc1"); err != nil {
		t.Fatal(err)
	}
	// doc1's two records gone, doc2 untouched.
	got, _ := m.SearchSingle(ctx, []float32{1, 0}, 10, nil)
	if len(got) != 1 {
		t.Fatalf("after delete: want 1 record, got %d", len(got))
	}
	if got[0].Rec.DocID != "doc2" {
		t.Fatalf("wrong survivor: %s", got[0].Rec.DocID)
	}
	// Deleting a doc with no records is a no-op, not an error.
	if err := m.Delete(ctx, "nonexistent"); err != nil {
		t.Fatalf("delete missing doc: %v", err)
	}
}

func TestMemoryStamps(t *testing.T) {
	m := NewMemory("t")
	ctx := context.Background()
	m.Upsert(ctx, []Record{
		{ID: "a:doc1:0-1", DocID: "doc1", Vector: []float32{1}, Payload: map[string]any{"doc_id": "doc1", "mtime": int64(111), "sha": "aaa"}},
		{ID: "a:doc1:1-2", DocID: "doc1", Vector: []float32{1}, Payload: map[string]any{"doc_id": "doc1", "mtime": int64(111), "sha": "aaa"}},
		{ID: "a:doc2:0-1", DocID: "doc2", Vector: []float32{1}, Payload: map[string]any{"doc_id": "doc2", "mtime": int64(222), "sha": "bbb"}},
	})
	st, err := m.Stamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 {
		t.Fatalf("want 2 doc stamps, got %d", len(st))
	}
	if st["doc1"] != (DocStamp{MTime: 111, SHA: "aaa"}) || st["doc2"] != (DocStamp{MTime: 222, SHA: "bbb"}) {
		t.Fatalf("wrong stamps: %+v", st)
	}
}

func TestQdrantStamps(t *testing.T) {
	ctx := context.Background()
	q, err := NewQdrant("localhost", 6334, "attndb_stamps_test")
	if err != nil {
		t.Skipf("no qdrant: %v", err)
	}
	if ok, err := q.client.CollectionExists(ctx, q.name); err != nil {
		t.Skipf("qdrant unreachable: %v", err)
	} else if ok {
		_ = q.client.DeleteCollection(ctx, q.name)
	}
	t.Cleanup(func() {
		_ = q.client.DeleteCollection(ctx, q.name)
		_ = q.Close()
	})
	// empty collection (before create) → empty stamps, no error
	if st, err := q.Stamps(ctx); err != nil || len(st) != 0 {
		t.Fatalf("stamps on absent collection: st=%v err=%v", st, err)
	}
	if err := q.Upsert(ctx, []Record{
		{ID: "a:doc1:0-1", DocID: "doc1", Vector: []float32{1, 0}, Payload: map[string]any{"doc_id": "doc1", "mtime": int64(111), "sha": "aaa"}},
		{ID: "a:doc2:0-1", DocID: "doc2", Vector: []float32{0, 1}, Payload: map[string]any{"doc_id": "doc2", "mtime": int64(222), "sha": "bbb"}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Upsert defaults to async; wait for visibility via a short scroll retry.
	var st map[string]DocStamp
	for i := 0; i < 20; i++ {
		st, err = q.Stamps(ctx)
		if err != nil {
			t.Fatalf("stamps: %v", err)
		}
		if len(st) == 2 {
			break
		}
	}
	if st["doc1"] != (DocStamp{MTime: 111, SHA: "aaa"}) || st["doc2"] != (DocStamp{MTime: 222, SHA: "bbb"}) {
		t.Fatalf("wrong stamps: %+v", st)
	}
}

// TestQdrantDelete exercises the real delete-by-doc_id filter path. It is an
// integration test: it skips unless a Qdrant is reachable at localhost:6334.
func TestQdrantDelete(t *testing.T) {
	ctx := context.Background()
	q, err := NewQdrant("localhost", 6334, "attndb_delete_test")
	if err != nil {
		t.Skipf("no qdrant: %v", err)
	}
	if ok, err := q.client.CollectionExists(ctx, q.name); err != nil {
		t.Skipf("qdrant unreachable: %v", err)
	} else if ok {
		_ = q.client.DeleteCollection(ctx, q.name)
	}
	// Delete the collection first, then close the client — closing empties the
	// connection pool, so any client call after Close panics.
	t.Cleanup(func() {
		_ = q.client.DeleteCollection(ctx, q.name)
		_ = q.Close()
	})

	recs := []Record{
		{ID: "a:doc1:0-1", DocID: "doc1", Vector: []float32{1, 0, 0}, Payload: map[string]any{"doc_id": "doc1"}},
		{ID: "a:doc1:1-2", DocID: "doc1", Vector: []float32{0, 1, 0}, Payload: map[string]any{"doc_id": "doc1"}},
		{ID: "a:doc2:0-1", DocID: "doc2", Vector: []float32{0, 0, 1}, Payload: map[string]any{"doc_id": "doc2"}},
	}
	if err := q.Upsert(ctx, recs); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := q.Delete(ctx, "doc1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := q.SearchSingle(ctx, []float32{1, 0, 0}, 10, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, s := range got {
		if s.Rec.DocID == "doc1" {
			t.Fatalf("doc1 record survived delete: span %v", s.Rec.Span)
		}
	}
	if len(got) != 1 || got[0].Rec.DocID != "doc2" {
		t.Fatalf("want only doc2 remaining, got %d records", len(got))
	}
}
