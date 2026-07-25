package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testVault builds a vaultService over a fresh, canonicalized temp root with a
// confined os.Root — matching how runServe sets things up (EvalSymlinks so the
// darwin /var -> /private/var symlink doesn't confuse path comparisons).
func testVault(t *testing.T) *vaultService {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("evalsymlinks temp root: %v", err)
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { rootFS.Close() })
	return &vaultService{root: root, rootFS: rootFS, reads: newReadState()}
}

func TestVaultName(t *testing.T) {
	valid := map[string]string{
		"/note.md":                  "note.md",
		"/Research/2026-07-14-x.md": filepath.FromSlash("Research/2026-07-14-x.md"),
		"/a//b.md":                  filepath.FromSlash("a/b.md"),
		// Absolute-in-vault semantics: ".." is clamped at the root, so it can only
		// ever name something *inside* the vault — safe (and os.Root re-checks).
		"/../x.md": "x.md",
	}
	for in, want := range valid {
		got, err := vaultName(in)
		if err != nil {
			t.Errorf("vaultName(%q): unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("vaultName(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"",                     // empty
		"relative.md",          // not absolute-in-vault
		"/",                    // names the root, not a file
		"/notes.txt",           // not .md
		"/notes",               // no extension
		"/.obsidian/config.md", // hidden dir component
		"/a/.git/x.md",         // nested hidden dir
	}
	for _, in := range bad {
		if got, err := vaultName(in); err == nil {
			t.Errorf("vaultName(%q) = %q, expected error", in, got)
		}
	}
}

func TestFSToolsRoundTrip(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()

	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{
		Path: "/Research/x.md", Content: "line1\nline2\nline3\n",
	}); err != nil {
		t.Fatalf("writeDoc: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.root, "Research", "x.md")); err != nil {
		t.Fatalf("written file missing on disk: %v", err)
	}

	_, out, err := svc.readDoc(ctx, nil, readDocInput{Path: "/Research/x.md"})
	if err != nil {
		t.Fatalf("readDoc: %v", err)
	}
	if out.TotalLines != 3 {
		t.Errorf("total_lines = %d, want 3", out.TotalLines)
	}
	// Verbatim: exactly the bytes on disk, no line-number prefixes, so the text
	// can go straight back into edit_doc.
	if out.Content != "line1\nline2\nline3\n" {
		t.Errorf("read content not verbatim: %q", out.Content)
	}

	_, eout, err := svc.editDoc(ctx, nil, editDocInput{
		Path: "/Research/x.md", OldString: "line2", NewString: "LINE2",
	})
	if err != nil {
		t.Fatalf("editDoc: %v", err)
	}
	if eout.Replacements != 1 {
		t.Errorf("replacements = %d, want 1", eout.Replacements)
	}
	b, _ := os.ReadFile(filepath.Join(svc.root, "Research", "x.md"))
	if !strings.Contains(string(b), "LINE2") {
		t.Errorf("edit not applied: %q", b)
	}

	if _, _, err := svc.deleteDoc(ctx, nil, deleteDocInput{Path: "/Research/x.md"}); err != nil {
		t.Fatalf("deleteDoc: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.root, "Research", "x.md")); !os.IsNotExist(err) {
		t.Errorf("file still present after delete (err=%v)", err)
	}
}

func TestReadDocPaging(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "row%d\n", i)
	}
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/big.md", Content: sb.String()}); err != nil {
		t.Fatal(err)
	}
	_, out, err := svc.readDoc(ctx, nil, readDocInput{Path: "/big.md", Offset: 4, Limit: 3})
	if err != nil {
		t.Fatalf("readDoc: %v", err)
	}
	if out.StartLine != 4 || out.EndLine != 6 || out.TotalLines != 10 {
		t.Errorf("paging window = [%d,%d] of %d, want [4,6] of 10", out.StartLine, out.EndLine, out.TotalLines)
	}
	// The window is the verbatim slice — position comes from the metadata above,
	// never from prefixes baked into the text.
	if out.Content != "row4\nrow5\nrow6\n" {
		t.Errorf("window content = %q, want %q", out.Content, "row4\nrow5\nrow6\n")
	}
}

// TestReadEditRoundTrip is the regression test for the line-number footgun:
// text returned by read_doc must be usable as edit_doc's old_string with no
// massaging. When read_doc prefixed each line with "%6d\t", every such edit
// failed with "old_string not found" even though the text was visibly present.
func TestReadEditRoundTrip(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()
	const body = "# Log\n\n## 2026-07-20\n- did a thing\n"
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/log.md", Content: body}); err != nil {
		t.Fatal(err)
	}
	// Read a window, then feed exactly what came back straight into edit_doc.
	_, out, err := svc.readDoc(ctx, nil, readDocInput{Path: "/log.md", Offset: 3, Limit: 2})
	if err != nil {
		t.Fatalf("readDoc: %v", err)
	}
	if _, eout, err := svc.editDoc(ctx, nil, editDocInput{
		Path: "/log.md", OldString: out.Content, NewString: "## 2026-07-21\n- did another\n",
	}); err != nil {
		t.Fatalf("editDoc with verbatim read output: %v", err)
	} else if eout.Replacements != 1 {
		t.Errorf("replacements = %d, want 1", eout.Replacements)
	}
	b, _ := os.ReadFile(filepath.Join(svc.root, "log.md"))
	if want := "# Log\n\n## 2026-07-21\n- did another\n"; string(b) != want {
		t.Errorf("round-trip edit produced %q, want %q", b, want)
	}
}

// TestWriteDocAppend covers the append path: it creates when absent, adds to the
// end when present, and inserts a separating newline when the existing file does
// not end with one (so an append never glues onto the last line).
func TestWriteDocAppend(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()

	// Creates the file when it does not exist.
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{
		Path: "/daily/2026-07-20.md", Content: "# 2026-07-20\n", Append: true,
	}); err != nil {
		t.Fatalf("append-create: %v", err)
	}
	// Appends to the end, preserving what was there.
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{
		Path: "/daily/2026-07-20.md", Content: "- entry one\n", Append: true,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(svc.root, "daily", "2026-07-20.md"))
	if want := "# 2026-07-20\n- entry one\n"; string(b) != want {
		t.Fatalf("append produced %q, want %q", b, want)
	}

	// A file not ending in a newline gets one inserted first.
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/nonl.md", Content: "no newline"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/nonl.md", Content: "added\n", Append: true}); err != nil {
		t.Fatalf("append to newline-less file: %v", err)
	}
	b, _ = os.ReadFile(filepath.Join(svc.root, "nonl.md"))
	if want := "no newline\nadded\n"; string(b) != want {
		t.Errorf("append to newline-less file produced %q, want %q", b, want)
	}

	// append=false still overwrites.
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/nonl.md", Content: "replaced\n"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(svc.root, "nonl.md"))
	if string(b) != "replaced\n" {
		t.Errorf("overwrite produced %q, want %q", b, "replaced\n")
	}
}

// TestReadBeforeOverwrite covers the guard that reconstructs the host harness's
// read-before-edit rule from the MCP session id. It drives guardOverwrite
// directly because a non-empty session id cannot be fabricated on an SDK
// ServerSession (ID() reads it from the live transport).
func TestReadBeforeOverwrite(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()
	const sess = "SESSION-A"

	// Creating a document that does not exist needs no prior read.
	if err := svc.guardOverwrite(sess, "new.md", "/new.md"); err != nil {
		t.Errorf("creation should be allowed without a read: %v", err)
	}

	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/note.md", Content: "v1\n"}); err != nil {
		t.Fatal(err)
	}

	// Existing file, this session never read it -> refused.
	if err := svc.guardOverwrite(sess, "note.md", "/note.md"); err == nil {
		t.Error("expected overwrite without a prior read to be refused")
	} else if !strings.Contains(err.Error(), "has not read it") {
		t.Errorf("unhelpful error: %v", err)
	}

	// After reading the current bytes, the overwrite is allowed.
	b, _ := os.ReadFile(filepath.Join(svc.root, "note.md"))
	svc.reads.note(sess, "note.md", sha256Hex(b))
	if err := svc.guardOverwrite(sess, "note.md", "/note.md"); err != nil {
		t.Errorf("overwrite after read should be allowed: %v", err)
	}

	// Someone else changes the file: the recorded hash is stale, so the write is
	// refused even though this session did read it. This is the case plain
	// read-before-write tracking would miss.
	if err := os.WriteFile(filepath.Join(svc.root, "note.md"), []byte("edited by a human\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.guardOverwrite(sess, "note.md", "/note.md"); err == nil {
		t.Error("expected a stale read to block the overwrite")
	} else if !strings.Contains(err.Error(), "changed on disk") {
		t.Errorf("unhelpful staleness error: %v", err)
	}

	// A different session's read does not vouch for this one.
	cur, _ := os.ReadFile(filepath.Join(svc.root, "note.md"))
	svc.reads.note("SESSION-B", "note.md", sha256Hex(cur))
	if err := svc.guardOverwrite(sess, "note.md", "/note.md"); err == nil {
		t.Error("one session's read must not authorize another's overwrite")
	}

	// Fail open: no session id (stateless transport) allows the write.
	if err := svc.guardOverwrite("", "note.md", "/note.md"); err != nil {
		t.Errorf("missing session id should fail open, got: %v", err)
	}
}

// TestWriteRefreshesReadState checks the ergonomics: after writing or editing,
// the session knows the current contents, so it can write again without an
// intervening read. Append is exempt from the guard entirely.
func TestWriteRefreshesReadState(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()
	const sess = "SESSION-A"

	// Append to a fresh file, then to an existing one: never guarded.
	for i := 0; i < 2; i++ {
		if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/log.md", Content: "line\n", Append: true}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Simulate the handler having recorded the post-write state for this session.
	b, _ := os.ReadFile(filepath.Join(svc.root, "log.md"))
	svc.reads.note(sess, "log.md", sha256Hex(b))
	if err := svc.guardOverwrite(sess, "log.md", "/log.md"); err != nil {
		t.Errorf("overwrite after this session's own write should be allowed: %v", err)
	}

	// An edit also refreshes the record, so the following overwrite is allowed.
	if _, _, err := svc.editDoc(ctx, nil, editDocInput{Path: "/log.md", OldString: "line\nline\n", NewString: "one\n"}); err != nil {
		t.Fatalf("editDoc: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(svc.root, "log.md"))
	svc.reads.note(sess, "log.md", sha256Hex(after))
	if err := svc.guardOverwrite(sess, "log.md", "/log.md"); err != nil {
		t.Errorf("overwrite after this session's own edit should be allowed: %v", err)
	}
}

// TestReadStateSweep checks lazy expiry: sessions idle beyond readTTL are
// dropped, recent ones survive, and sweeping is rate-limited.
func TestReadStateSweep(t *testing.T) {
	r := newReadState()
	r.note("old", "a.md", "sum")
	r.note("fresh", "b.md", "sum")
	r.sessions["old"].at = time.Now().Add(-2 * readTTL)

	r.sweep() // lastSweep is zero, so this one runs
	if _, ok := r.sessions["old"]; ok {
		t.Error("expired session should have been swept")
	}
	if _, ok := r.sessions["fresh"]; !ok {
		t.Error("recent session should have survived the sweep")
	}

	// Rate limiting: a second sweep right away is a no-op even with a stale entry.
	r.note("old2", "c.md", "sum")
	r.sessions["old2"].at = time.Now().Add(-2 * readTTL)
	r.sweep()
	if _, ok := r.sessions["old2"]; !ok {
		t.Error("sweep should be rate-limited by readSweepIval")
	}
}

func TestVaultGlob(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"daily/*.md", "daily/2026-07-20.md", true},
		{"daily/*.md", "daily/sub/2026-07-20.md", false}, // * does not cross '/'
		{"daily/**/*.md", "daily/sub/2026-07-20.md", true},
		{"daily/**/*.md", "daily/2026-07-20.md", true}, // ** also matches zero segments
		{"**/*.md", "Research/2026/topic.md", true},
		{"*.md", "top.md", true},
		{"*.md", "sub/top.md", false},
		{"daily/2026-07-??.md", "daily/2026-07-20.md", true},
		{"daily/2026-07-??.md", "daily/2026-07-2.md", false}, // ? matches exactly one char
		{"Research/*.md", "research/x.md", false},            // literal chars are case-sensitive
		{"a.b.md", "aXbXmd", false},                          // literal '.' must not behave as regex wildcard
		{"a.b.md", "a.b.md", true},
	}
	for _, c := range cases {
		re, err := vaultGlob(c.pattern)
		if err != nil {
			t.Fatalf("vaultGlob(%q): %v", c.pattern, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("vaultGlob(%q).Match(%q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// TestListDocs covers pattern filtering, hidden-path/non-md exclusion, recency
// sort, and the limit/truncated accounting.
func TestListDocs(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()

	write := func(relPath, content string, age time.Duration) {
		p := "/" + relPath
		if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: p, Content: content}); err != nil {
			t.Fatalf("writeDoc %s: %v", p, err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(svc.root, relPath), mt, mt); err != nil {
			t.Fatalf("chtimes %s: %v", relPath, err)
		}
	}
	write("daily/2026-07-19.md", "old\n", 2*time.Hour)
	write("daily/2026-07-20.md", "new\n", 1*time.Hour)
	write("Research/topic.md", "research\n", 30*time.Minute)
	// Not real files, created directly on disk (bypassing writeDoc's .md-only
	// policy) so listDocs is proven to filter them, not merely never encounter
	// them.
	if err := os.MkdirAll(filepath.Join(svc.root, ".obsidian"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.root, ".obsidian", "config.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No pattern: every .md, most recent first, hidden dir and non-.md excluded.
	_, out, err := svc.listDocs(ctx, nil, listDocsInput{})
	if err != nil {
		t.Fatalf("listDocs: %v", err)
	}
	if out.Total != 3 {
		t.Fatalf("total = %d, want 3 (got %+v)", out.Total, out.Docs)
	}
	wantOrder := []string{"/Research/topic.md", "/daily/2026-07-20.md", "/daily/2026-07-19.md"}
	for i, w := range wantOrder {
		if out.Docs[i].Path != w {
			t.Errorf("Docs[%d] = %s, want %s (recency order wrong)", i, out.Docs[i].Path, w)
		}
	}
	if out.Truncated {
		t.Error("should not be truncated under the default limit")
	}

	// Pattern scoped to one directory.
	_, out, err = svc.listDocs(ctx, nil, listDocsInput{Pattern: "/daily/*.md"})
	if err != nil {
		t.Fatalf("listDocs pattern: %v", err)
	}
	if out.Total != 2 {
		t.Errorf("pattern total = %d, want 2", out.Total)
	}
	for _, d := range out.Docs {
		if !strings.HasPrefix(d.Path, "/daily/") {
			t.Errorf("pattern leaked non-daily path: %s", d.Path)
		}
	}

	// Limit truncates but total still reports the full count.
	_, out, err = svc.listDocs(ctx, nil, listDocsInput{Limit: 1})
	if err != nil {
		t.Fatalf("listDocs limit: %v", err)
	}
	if len(out.Docs) != 1 || out.Total != 3 || !out.Truncated {
		t.Errorf("limit=1: got %d docs, total=%d, truncated=%v; want 1, 3, true", len(out.Docs), out.Total, out.Truncated)
	}
}

// TestToolRegistration guards against a gap the handler-level tests below can't
// see: they call e.g. svc.listDocs directly, bypassing mcp.AddTool's reflection
// over the input/output struct tags entirely. A malformed jsonschema tag (a real
// risk on a new tool with a nested struct/slice output like docEntry) would only
// surface here, or in the live server — this is cheaper than restarting a real
// daemon to find out.
func TestToolRegistration(t *testing.T) {
	svc := testVault(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "attndb-test", Version: "0"}, nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering a tool panicked (bad schema tag?): %v", r)
		}
	}()
	mcp.AddTool(server, &mcp.Tool{Name: "search_vault", Description: "d"}, svc.search)
	mcp.AddTool(server, &mcp.Tool{Name: "list_docs", Description: "d"}, svc.listDocs)
	mcp.AddTool(server, &mcp.Tool{Name: "read_doc", Description: "d"}, svc.readDoc)
	mcp.AddTool(server, &mcp.Tool{Name: "write_doc", Description: "d"}, svc.writeDoc)
	mcp.AddTool(server, &mcp.Tool{Name: "edit_doc", Description: "d"}, svc.editDoc)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_doc", Description: "d"}, svc.deleteDoc)
}

func TestEditDocUniqueness(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/dup.md", Content: "dup\ndup\n"}); err != nil {
		t.Fatal(err)
	}
	// Non-unique old_string without replace_all must be refused.
	if _, _, err := svc.editDoc(ctx, nil, editDocInput{Path: "/dup.md", OldString: "dup", NewString: "y"}); err == nil {
		t.Error("expected non-unique old_string to be refused")
	}
	// replace_all replaces every occurrence.
	_, out, err := svc.editDoc(ctx, nil, editDocInput{Path: "/dup.md", OldString: "dup", NewString: "y", ReplaceAll: true})
	if err != nil {
		t.Fatalf("editDoc replace_all: %v", err)
	}
	if out.Replacements != 2 {
		t.Errorf("replacements = %d, want 2", out.Replacements)
	}
}

// TestFSToolsSymlinkEscape confirms os.Root refuses operations that would escape
// the vault via a symlink — both a leaf symlink (read) and a directory symlink
// ancestor (write) — and that nothing is written outside the tree.
func TestFSToolsSymlinkEscape(t *testing.T) {
	svc := testVault(t)
	ctx := context.Background()

	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Leaf symlink: <root>/leak.md -> <outside>/secret.md. Reading it must fail.
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(svc.root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.readDoc(ctx, nil, readDocInput{Path: "/leak.md"}); err == nil {
		t.Error("reading a leaf symlink pointing outside the vault should fail")
	}

	// Directory symlink: <root>/out -> <outside>. Writing through it must fail,
	// and must not create anything outside the vault.
	if err := os.Symlink(outside, filepath.Join(svc.root, "out")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.writeDoc(ctx, nil, writeDocInput{Path: "/out/pwned.md", Content: "x"}); err == nil {
		t.Error("writing through a directory symlink out of the vault should fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.md")); !os.IsNotExist(err) {
		t.Errorf("a file was written outside the vault via symlink (err=%v)", err)
	}
}

func TestLineOf(t *testing.T) {
	text := "abc\ndef\nghi"
	cases := []struct {
		off  int
		want int
	}{
		{0, 1},   // start of line 1
		{3, 1},   // the '\n' after line 1 is still counted at line 1
		{4, 2},   // start of line 2
		{8, 3},   // into line 3
		{999, 3}, // past end clamps to last line
	}
	for _, c := range cases {
		if got := lineOf(text, c.off); got != c.want {
			t.Errorf("lineOf(off=%d) = %d, want %d", c.off, got, c.want)
		}
	}
}
