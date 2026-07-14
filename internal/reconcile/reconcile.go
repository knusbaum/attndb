// Package reconcile keeps a vector index in sync with a directory tree. It
// exposes two entry points over one diff-driven core:
//
//   - ReconcileAll — diff the whole tree against the index's stamps. Used at
//     startup, for the polling watcher, and as a crash-recovery backstop; it is
//     the only path that detects deletions of files missed while down.
//   - Reconcile(paths) — a low-latency granular update for a specific set of
//     changed paths, used by the FSEvents watcher.
//
// Every mutation is delete-then-ingest (point IDs are span-derived, so a bare
// re-ingest would orphan the previous version's spans). A single-writer lock
// serializes passes so ingests never interleave.
package reconcile

import (
	"context"
	"os"
	"sync"

	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/corpus"
	"github.com/kjn/attndb/internal/store"
)

// Store is the slice of the DB the reconciler needs. *db.DB satisfies it.
type Store interface {
	Ingest(ctx context.Context, docs []core.Document) error
	Delete(ctx context.Context, docIDs []string) error
	Stamps(ctx context.Context) (map[string]store.DocStamp, error)
}

// Reconciler diffs a directory tree against a Store and applies the changes.
type Reconciler struct {
	root  string
	db    Store
	mu    sync.Mutex                // single writer: serialize reconcile passes
	known map[string]store.DocStamp // docID -> last-applied stamp (in-memory cache)
}

// New returns a Reconciler for root backed by db. Call ReconcileAll once before
// serving to seed the in-memory stamp cache from the index.
func New(root string, db Store) *Reconciler {
	return &Reconciler{root: root, db: db, known: map[string]store.DocStamp{}}
}

// Count returns the number of documents currently tracked (indexed).
func (r *Reconciler) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.known)
}

// ReconcileAll diffs the whole tree against the index and applies adds, updates,
// and deletes. It returns the number of documents changed (added+updated+deleted).
func (r *Reconciler) ReconcileAll(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	docs, err := corpus.LoadDir(r.root)
	if err != nil {
		return 0, err
	}
	// Seed the known set from the index on the first pass so we don't trust a
	// cold cache (which would re-ingest everything). After that the cache is the
	// source of truth and the scroll is skipped.
	if r.known == nil || len(r.known) == 0 {
		if idx, err := r.db.Stamps(ctx); err == nil && len(idx) > 0 {
			r.known = idx
		}
	}

	onDisk := make(map[string]core.Document, len(docs))
	changed := 0
	for _, d := range docs {
		onDisk[d.ID] = d
		if sameContent(r.stampOf(d), r.known[d.ID]) {
			continue // unchanged
		}
		if err := r.applyUpsert(ctx, d); err != nil {
			return changed, err
		}
		changed++
	}
	// Deletions: in the index/cache but no longer on disk.
	for id := range r.known {
		if _, ok := onDisk[id]; ok {
			continue
		}
		if err := r.applyDelete(ctx, id); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// Reconcile applies a granular set of changed paths: gone → delete, changed →
// delete-then-ingest, unchanged/non-indexable → skip. Returns the change count.
func (r *Reconciler) Reconcile(ctx context.Context, paths []string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	changed := 0
	for _, p := range paths {
		id := corpus.DocID(r.root, p)
		info, err := os.Stat(p)
		switch {
		case os.IsNotExist(err):
			if _, ok := r.known[id]; ok {
				if err := r.applyDelete(ctx, id); err != nil {
					return changed, err
				}
				changed++
			}
		case err != nil:
			return changed, err
		case info.IsDir() || !corpus.IsIndexable(info.Name()):
			// directory events and non-.md files are not indexable
		default:
			doc, err := corpus.LoadDoc(r.root, p)
			if err != nil {
				return changed, err
			}
			if sameContent(r.stampOf(doc), r.known[id]) {
				continue // content unchanged (e.g. a bare touch)
			}
			if err := r.applyUpsert(ctx, doc); err != nil {
				return changed, err
			}
			changed++
		}
	}
	return changed, nil
}

// applyUpsert deletes any prior version then ingests the new one, keeping the
// cache in step. Caller holds r.mu.
func (r *Reconciler) applyUpsert(ctx context.Context, d core.Document) error {
	if err := r.db.Delete(ctx, []string{d.ID}); err != nil {
		return err
	}
	if err := r.db.Ingest(ctx, []core.Document{d}); err != nil {
		return err
	}
	r.known[d.ID] = r.stampOf(d)
	return nil
}

// applyDelete drops a document and forgets its stamp. Caller holds r.mu.
func (r *Reconciler) applyDelete(ctx context.Context, id string) error {
	if err := r.db.Delete(ctx, []string{id}); err != nil {
		return err
	}
	delete(r.known, id)
	return nil
}

// sameContent reports whether the on-disk stamp matches the indexed one. The
// content hash is authoritative when both sides have it (so a bare touch that
// only bumps mtime is not a change); it falls back to mtime for legacy points
// indexed before stamping existed (empty sha).
func sameContent(disk, known store.DocStamp) bool {
	if disk.SHA != "" && known.SHA != "" {
		return disk.SHA == known.SHA
	}
	return known.MTime != 0 && disk.MTime == known.MTime
}

// stampOf reads the (mtime, sha) stamp from a freshly loaded Document's Meta.
func (r *Reconciler) stampOf(d core.Document) store.DocStamp {
	var st store.DocStamp
	if v, ok := d.Meta["mtime"].(int64); ok {
		st.MTime = v
	}
	if v, ok := d.Meta["sha"].(string); ok {
		st.SHA = v
	}
	return st
}
