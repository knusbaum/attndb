package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/store"
)

// fakeStore is an in-memory Store recording the current doc set by ID.
type fakeStore struct {
	docs    map[string]core.Document
	ingests int
	deletes int
	withSHA bool // include sha in Stamps (simulate a stamped index)
}

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string]core.Document{}} }

func (f *fakeStore) Ingest(_ context.Context, docs []core.Document) error {
	for _, d := range docs {
		f.docs[d.ID] = d
		f.ingests++
	}
	return nil
}
func (f *fakeStore) Delete(_ context.Context, ids []string) error {
	for _, id := range ids {
		delete(f.docs, id)
		f.deletes++
	}
	return nil
}
func (f *fakeStore) Stamps(_ context.Context) (map[string]store.DocStamp, error) {
	out := map[string]store.DocStamp{}
	for id, d := range f.docs {
		var st store.DocStamp
		if v, ok := d.Meta["mtime"].(int64); ok {
			st.MTime = v
		}
		if f.withSHA {
			if v, ok := d.Meta["sha"].(string); ok {
				st.SHA = v
			}
		}
		out[id] = st
	}
	return out, nil
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileAll(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.md"), "alpha")
	write(t, filepath.Join(root, "b.md"), "beta")
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	write(t, filepath.Join(root, "sub", "c.md"), "gamma") // subdir recursion
	write(t, filepath.Join(root, "note.txt"), "ignored")  // non-.md
	os.Mkdir(filepath.Join(root, ".obsidian"), 0o755)
	write(t, filepath.Join(root, ".obsidian", "x.md"), "hidden") // hidden dir

	fs := newFakeStore()
	r := New(root, fs, nil)
	ctx := context.Background()

	n, err := r.ReconcileAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("initial: want 3 changes, got %d", n)
	}
	if len(fs.docs) != 3 {
		t.Fatalf("want 3 docs indexed, got %d: %v", len(fs.docs), keys(fs.docs))
	}
	if _, ok := fs.docs["sub/c.md"]; !ok {
		t.Fatalf("subdir doc not indexed by relative id; have %v", keys(fs.docs))
	}

	// No-op pass: nothing changed on disk.
	if n, _ := r.ReconcileAll(ctx); n != 0 {
		t.Fatalf("second pass: want 0 changes, got %d", n)
	}
}

// TestReconcileAllDetectsBulkDeletion is the backstop scenario: a whole subdir
// vanishes (a bulk/dir op the granular watcher may miss). ReconcileAll must drop
// the gone docs by diffing disk against the tracked set.
func TestReconcileAllDetectsBulkDeletion(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	write(t, filepath.Join(root, "keep.md"), "keep")
	write(t, filepath.Join(root, "sub", "a.md"), "a")
	write(t, filepath.Join(root, "sub", "b.md"), "b")

	fs := newFakeStore()
	r := New(root, fs, nil)
	ctx := context.Background()
	r.ReconcileAll(ctx)
	if len(fs.docs) != 3 {
		t.Fatalf("setup: want 3 docs, got %d", len(fs.docs))
	}

	os.RemoveAll(filepath.Join(root, "sub")) // bulk removal
	n, err := r.ReconcileAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 deletions, got %d", n)
	}
	if len(fs.docs) != 1 {
		t.Fatalf("want 1 doc left, got %d: %v", len(fs.docs), keys(fs.docs))
	}
	if _, ok := fs.docs["keep.md"]; !ok {
		t.Fatalf("keep.md should survive")
	}
}

func TestReconcileGranular(t *testing.T) {
	root := t.TempDir()
	apath := filepath.Join(root, "a.md")
	write(t, apath, "alpha")

	fs := newFakeStore()
	r := New(root, fs, nil)
	ctx := context.Background()
	r.ReconcileAll(ctx) // seed: a.md indexed

	// update a.md
	write(t, apath, "alpha v2")
	if n, _ := r.Reconcile(ctx, []string{apath}); n != 1 {
		t.Fatalf("update: want 1 change, got %d", n)
	}
	if fs.docs["a.md"].Text != "alpha v2" {
		t.Fatalf("update not applied: %q", fs.docs["a.md"].Text)
	}

	// touch with identical content → no re-ingest
	before := fs.ingests
	write(t, apath, "alpha v2")
	later := time.Now().Add(2 * time.Hour)
	os.Chtimes(apath, later, later)
	if n, _ := r.Reconcile(ctx, []string{apath}); n != 0 {
		t.Fatalf("touch: want 0 changes, got %d", n)
	}
	if fs.ingests != before {
		t.Fatalf("touch caused a re-ingest: %d -> %d", before, fs.ingests)
	}

	// delete a.md
	os.Remove(apath)
	if n, _ := r.Reconcile(ctx, []string{apath}); n != 1 {
		t.Fatalf("delete: want 1 change, got %d", n)
	}
	if _, ok := fs.docs["a.md"]; ok {
		t.Fatalf("doc still indexed after delete")
	}

	// rename b.md -> c.md (delete old id, add new id)
	bpath := filepath.Join(root, "b.md")
	cpath := filepath.Join(root, "c.md")
	write(t, bpath, "beta")
	r.Reconcile(ctx, []string{bpath})
	os.Rename(bpath, cpath)
	if n, _ := r.Reconcile(ctx, []string{bpath, cpath}); n != 2 {
		t.Fatalf("rename: want 2 changes (delete+add), got %d", n)
	}
	if _, ok := fs.docs["b.md"]; ok {
		t.Fatalf("old name still indexed after rename")
	}
	if _, ok := fs.docs["c.md"]; !ok {
		t.Fatalf("new name not indexed after rename")
	}
}

func keys(m map[string]core.Document) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
