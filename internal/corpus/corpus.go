// Package corpus loads documents from a directory tree into core.Documents,
// applying the ignore rules and the (doc_id, mtime, sha) stamping shared by the
// CLI ingest path and the live reconciler. Doc IDs are paths relative to the
// root, so they are stable identifiers for a file and unique across subdirs.
package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjn/attndb/internal/core"
)

// IsIndexable reports whether a path should be indexed: a regular .md file, not
// under a hidden directory. Mirrors the WalkDir skip logic so the watcher and
// the loader agree on what counts.
func IsIndexable(name string) bool {
	return strings.HasSuffix(name, ".md")
}

// DocID returns the doc_id for absPath under root: the relative path.
func DocID(root, absPath string) string {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

// LoadDir walks root and returns a Document for every indexable file, stamped
// with mtime and content sha. Hidden directories (.obsidian, .git, …) are
// skipped entirely.
func LoadDir(root string) ([]core.Document, error) {
	var docs []core.Document
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !IsIndexable(d.Name()) {
			return nil
		}
		doc, err := LoadDoc(root, path)
		if err != nil {
			return err
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// LoadDoc reads a single file into a stamped Document. Its ID is the path
// relative to root.
func LoadDoc(root, absPath string) (core.Document, error) {
	b, err := os.ReadFile(absPath)
	if err != nil {
		return core.Document{}, err
	}
	var mtime int64
	if info, err := os.Stat(absPath); err == nil {
		mtime = info.ModTime().Unix()
	}
	sum := sha256.Sum256(b)
	id := DocID(root, absPath)
	return core.Document{
		ID:   id,
		Text: string(b),
		Meta: map[string]any{
			"path":  id,
			"mtime": mtime,
			"sha":   hex.EncodeToString(sum[:]),
		},
	}, nil
}
