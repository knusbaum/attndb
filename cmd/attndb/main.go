// Command attndb is the CLI for the multi-pass database. Subcommands:
//
//	ingest   encode documents and store them (use -store qdrant to persist)
//	search   query an already-ingested store (no re-encoding)
//	query    one-shot: ingest then search in one process (good for -store memory)
//
// The store is selectable (memory | qdrant); encoders are stub (default) or onnx
// (real GTE-ModernColBERT + gte-modernbert-base, needs a -tags onnx build).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/corpus"
	"github.com/kjn/attndb/internal/db"
	"github.com/kjn/attndb/internal/encode"
	"github.com/kjn/attndb/internal/pass"
	"github.com/kjn/attndb/internal/store"
)

// sampleQueriesN is how many pseudo-queries calibration uses.
const sampleQueriesN = 150

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "ingest":
		err = runIngest(args)
	case "search":
		err = runSearch(args)
	case "query":
		err = runQuery(args)
	case "serve":
		err = runServe(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal("%v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: attndb <command> [flags] [query...]

commands:
  ingest   encode documents and store them (use -store qdrant to persist)
  search   query an already-ingested store (no re-encoding)
  query    one-shot: ingest then search in one process (good for -store memory)
  serve    long-running daemon: watch docs, keep the index synced, search over MCP

common flags: -store memory|qdrant  -qdrant host:port  -encoder stub|onnx
              -provider cpu|coreml  -model dir  -dim N  -docs dir
search/query: -k N  -filter key=value
`)
}

// common holds the flags shared by all subcommands.
type common struct {
	store, qaddr, encoder, provider, model, docs, calib, ns *string
	dim                                                     *int
	// retrieval tuning knobs (exposed for measurement/A-B; see buildDB)
	recallK                 *int
	peak, wTok, wPara, wDoc *float64
}

func registerCommon(fs *flag.FlagSet) *common {
	return &common{
		store:    fs.String("store", "memory", "vector store: memory | qdrant"),
		qaddr:    fs.String("qdrant", "localhost:6334", "qdrant gRPC host:port"),
		encoder:  fs.String("encoder", "stub", "encoder: stub | onnx"),
		provider: fs.String("provider", "cpu", "onnx execution provider: cpu | coreml (Mac/Metal)"),
		model:    fs.String("model", "models", "model directory (onnx encoder)"),
		docs:     fs.String("docs", "./sample_docs", "documents directory"),
		calib:    fs.String("calib", ".attndb-calibration.json", "calibration file (written by ingest, read by search)"),
		ns:       fs.String("ns", "", "collection namespace: isolates this index from others in the same store (empty = default shared collections)"),
		dim:      fs.Int("dim", 64, "stub embedding dimension"),
		recallK:  fs.Int("recallK", 50, "candidates each pass contributes before fusion"),
		peak:     fs.Float64("peak", 0.5, "peak-region growth fraction (heat accumulation)"),
		wTok:     fs.Float64("w-tok", 1, "weight of the per-token (ColBERT) pass"),
		wPara:    fs.Float64("w-para", 1, "weight of the paragraph single-vector pass"),
		wDoc:     fs.Float64("w-doc", 0.25, "weight of the whole-doc single-vector pass"),
	}
}

func (c *common) database() (*db.DB, error) {
	mk, err := poolFactory(*c.store, *c.qaddr)
	if err != nil {
		return nil, err
	}
	multi, single, err := encoders(*c.encoder, *c.dim, *c.model, *c.provider)
	if err != nil {
		return nil, err
	}
	return buildDB(multi, single, *c.encoder, *c.ns, mk, tuning{
		recallK: *c.recallK, peak: *c.peak,
		wTok: *c.wTok, wPara: *c.wPara, wDoc: *c.wDoc,
	})
}

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	c := registerCommon(fs)
	fs.Parse(args)
	if *c.store == "memory" {
		return fmt.Errorf("`ingest -store memory` has no effect (in-memory store isn't persisted); use -store qdrant, or the `query` command")
	}
	docs, err := loadDocs(*c.docs)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("no .md documents found in %s", *c.docs)
	}
	database, err := c.database()
	if err != nil {
		return err
	}
	ctx := context.Background()
	start := time.Now()
	if err := database.Ingest(ctx, docs); err != nil {
		return err
	}
	fmt.Printf("ingested %d document(s) from %s into %s in %s\n",
		len(docs), *c.docs, *c.store, time.Since(start).Round(time.Millisecond))

	// calibrate per-pass score ranges so non-matches score low at search time
	cstart := time.Now()
	samples := sampleQueries(docs, sampleQueriesN)
	calib, err := database.Calibrate(ctx, samples, noiseQueries())
	if err != nil {
		return err
	}
	if err := saveCalibration(*c.calib, *c.encoder, calib); err != nil {
		return err
	}
	fmt.Printf("calibrated %d pass(es) from %d sample queries in %s -> %s\n",
		len(calib), len(samples), time.Since(cstart).Round(time.Millisecond), *c.calib)
	return nil
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	c := registerCommon(fs)
	k := fs.Int("k", 5, "number of results to show")
	minScore := fs.Float64("min", 0, "drop results below this score (relevance gate; needs calibration)")
	filterFlag := fs.String("filter", "", "payload equality filter key=value")
	explain := fs.Bool("explain", false, "print each pass's recalled candidates and scores")
	fs.Parse(args)
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return fmt.Errorf("search needs a query, e.g. attndb search -store qdrant \"...\"")
	}
	if *c.store == "memory" {
		return fmt.Errorf("`search -store memory` finds nothing (the in-memory store isn't persisted across runs); ingest into -store qdrant first, or use `query`")
	}
	database, err := c.database()
	if err != nil {
		return err
	}
	calib, err := loadCalibration(*c.calib, *c.encoder)
	if err != nil {
		return err
	}
	if calib != nil {
		database.SetCalibration(calib)
	}
	text, err := loadText(*c.docs) // doc text for snippets only — no encoding
	if err != nil {
		return err
	}
	if *explain {
		exp, err := database.Explain(context.Background(), buildQuery(query, *filterFlag), *k)
		if err != nil {
			return err
		}
		printExplain(query, exp)
		return nil
	}
	results, err := database.Search(context.Background(), buildQuery(query, *filterFlag), *k)
	if err != nil {
		return err
	}
	printResults(query, gate(results, *minScore), text)
	return nil
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	c := registerCommon(fs)
	k := fs.Int("k", 5, "number of results to show")
	minScore := fs.Float64("min", 0, "drop results below this score (relevance gate)")
	filterFlag := fs.String("filter", "", "payload equality filter key=value")
	explain := fs.Bool("explain", false, "print each pass's recalled candidates and scores")
	fs.Parse(args)
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return fmt.Errorf("query needs a query string")
	}
	docs, err := loadDocs(*c.docs)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("no .md documents found in %s", *c.docs)
	}
	database, err := c.database()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := database.Ingest(ctx, docs); err != nil {
		return err
	}
	// one-shot: calibrate in-process (not persisted)
	if calib, err := database.Calibrate(ctx, sampleQueries(docs, sampleQueriesN), noiseQueries()); err == nil {
		database.SetCalibration(calib)
	}
	text := make(map[string]string, len(docs))
	for _, d := range docs {
		text[d.ID] = d.Text
	}
	if *explain {
		exp, err := database.Explain(ctx, buildQuery(query, *filterFlag), *k)
		if err != nil {
			return err
		}
		printExplain(query, exp)
		return nil
	}
	results, err := database.Search(ctx, buildQuery(query, *filterFlag), *k)
	if err != nil {
		return err
	}
	printResults(query, gate(results, *minScore), text)
	return nil
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

// encoders selects the encoder pair. onnxEncoders is provided by a build-tagged
// file (real under -tags onnx, an error otherwise).
func encoders(kind string, dim int, modelDir, provider string) (core.MultiVectorEncoder, core.SingleVectorEncoder, error) {
	switch kind {
	case "stub":
		if provider != "cpu" {
			return nil, nil, fmt.Errorf("-provider %q requires -encoder onnx", provider)
		}
		return encode.NewStubMulti(dim), encode.NewStubSingle(dim), nil
	case "onnx":
		return onnxEncoders(modelDir, provider)
	default:
		return nil, nil, fmt.Errorf("unknown -encoder %q (want stub|onnx)", kind)
	}
}

// tuning carries the retrieval knobs exposed on the CLI for measurement/A-B.
type tuning struct {
	recallK           int
	peak              float64
	wTok, wPara, wDoc float64
}

func buildDB(multi core.MultiVectorEncoder, single core.SingleVectorEncoder, encKind, ns string, mk func(string) (store.Pool, error), t tuning) (*db.DB, error) {
	// collection names are encoder-scoped so different encoders (and dims) don't
	// collide in the same store. An optional namespace further isolates a corpus
	// (e.g. the Obsidian vault) from the default shared collections.
	prefix := "attndb"
	if ns != "" {
		prefix += "_" + ns
	}
	suffix := "_" + encKind
	tokPool, err := mk(prefix + "_tok_section" + suffix)
	if err != nil {
		return nil, err
	}
	paraPool, err := mk(prefix + "_para" + suffix)
	if err != nil {
		return nil, err
	}
	docPool, err := mk(prefix + "_doc" + suffix)
	if err != nil {
		return nil, err
	}
	passes := []pass.Pass{
		// per-token core: section-aligned (kept under the model's 300-token doc cap)
		pass.NewPerTokenPass("tok-section", chunk.BySection(200, 32), multi, tokPool, pass.WithWeight(t.wTok)),
		// paragraph-level single-vector: diffuse-meaning deposits
		pass.NewSingleVectorPass("para", chunk.ByParagraph(), single, paraPool, pass.WithWeight(t.wPara)),
		// document-level single-vector: a GENTLE whole-doc topical prior — kept
		// low-weight so its flat deposit lifts the document without engulfing the
		// localized peaks from the token/paragraph passes.
		pass.NewSingleVectorPass("doc", chunk.WholeDoc(), single, docPool, pass.WithWeight(t.wDoc)),
	}
	return db.New(passes, db.WithRecallK(t.recallK), db.WithPeakFraction(t.peak)), nil
}

func buildQuery(text, filter string) core.Query {
	q := core.Query{Text: text}
	if filter != "" {
		if key, val, ok := strings.Cut(filter, "="); ok {
			q.Filters = map[string]any{key: val}
		}
	}
	return q
}

// sampleQueries builds short pseudo-queries (paragraph openings) for calibration.
func sampleQueries(docs []core.Document, n int) []string {
	var pool []string
	for _, d := range docs {
		for _, ch := range chunk.ByParagraph().Chunk(d) {
			words := strings.Fields(ch.Text)
			if len(words) < 4 {
				continue
			}
			if len(words) > 12 {
				words = words[:12]
			}
			pool = append(pool, strings.Join(words, " "))
		}
	}
	r := rand.New(rand.NewSource(1)) // deterministic
	r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if n > 0 && len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

// noiseQueries are out-of-domain, question-shaped queries used as calibration
// negatives: they share the form of real queries but have no relation to any
// corpus, so the best score they reach marks each pass's noise ceiling (LO).
// Kept content-diverse and corpus-agnostic so the floor reflects "unrelated
// query" rather than "rare in-corpus term".
func noiseQueries() []string {
	return []string{
		"how do you bake sourdough bread at high altitude",
		"what is the migration pattern of monarch butterflies",
		"best way to tune a six string acoustic guitar",
		"history of the roman aqueduct construction techniques",
		"why do cats purr when they are content",
		"recipe for a classic neapolitan margherita pizza",
		"how far is the andromeda galaxy from earth",
		"rules for scoring a game of cricket",
		"how to prune tomato plants for better yield",
		"what causes the northern lights to appear",
		"techniques for watercolor landscape painting",
		"how do bees communicate the location of flowers",
		"what year did the first transcontinental railroad open",
		"how to brew a proper cup of green tea",
		"the life cycle of a pacific salmon",
		"how do submarines control their buoyancy underwater",
		"famous composers of the baroque musical period",
		"how to change a flat tire on a bicycle",
		"what makes a volcano erupt explosively",
		"steps for folding an origami paper crane",
		"how do tides relate to the phases of the moon",
		"traditional ingredients in a moroccan tagine",
		"why is the sky blue during the day",
		"how to train a puppy to sit and stay",
		"the difference between stalactites and stalagmites",
	}
}

// calibration files hold one entry per encoder.
func loadCalibration(path, encoder string) (db.Calibration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cf := map[string]db.Calibration{}
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	return cf[encoder], nil
}

func saveCalibration(path, encoder string, c db.Calibration) error {
	cf := map[string]db.Calibration{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cf)
	}
	cf[encoder] = c
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// gate drops results scoring below min (the relevance floor).
func gate(results []core.Result, min float64) []core.Result {
	if min <= 0 {
		return results
	}
	out := results[:0:0]
	for _, r := range results {
		if float64(r.Score) >= min {
			out = append(out, r)
		}
	}
	return out
}

// loadDocs walks dir for indexable, stamped documents (see internal/corpus).
func loadDocs(dir string) ([]core.Document, error) {
	return corpus.LoadDir(dir)
}

// loadText reads document text for snippet rendering without building Documents.
func loadText(dir string) (map[string]string, error) {
	docs, err := loadDocs(dir)
	if err != nil {
		return nil, err
	}
	text := make(map[string]string, len(docs))
	for _, d := range docs {
		text[d.ID] = d.Text
	}
	return text, nil
}

func printResults(query string, results []core.Result, text map[string]string) {
	fmt.Printf("query: %q\n", query)
	if len(results) == 0 {
		fmt.Println("(no results)")
		return
	}
	for i, r := range results {
		fmt.Printf("\n%d. [%.4f] %s  bytes %d–%d\n   %s\n",
			i+1, r.Score, r.DocID, r.Span.Start, r.Span.End, snippet(text[r.DocID], r.Span))
	}
}

// printExplain dumps each pass's recalled candidates (top by contribution) with
// raw/normalized/weighted scores, then the fused peaks — so you can see where a
// document ranks in each lane and whether it's a recall, scoring, or fusion gap.
func printExplain(query string, exp *db.Explanation) {
	fmt.Printf("query: %q\n", query)
	const topPerPass = 10
	for _, p := range exp.Passes {
		cands := append([]db.ExplainCand(nil), p.Cands...)
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Contribution > cands[j].Contribution })
		fmt.Printf("\n== pass %q (weight %.2f, %d candidates) ==\n", p.Pass, p.Weight, len(p.Cands))
		if len(cands) == 0 {
			fmt.Println("   (no candidates recalled)")
			continue
		}
		fmt.Printf("   %-32s %8s %6s %6s\n", "doc  bytes", "raw", "norm", "contrib")
		for i, c := range cands {
			if i >= topPerPass {
				fmt.Printf("   … %d more\n", len(cands)-topPerPass)
				break
			}
			loc := fmt.Sprintf("%s %d–%d", c.DocID, c.Span.Start, c.Span.End)
			fmt.Printf("   %-32s %8.4f %6.3f %6.3f\n", loc, c.Raw, c.Norm, c.Contribution)
		}
	}
	fmt.Printf("\n== fused peaks ==\n")
	if len(exp.Peaks) == 0 {
		fmt.Println("   (none)")
		return
	}
	for i, r := range exp.Peaks {
		fmt.Printf("   %d. [%.4f] %s  bytes %d–%d\n", i+1, r.Score, r.DocID, r.Span.Start, r.Span.End)
	}
}

func snippet(text string, span core.Span) string {
	if span.Start < 0 || span.End > len(text) || span.Start >= span.End {
		return ""
	}
	s := strings.Join(strings.Fields(text[span.Start:span.End]), " ")
	const max = 240
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
