package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	return &vaultService{root: root, rootFS: rootFS}
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
