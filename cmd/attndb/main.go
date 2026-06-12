// Command attndb runs the multi-pass database: it ingests a directory of
// Markdown documents and runs a query, printing ranked, localized results. The
// store is selectable (in-memory or Qdrant); the encoders here are the stub
// encoders (real ONNX encoders implement the same interfaces and drop in).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/db"
	"github.com/kjn/attndb/internal/encode"
	"github.com/kjn/attndb/internal/pass"
	"github.com/kjn/attndb/internal/store"
)

func main() {
	docsDir := flag.String("docs", "./sample_docs", "directory of .md documents to ingest")
	k := flag.Int("k", 5, "number of results to show")
	dim := flag.Int("dim", 64, "stub embedding dimension")
	storeKind := flag.String("store", "memory", "vector store: memory | qdrant")
	qaddr := flag.String("qdrant", "localhost:6334", "qdrant gRPC host:port (when -store=qdrant)")
	filterFlag := flag.String("filter", "", "payload equality filter key=value (e.g. doc_id=policy.md)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: attndb [-store memory|qdrant] [-docs dir] [-k N] <query...>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	query := strings.Join(flag.Args(), " ")
	if query == "" {
		flag.Usage()
		os.Exit(2)
	}

	mk, err := poolFactory(*storeKind, *qaddr)
	if err != nil {
		fatal("%v", err)
	}

	ctx := context.Background()
	database, err := buildDB(*dim, mk)
	if err != nil {
		fatal("build db: %v", err)
	}

	docs, err := loadDocs(*docsDir)
	if err != nil {
		fatal("load docs: %v", err)
	}
	if len(docs) == 0 {
		fatal("no .md documents found in %s", *docsDir)
	}
	if err := database.Ingest(ctx, docs); err != nil {
		fatal("ingest: %v", err)
	}
	fmt.Printf("ingested %d document(s) from %s into %s store\n\n", len(docs), *docsDir, *storeKind)

	q := core.Query{Text: query}
	if *filterFlag != "" {
		key, val, ok := strings.Cut(*filterFlag, "=")
		if !ok {
			fatal("invalid -filter %q (want key=value)", *filterFlag)
		}
		q.Filters = map[string]any{key: val}
	}
	results, err := database.Search(ctx, q, *k)
	if err != nil {
		fatal("search: %v", err)
	}
	printResults(query, results)
}

// poolFactory returns a function that creates a named Pool for the chosen store.
func poolFactory(kind, qaddr string) (func(name string) (store.Pool, error), error) {
	switch kind {
	case "memory":
		return func(name string) (store.Pool, error) { return store.NewMemory(name), nil }, nil
	case "qdrant":
		host, port, err := splitHostPort(qaddr)
		if err != nil {
			return nil, fmt.Errorf("invalid -qdrant %q: %w", qaddr, err)
		}
		return func(name string) (store.Pool, error) { return store.NewQdrant(host, port, name) }, nil
	default:
		return nil, fmt.Errorf("unknown -store %q (want memory|qdrant)", kind)
	}
}

func buildDB(dim int, mk func(string) (store.Pool, error)) (*db.DB, error) {
	single := encode.NewStubSingle(dim)
	multi := encode.NewStubMulti(dim)

	tokPool, err := mk("attndb_tok_section")
	if err != nil {
		return nil, err
	}
	paraPool, err := mk("attndb_para")
	if err != nil {
		return nil, err
	}
	docPool, err := mk("attndb_doc")
	if err != nil {
		return nil, err
	}

	passes := []pass.Pass{
		// per-token core: section-aligned ~1K window; deposits MaxSim matches
		pass.NewPerTokenPass("tok-section", chunk.BySection(1024, 128), multi, tokPool),
		// paragraph-level single-vector: diffuse-meaning deposits
		pass.NewSingleVectorPass("para", chunk.ByParagraph(), single, paraPool),
		// document-level single-vector: coarse topical / whole-doc gestalt prior
		pass.NewSingleVectorPass("doc", chunk.WholeDoc(), single, docPool),
	}
	return db.New(passes), nil
}

func loadDocs(dir string) ([]core.Document, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var docs []core.Document
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		docs = append(docs, core.Document{
			ID:   e.Name(),
			Text: string(b),
			Meta: map[string]any{"path": e.Name()},
		})
	}
	return docs, nil
}

func splitHostPort(addr string) (string, int, error) {
	host, portStr, ok := strings.Cut(addr, ":")
	if !ok {
		return "", 0, fmt.Errorf("expected host:port")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func printResults(query string, results []core.Result) {
	fmt.Printf("query: %q\n", query)
	if len(results) == 0 {
		fmt.Println("(no results)")
		return
	}
	for i, r := range results {
		snippet := strings.Join(strings.Fields(r.Snippet), " ")
		fmt.Printf("\n%d. [%.4f] %s  bytes %d–%d\n   %s\n",
			i+1, r.Score, r.DocID, r.Span.Start, r.Span.End, snippet)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
