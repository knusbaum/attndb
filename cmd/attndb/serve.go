package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kjn/attndb/internal/core"
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
	ctx := context.Background()

	database, err := c.database()
	if err != nil {
		return err
	}
	if calib, err := loadCalibration(*c.calib, *c.encoder); err != nil {
		return err
	} else if calib != nil {
		database.SetCalibration(calib)
	}

	rec := reconcile.New(root, database)
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

	server := mcp.NewServer(&mcp.Implementation{Name: "attndb", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_vault",
		Description: "Search the indexed document vault with late-interaction semantic retrieval. " +
			"Returns ranked spans with file path, byte range, calibrated score, and a snippet. " +
			"Scores are calibrated: a low top score means no confident match.",
	}, svc.search)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	log.Printf("attndb MCP server listening on http://%s (ns=%q, docs=%s)", *addr, *c.ns, root)
	return http.ListenAndServe(*addr, handler)
}

// vaultService bundles the resident DB, reconciler, and calibration policy that
// the watch loop and the MCP tool share.
type vaultService struct {
	db        *db.DB
	rec       *reconcile.Reconciler
	root      string
	calibPath string
	encoder   string
	defMin    float64
	idle      time.Duration
	idleFloor int
	recalSem  chan struct{} // size-1: at most one background recalibration
}

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

// --- search_vault tool ---

type searchInput struct {
	Query string  `json:"query" jsonschema:"the natural-language search query"`
	K     int     `json:"k,omitempty" jsonschema:"maximum number of results (default 5)"`
	Min   float64 `json:"min,omitempty" jsonschema:"drop results scoring below this calibrated threshold; omit to use the server default"`
}

type searchHit struct {
	Path    string  `json:"path" jsonschema:"vault-relative path of the matching document"`
	Start   int     `json:"start" jsonschema:"start byte offset of the matched span"`
	End     int     `json:"end" jsonschema:"end byte offset of the matched span"`
	Score   float64 `json:"score" jsonschema:"calibrated relevance score"`
	Snippet string  `json:"snippet" jsonschema:"text of the matched span"`
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
		sn := s.readSnippet(r.DocID, r.Span)
		hits = append(hits, searchHit{
			Path: r.DocID, Start: r.Span.Start, End: r.Span.End,
			Score: float64(r.Score), Snippet: sn,
		})
		fmt.Fprintf(&b, "%d. [%.3f] %s (bytes %d–%d)\n   %s\n", i+1, r.Score, r.DocID, r.Span.Start, r.Span.End, sn)
	}
	text := b.String()
	if text == "" {
		text = "no confident match"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, searchOutput{Hits: hits}, nil
}

// readSnippet reads the matched span fresh from disk so the text is always
// current. Spans are clamped: a file edited since indexing may have shifted,
// and its stored offsets can run past the current length until re-ingest.
func (s *vaultService) readSnippet(docID string, span core.Span) string {
	b, err := os.ReadFile(filepath.Join(s.root, docID))
	if err != nil {
		return ""
	}
	text := string(b)
	sp := span
	if sp.End > len(text) {
		sp.End = len(text)
	}
	if sp.Start > sp.End {
		sp.Start = sp.End
	}
	return snippet(text, sp)
}
