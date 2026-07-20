package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux (served only when -debug-addr is set)
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/corpus"
	"github.com/kjn/attndb/internal/db"
	"github.com/kjn/attndb/internal/reconcile"
	"github.com/kjn/attndb/internal/watch"
)

// runServe starts the always-on daemon: it builds the DB once (resident
// encoders), keeps the index in sync with the docs directory via a file
// watcher + reconciler, recalibrates lazily as the corpus drifts, and serves
// search over MCP (Streamable HTTP).
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := registerCommon(fs)
	addr := fs.String("addr", "localhost:8765", "HTTP listen address for the MCP server")
	poll := fs.Duration("poll", 30*time.Second, "poll interval for the fallback watcher (no native FSEvents)")
	resync := fs.Duration("resync", 5*time.Minute, "periodic full ReconcileAll backstop (catches bulk/dir ops the granular watcher misses)")
	idle := fs.Duration("recal-idle", 2*time.Minute, "after edits settle, recalibrate if at least -recal-floor docs changed")
	floor := fs.Int("recal-floor", 5, "minimum changed docs before the idle debounce recalibrates (small edits ride the staleness cap)")
	maxStale := fs.Duration("recal-max-stale", time.Hour, "recalibrate at most this often when any change is pending (staleness cap)")
	defMin := fs.Float64("min", 0, "default relevance gate for search_vault (0 = no gate)")
	debugAddr := fs.String("debug-addr", "", "if set, serve net/http/pprof + debug on this address (e.g. localhost:6060)")
	memlog := fs.Duration("memlog", 5*time.Minute, "interval to log Go memstats + process RSS (0 disables); the rss-minus-Go-heap gap is native/ONNX memory")
	fs.Parse(args)

	if *c.store != "qdrant" {
		return fmt.Errorf("serve requires -store qdrant (the index must persist across restarts)")
	}
	// Canonicalize the root: the OS watcher (FSEvents on macOS) reports
	// symlink-resolved absolute paths (e.g. /private/var/… for /var/…), so the
	// reconciler's root must match or filepath.Rel yields bogus ../.. doc IDs
	// and updates/deletes fail to line up with the startup-indexed IDs.
	root := *c.docs
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	// Confined handle for the filesystem tools: os.Root guarantees no operation
	// escapes the vault via "..", an absolute path, or a symlink pointing out
	// (it returns an error instead), and is safe for concurrent use.
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("open vault root: %w", err)
	}
	defer rootFS.Close()
	ctx := context.Background()

	// Build the query DB explicitly (rather than c.database()) so the memory-heavy
	// ingest path can reuse the shared pools and ColBERT encoder while swapping in
	// a fresh, ephemeral single-vector session per reconcile. The query session
	// (in `database`) only ever encodes short queries and stays small; large
	// document encodes live in the ephemeral ingest session, which is closed after
	// each pass so its memory returns to the OS. See docs/proposal-onnx-memory.md.
	mk, err := poolFactory(*c.store, *c.qaddr)
	if err != nil {
		return err
	}
	multi, querySingle, err := encoders(*c.encoder, *c.dim, *c.model, *c.provider)
	if err != nil {
		return err
	}
	pools, err := newPools(*c.encoder, *c.ns, mk)
	if err != nil {
		return err
	}
	t := tuning{recallK: *c.recallK, peak: *c.peak, wTok: *c.wTok, wPara: *c.wPara, wDoc: *c.wDoc}
	database := assembleDB(multi, querySingle, pools, t)

	if calib, err := loadCalibration(*c.calib, *c.encoder); err != nil {
		return err
	} else if calib != nil {
		database.SetCalibration(calib)
	}

	// Per-reconcile ingest factory: a fresh single-vector session sharing the
	// query DB's pools and ColBERT encoder, closed when the pass finishes.
	newIngester := func() (reconcile.Ingester, error) {
		single, closeSingle, err := singleEncoder(*c.encoder, *c.dim, *c.model, *c.provider)
		if err != nil {
			return nil, err
		}
		return &ingestDB{db: assembleDB(multi, single, pools, t), closeSingle: closeSingle}, nil
	}

	rec := reconcile.New(root, database, newIngester)
	log.Printf("startup reconcile of %s …", root)
	changed, err := rec.ReconcileAll(ctx)
	if err != nil {
		return fmt.Errorf("startup reconcile: %w", err)
	}
	log.Printf("startup reconcile: %d document(s) changed, %d indexed", changed, rec.Count())

	svc := &vaultService{
		db:        database,
		rec:       rec,
		root:      root,
		rootFS:    rootFS,
		calibPath: *c.calib,
		encoder:   *c.encoder,
		defMin:    *defMin,
		idle:      *idle,
		idleFloor: *floor,
		recalSem:  make(chan struct{}, 1),
	}
	if changed > 0 {
		svc.recalibrate(ctx) // fresh corpus → get calibration in line before serving
	}

	w, err := watch.Best(root, *poll)
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	defer w.Close()
	go svc.watchLoop(ctx, w, *resync, *maxStale)

	// Diagnostics. The daemon's memory footprint is dominated by native ONNX
	// Runtime allocations (not the Go heap), so watch process RSS against Go's
	// own memstats: a growing rss-minus-Go-heap gap is native/ORT growth. pprof
	// (opt-in via -debug-addr) covers the Go side if that ever climbs instead.
	if *memlog > 0 {
		go memLogLoop(ctx, *memlog)
	}
	if *debugAddr != "" {
		go func() {
			log.Printf("debug/pprof server on http://%s/debug/pprof/", *debugAddr)
			if err := http.ListenAndServe(*debugAddr, nil); err != nil {
				log.Printf("debug server: %v", err)
			}
		}()
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "attndb", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_vault",
		Description: "Search the user's indexed document vault (their personal notes / knowledge corpus). " +
			"Reach for this before answering any question the user's own notes might cover — prefer a " +
			"search over guessing about their specific material. Phrase queries as natural language with " +
			"specific, entity-rich terms (names, error codes, distinctive nouns); it matches meaning, not " +
			"exact keywords. Returns ranked matches best-first, each with a document path, the line range of " +
			"the match, a calibrated score, and a snippet. To read more around a match, call read_doc with " +
			"the path and start line. Scores are calibrated: a low top score means no confident match — say " +
			"you found nothing relevant rather than dressing up a weak hit. Use a small k (3–5) for focused " +
			"questions; larger only when surveying. Always cite the path and line so the user can verify.",
	}, svc.search)
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_doc",
		Description: "Read a document from the user's vault by its absolute vault path. Returns the text " +
			"verbatim — exactly the bytes in the file, with no line-number prefixes — so what you read can be " +
			"passed straight back as edit_doc's `old_string`. The range you got is reported separately as " +
			"`start_line`/`end_line`/`total_lines`. Reads from line `offset` (default 1) for up to `limit` " +
			"lines (default 2000); page through a large document by advancing `offset` instead of pulling it " +
			"all into context. Pairs with search_vault, which gives you a path and start line to read around. " +
			"Convention: read a document before editing it.",
	}, svc.readDoc)
	mcp.AddTool(server, &mcp.Tool{
		Name: "write_doc",
		Description: "Create or overwrite a markdown document in the user's vault at an absolute vault path, " +
			"creating parent folders as needed. Use it to save durable notes or research so they become " +
			"searchable later. Set `append` to add `content` to the end of the document instead of replacing " +
			"it (creating it if absent) — the right way to grow a running log or add a section, since it " +
			"needs no read-modify-write round trip. A written document becomes searchable shortly after, not " +
			"instantly — to confirm it landed, search again after a moment rather than expecting an immediate " +
			"hit. To change part of an existing document prefer edit_doc over rewriting the whole thing. " +
			"Paths must be inside the vault and end in .md.",
	}, svc.writeDoc)
	mcp.AddTool(server, &mcp.Tool{
		Name: "edit_doc",
		Description: "Edit a document in the user's vault by replacing an exact string. `old_string` must " +
			"match the file exactly and be unique, unless `replace_all` is set. read_doc returns text " +
			"verbatim, so text you read can be used as `old_string` as-is. Prefer this over rewriting a " +
			"whole document when you are updating part of it; to add to the end of a document, use write_doc " +
			"with `append`. Convention: read the document first. Paths must be inside the vault and end in .md.",
	}, svc.editDoc)
	mcp.AddTool(server, &mcp.Tool{
		Name: "delete_doc",
		Description: "Delete a document from the user's vault by its absolute vault path. It stops being " +
			"searchable shortly after removal. Paths must be inside the vault and end in .md.",
	}, svc.deleteDoc)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	log.Printf("attndb MCP server listening on http://%s (ns=%q, docs=%s)", *addr, *c.ns, root)
	return http.ListenAndServe(*addr, handler)
}

// vaultService bundles the resident DB, reconciler, and calibration policy that
// the watch loop and the MCP tools share.
type vaultService struct {
	db        *db.DB
	rec       *reconcile.Reconciler
	root      string   // canonical absolute path of the vault root on the host
	rootFS    *os.Root // confined handle: all tool filesystem ops go through it
	calibPath string
	encoder   string
	defMin    float64
	idle      time.Duration
	idleFloor int
	recalSem  chan struct{} // size-1: at most one background recalibration
}

// ingestDB is a reconcile.Ingester backed by a DB whose single-vector session is
// ephemeral: Close destroys that session (freeing the large-encode working set)
// while the shared pools and ColBERT encoder live on.
type ingestDB struct {
	db          *db.DB
	closeSingle func() error
}

func (i *ingestDB) Ingest(ctx context.Context, docs []core.Document) error {
	return i.db.Ingest(ctx, docs)
}

func (i *ingestDB) Close() error { return i.closeSingle() }

// watchLoop applies watcher batches to the index and drives the recalibration
// policy: recalibrate when the corpus has drifted enough (churn ceiling) or has
// gone quiet for a while after changes (idle debounce).
func (s *vaultService) watchLoop(ctx context.Context, w watch.Watcher, resync, maxStale time.Duration) {
	dirty := 0
	idleTimer := time.NewTimer(s.idle)
	idleTimer.Stop()
	// Backstop: a periodic full ReconcileAll catches changes the granular
	// watcher can miss — chiefly bulk/directory operations (rm -rf a subdir, a
	// folder rename, a bulk sync), which FSEvents may report at directory
	// granularity that Reconcile(paths) skips. Diffing 100s of docs is seconds
	// and no-ops when nothing changed.
	backstop := time.NewTicker(resync)
	defer backstop.Stop()
	// Staleness cap: fold any lingering sub-floor changes into calibration at
	// most once per interval, so slow trickle edits never leave it stale.
	stale := time.NewTicker(maxStale)
	defer stale.Stop()

	recal := func() { s.recalibrate(ctx); dirty = 0 }

	apply := func(changed int, err error) {
		if err != nil {
			log.Printf("reconcile: %v", err)
			return
		}
		if changed == 0 {
			return
		}
		log.Printf("reconciled %d change(s)", changed)
		dirty += changed
		// Churn ceiling: enough of the corpus moved that calibration may have
		// meaningfully drifted — recalibrate now, regardless of the idle floor.
		if dirty >= max(20, s.rec.Count()/10) {
			recal()
			idleTimer.Stop()
		} else {
			idleTimer.Reset(s.idle) // otherwise wait for edits to settle
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case batch, ok := <-w.Events():
			if !ok {
				return
			}
			if len(batch) == 0 {
				apply(s.rec.ReconcileAll(ctx)) // poller / full resync request
			} else {
				apply(s.rec.Reconcile(ctx, batch)) // granular (FSEvents)
			}
		case <-backstop.C:
			apply(s.rec.ReconcileAll(ctx))
		case <-idleTimer.C:
			// Edits settled: recalibrate only if enough changed to matter. A
			// smaller pending count waits for the staleness cap.
			if dirty >= s.idleFloor {
				recal()
			}
		case <-stale.C:
			if dirty > 0 {
				recal()
			}
		}
	}
}

// recalibrate resamples the corpus and swaps in a fresh calibration in the
// background. It reads only document text (not the index), so it is safe
// concurrent with search and reconcile. At most one runs at a time.
func (s *vaultService) recalibrate(ctx context.Context) {
	select {
	case s.recalSem <- struct{}{}:
	default:
		return // one already in flight; the next trigger will catch up
	}
	go func() {
		defer func() { <-s.recalSem }()
		docs, err := loadDocs(s.root)
		if err != nil {
			log.Printf("recalibrate load: %v", err)
			return
		}
		samples := sampleQueries(docs, sampleQueriesN)
		calib, err := s.db.Calibrate(ctx, samples, noiseQueries())
		if err != nil {
			log.Printf("recalibrate: %v", err)
			return
		}
		s.db.SetCalibration(calib)
		if err := saveCalibration(s.calibPath, s.encoder, calib); err != nil {
			log.Printf("recalibrate save: %v", err)
			return
		}
		log.Printf("recalibrated from %d sample queries -> %s", len(samples), s.calibPath)
	}()
}

// memLogLoop periodically logs Go runtime memory against process RSS. The Go GC
// does not manage native (cgo/ONNX Runtime) allocations, so Go's own memstats
// stay flat even as the process grows; the gap (RSS − Go Sys) is that native
// footprint, and a steadily growing gap is the signal for a native leak or
// arena retention. RSS is read via `ps` (no portable stdlib for it on darwin).
func memLogLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	pid := strconv.Itoa(os.Getpid())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			goSysMiB := int64(m.Sys >> 20)
			rssMiB, native := int64(-1), int64(-1)
			if out, err := exec.Command("ps", "-o", "rss=", "-p", pid).Output(); err == nil {
				if kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
					rssMiB = kb / 1024
					native = rssMiB - goSysMiB
				}
			}
			log.Printf("mem: rss=%dMiB native(rss-go.sys)=%dMiB go.sys=%dMiB go.heapAlloc=%dMiB go.heapSys=%dMiB numGC=%d goroutines=%d",
				rssMiB, native, goSysMiB, m.HeapAlloc>>20, m.HeapSys>>20, m.NumGC, runtime.NumGoroutine())
		}
	}
}

// --- search_vault tool ---

type searchInput struct {
	Query string  `json:"query" jsonschema:"the natural-language search query"`
	K     int     `json:"k,omitempty" jsonschema:"maximum number of results (default 5)"`
	Min   float64 `json:"min,omitempty" jsonschema:"drop results scoring below this calibrated threshold; omit to use the server default"`
}

type searchHit struct {
	Path      string  `json:"path" jsonschema:"absolute vault path of the matching document"`
	StartLine int     `json:"start_line" jsonschema:"1-based first line of the matched span"`
	EndLine   int     `json:"end_line" jsonschema:"1-based last line of the matched span"`
	Score     float64 `json:"score" jsonschema:"calibrated relevance score"`
	Snippet   string  `json:"snippet" jsonschema:"text of the matched span"`
}

type searchOutput struct {
	Hits []searchHit `json:"hits" jsonschema:"ranked matches, best first"`
}

func (s *vaultService) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	k := in.K
	if k <= 0 {
		k = 5
	}
	min := in.Min
	if min == 0 {
		min = s.defMin
	}
	results, err := s.db.Search(ctx, core.Query{Text: in.Query}, k)
	if err != nil {
		return nil, searchOutput{}, err
	}
	results = gate(results, min)

	hits := make([]searchHit, 0, len(results))
	var b strings.Builder
	for i, r := range results {
		sn, startLine, endLine := s.spanPreview(r.DocID, r.Span)
		apiPath := "/" + filepath.ToSlash(r.DocID)
		hits = append(hits, searchHit{
			Path: apiPath, StartLine: startLine, EndLine: endLine,
			Score: float64(r.Score), Snippet: sn,
		})
		fmt.Fprintf(&b, "%d. [%.3f] %s:%d\n   %s\n", i+1, r.Score, apiPath, startLine, sn)
	}
	text := b.String()
	if text == "" {
		text = "no confident match"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, searchOutput{Hits: hits}, nil
}

// spanPreview reads the matched span fresh from disk and returns the snippet
// plus the 1-based line range it falls on. Reading current content (rather than
// storing it) keeps the preview and a follow-up read_doc on the same live-disk
// basis, so the line we report leads read_doc to the previewed text. Spans are
// clamped: a file edited since indexing may have shifted, and its stored offsets
// can run past the current length until the watcher re-ingests it.
func (s *vaultService) spanPreview(docID string, span core.Span) (snip string, startLine, endLine int) {
	b, err := s.rootFS.ReadFile(docID)
	if err != nil {
		return "", 0, 0
	}
	text := string(b)
	sp := span
	if sp.End > len(text) {
		sp.End = len(text)
	}
	if sp.Start > sp.End {
		sp.Start = sp.End
	}
	return snippet(text, sp), lineOf(text, sp.Start), lineOf(text, sp.End)
}

// lineOf returns the 1-based line number that byte offset off falls on.
func lineOf(text string, off int) int {
	if off > len(text) {
		off = len(text)
	}
	if off < 0 {
		off = 0
	}
	return 1 + strings.Count(text[:off], "\n")
}

// --- filesystem tools: read_doc / write_doc / edit_doc / delete_doc ---
//
// A thin read/write interface over the watched vault folder, mirroring the shape
// of the host filesystem tools (Read/Write/Edit) so a model already fluent in
// those performs well here. Paths are absolute *within the vault* ("/" = vault
// root); vaultName maps them to the confined os.Root, which guarantees no
// operation escapes the tree. The write tools only touch the filesystem — they
// never index directly: the watcher notices the change and the reconciler
// indexes it a beat later, keeping the folder the sole sync entry point and the
// recalibration policy (which counts changes in the watch loop) intact.
// Searchability is therefore eventually-consistent: a caller that must confirm a
// write should re-search briefly rather than expect an instant hit.
//
// Convention (documented, not enforced here because MCP is stateless): read a
// document before overwriting/editing it. edit_doc's unique-old_string
// requirement is the safety net against blind edits.

const readDocDefaultLimit = 2000 // lines, matching the host Read tool's default

type readDocInput struct {
	Path   string `json:"path" jsonschema:"absolute vault path of the document, e.g. /Research/2026-07-14-topic.md"`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based line to start reading from (default 1)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of lines to read (default 2000)"`
}
type readDocOutput struct {
	Path       string `json:"path" jsonschema:"the absolute vault path read"`
	Content    string `json:"content" jsonschema:"the requested lines, verbatim — exactly the bytes in the file, safe to pass back to edit_doc"`
	StartLine  int    `json:"start_line" jsonschema:"1-based line of the first returned line"`
	EndLine    int    `json:"end_line" jsonschema:"1-based line of the last returned line"`
	TotalLines int    `json:"total_lines" jsonschema:"total number of lines in the document"`
}

func (s *vaultService) readDoc(_ context.Context, _ *mcp.CallToolRequest, in readDocInput) (*mcp.CallToolResult, readDocOutput, error) {
	name, err := vaultName(in.Path)
	if err != nil {
		return nil, readDocOutput{}, err
	}
	b, err := s.rootFS.ReadFile(name)
	if err != nil {
		return nil, readDocOutput{}, err
	}
	lines := strings.Split(string(b), "\n")
	// A trailing newline yields a final empty element; drop it so line counts
	// match what an editor shows. Remember whether it was there so the slice we
	// return can be reassembled byte-for-byte.
	trailingNL := false
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
		trailingNL = true
	}
	total := len(lines)

	start := in.Offset
	if start <= 0 {
		start = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = readDocDefaultLimit
	}
	if start > total {
		start = total + 1 // nothing to return
	}
	end := start + limit - 1
	if end > total {
		end = total
	}

	// Verbatim: the returned text is exactly the bytes on disk for [start,end],
	// so it can be passed straight back as edit_doc's old_string or spliced into
	// write_doc without any un-prefixing. Position is reported out-of-band in
	// StartLine/EndLine/TotalLines — never mixed into the content, which would
	// make every read a corrupting round trip.
	var content string
	if start <= end {
		content = strings.Join(lines[start-1:end], "\n")
		// Re-attach the newline that Split consumed: there is one after the last
		// returned line whenever another line follows it, or when the file itself
		// ended with a newline.
		if end < total || trailingNL {
			content += "\n"
		}
	}
	out := readDocOutput{Path: in.Path, Content: content, StartLine: start, EndLine: end, TotalLines: total}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: content}}}, out, nil
}

type writeDocInput struct {
	Path    string `json:"path" jsonschema:"absolute vault path of the .md document, e.g. /Research/2026-07-14-topic.md"`
	Content string `json:"content" jsonschema:"markdown content: the whole document, or the fragment to add when append is true"`
	Append  bool   `json:"append,omitempty" jsonschema:"append content to the end of the document instead of overwriting it, creating it if absent"`
}
type writeDocOutput struct {
	Path string `json:"path" jsonschema:"the absolute vault path written"`
}

func (s *vaultService) writeDoc(_ context.Context, _ *mcp.CallToolRequest, in writeDocInput) (*mcp.CallToolResult, writeDocOutput, error) {
	name, err := vaultName(in.Path)
	if err != nil {
		return nil, writeDocOutput{}, err
	}
	if dir := filepath.Dir(name); dir != "." {
		if err := s.rootFS.MkdirAll(dir, 0o755); err != nil {
			return nil, writeDocOutput{}, err
		}
	}
	verb := "Wrote"
	if in.Append {
		// Append exists so growing a document (a running log, a note you add to)
		// doesn't require reading the whole thing back and rewriting it — a
		// round trip that costs context and risks clobbering concurrent edits.
		if err := s.appendFile(name, in.Content); err != nil {
			return nil, writeDocOutput{}, err
		}
		verb = "Appended to"
	} else if err := s.rootFS.WriteFile(name, []byte(in.Content), 0o644); err != nil {
		return nil, writeDocOutput{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("%s %s (searchable shortly)", verb, in.Path),
	}}}, writeDocOutput{Path: in.Path}, nil
}

// appendFile adds content to the end of name, creating it if absent. It inserts
// a newline first when the existing file does not end with one, so an append
// never silently glues itself onto the last line.
func (s *vaultService) appendFile(name, content string) error {
	f, err := s.rootFS.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if end > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], end-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			content = "\n" + content
		}
	}
	_, err = f.Write([]byte(content))
	return err
}

type editDocInput struct {
	Path       string `json:"path" jsonschema:"absolute vault path of the .md document to edit"`
	OldString  string `json:"old_string" jsonschema:"exact text to replace (must be unique unless replace_all)"`
	NewString  string `json:"new_string" jsonschema:"text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match"`
}
type editDocOutput struct {
	Path         string `json:"path" jsonschema:"the absolute vault path edited"`
	Replacements int    `json:"replacements" jsonschema:"number of occurrences replaced"`
}

func (s *vaultService) editDoc(_ context.Context, _ *mcp.CallToolRequest, in editDocInput) (*mcp.CallToolResult, editDocOutput, error) {
	name, err := vaultName(in.Path)
	if err != nil {
		return nil, editDocOutput{}, err
	}
	if in.OldString == "" {
		return nil, editDocOutput{}, fmt.Errorf("old_string is required")
	}
	if in.OldString == in.NewString {
		return nil, editDocOutput{}, fmt.Errorf("old_string and new_string are identical")
	}
	b, err := s.rootFS.ReadFile(name)
	if err != nil {
		return nil, editDocOutput{}, err
	}
	text := string(b)
	n := strings.Count(text, in.OldString)
	if n == 0 {
		return nil, editDocOutput{}, fmt.Errorf("old_string not found in %s", in.Path)
	}
	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(text, in.OldString, in.NewString)
	} else {
		if n > 1 {
			return nil, editDocOutput{}, fmt.Errorf("old_string is not unique in %s (%d matches); make it more specific or set replace_all", in.Path, n)
		}
		updated = strings.Replace(text, in.OldString, in.NewString, 1)
		n = 1
	}
	if err := s.rootFS.WriteFile(name, []byte(updated), 0o644); err != nil {
		return nil, editDocOutput{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("Edited %s (%d replacement(s))", in.Path, n),
	}}}, editDocOutput{Path: in.Path, Replacements: n}, nil
}

type deleteDocInput struct {
	Path string `json:"path" jsonschema:"absolute vault path of the document to delete"`
}
type deleteDocOutput struct {
	Path string `json:"path" jsonschema:"the absolute vault path deleted"`
}

func (s *vaultService) deleteDoc(_ context.Context, _ *mcp.CallToolRequest, in deleteDocInput) (*mcp.CallToolResult, deleteDocOutput, error) {
	name, err := vaultName(in.Path)
	if err != nil {
		return nil, deleteDocOutput{}, err
	}
	if err := s.rootFS.Remove(name); err != nil {
		return nil, deleteDocOutput{}, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: fmt.Sprintf("Deleted %s", in.Path),
	}}}, deleteDocOutput{Path: in.Path}, nil
}

// vaultName validates a caller-supplied vault path and returns the name to pass
// to the confined os.Root. Paths are absolute within the vault ("/" = the vault
// root); os.Root is the actual guard against escaping the tree (via "..", an
// absolute host path, or a symlink pointing out). On top of that safety this
// enforces policy — only .md files are addressable and hidden components
// (.obsidian, .git, …) are refused — so the tools match what the indexer indexes
// and cannot touch dotfile config trees.
//
// This is a trusted small-team tool: os.Root notes it does not fully defend
// against TOCTOU races on some platforms, which we accept here.
func vaultName(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path must be absolute within the vault (start with '/'): %q", p)
	}
	// POSIX cleaning on the logical vault path: for an absolute path filepath's
	// Clean clamps any ".." at the root, so it cannot express a location outside
	// the vault; os.Root re-checks regardless.
	clean := path.Clean(p)
	name := strings.TrimPrefix(clean, "/")
	if name == "" || name == "." {
		return "", fmt.Errorf("path must name a file, not the vault root: %q", p)
	}
	if !corpus.IsIndexable(name) {
		return "", fmt.Errorf("only .md files are addressable: %q", p)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return "", fmt.Errorf("hidden paths are not addressable: %q", p)
		}
	}
	return filepath.FromSlash(name), nil
}
