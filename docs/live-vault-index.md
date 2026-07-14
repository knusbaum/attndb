# Live vault index: file watcher + MCP server

Design sketch for turning attndb into a long-running service that keeps a
directory tree (e.g. an Obsidian vault) indexed as files change, and answers
searches over MCP.

Status: **implemented** (`attndb serve`). This doc is the design of record;
the build order at the bottom is done. Verified end-to-end: start the daemon,
search over MCP, add/edit/delete files live, confirm the index tracks.

## Implemented layout

- `internal/corpus` — `LoadDir` / `LoadDoc`, ignore rules, `(doc_id, mtime, sha)`
  stamping. Shared by the CLI ingest and the reconciler.
- `internal/store` — `Pool.Delete` (delete-by-`doc_id` filter, `Wait=true`) and
  `Pool.Stamps` (scroll → per-doc stamp). Qdrant + Memory + test fake.
- `internal/db` — `DB.Delete`, `DB.Stamps` (via the "doc" pass), and an
  `RWMutex` around the calibration swap.
- `internal/reconcile` — `Reconcile(paths)` (granular) + `ReconcileAll` (full
  diff), one delete-then-ingest core, single-writer lock, in-memory stamp cache.
- `internal/watch` — `Watcher` interface; `Poller` (all platforms, emits nil →
  `ReconcileAll`) and darwin FSEvents (`native_darwin.go`, emits changed paths).
- `cmd/attndb serve` — builds the DB once (resident encoders), startup reconcile,
  watch loop with the calibration policy, MCP `search_vault` over Streamable HTTP
  (`modelcontextprotocol/go-sdk`).

Run: `./attndb serve -store qdrant -encoder onnx -provider cpu -ns vault -docs <dir> -calib .attndb-vault-calibration.json -addr localhost:8765`

### Gotcha found in testing: canonicalize the root

FSEvents reports symlink-resolved absolute paths (`/private/var/…` for `/var/…`
on macOS). If the reconciler's root isn't canonicalized to match, `filepath.Rel`
produces bogus `../../` doc IDs and live updates/deletes fail to line up with the
startup-indexed IDs. `serve` runs `filepath.EvalSymlinks` + `filepath.Abs` on the
root before wiring the watcher/reconciler. (Won't bite under `~/Documents`, which
has no symlink component, but the fix is correct regardless.)

### Calibration policy

Recalibration is lazy and never per-edit. Triggers, in priority order:

- **Startup:** recalibrate only if ≥1 doc changed (Calibrate is deterministic,
  so an unchanged corpus reproduces identical values).
- **Churn ceiling** — `dirty ≥ max(20 docs, 10%)`: recalibrate immediately (a
  big change likely moved the score distribution).
- **Idle debounce with a floor** (`-recal-idle` 2m, `-recal-floor` 5): once edits
  settle, recalibrate only if at least 5 docs changed. A single edit does *not*
  trigger it.
- **Staleness cap** (`-recal-max-stale` 1h): fold any lingering sub-floor changes
  in at most once an hour, so slow trickle edits never leave calibration stale.

Recomputed in the background off a doc-text snapshot (no index reads), swapped in
atomically, persisted to the calib file. At most one runs at a time.

## Shape: one binary

The MCP server *hosts* the file watcher. They are one process for one reason:
the ColBERT + single-vector ONNX encoders load once and stay resident, so

- re-indexing a changed file is **sub-second** (no model load), and
- every search is **tens of ms** (query encode + Qdrant lookup).

The CLI, by contrast, pays full model load per invocation (~0.66s cold, of
which the query itself is a small fraction). Resident encoders are what make
incremental indexing cheap enough to run on every file save.

Measured baseline (129-doc vault, ~968 KB, CPU provider): full ingest 8m7s
(one forward pass per chunk — encoder batching is unimplemented), calibration
+10s, cold search 0.66s, top-5 recall 10/10 on known-target queries.

## 0. Hard prerequisite: `Pool.Delete`

Everything else depends on this. Point IDs are `hashID("pass:doc_id:start-end")`
— deterministic per *span*. When a doc's text shifts, its span boundaries move,
so a re-ingest writes **new** points and **orphans the old ones** (their IDs
never regenerate, so upsert can't overwrite them). Every update must be
delete-then-ingest.

```go
// internal/store: add to the Pool interface
Delete(ctx context.Context, docID string) error
```

- **Qdrant:** `client.Delete` with a filter matching the `doc_id` payload field
  (already present on every point). No payload index needed at this scale; add
  one only if you also want doc-scoped *search* filters.
- **Memory:** drop records where `DocID == docID`.
- **fakePool** (`internal/pass/deposits_test.go`): no-op stub to satisfy the
  interface.

DB level:

```go
func (d *DB) Delete(ctx context.Context, docIDs []string) error // fan out to every pass's pool
// update == Delete(id) then Ingest([]core.Document{doc})
```

## 1. Change detection: stamp identity into the index

Add `mtime` (and optionally a content `sha`) to each doc. The mechanism already
exists — `payload()` copies `Document.Meta`, so `loadDocs` just sets
`Meta["mtime"]` / `Meta["sha"]`.

The **doc-level collection** (`attndb_vault_doc_onnx`, exactly 1 point/doc) is
the index-of-record. Scroll it for `{doc_id, mtime, sha}`, diff against a
filesystem walk. Two-tier: mtime as the fast path, hash only when mtime differs.

**This is what avoids re-ingesting the whole tree on every boot.**

## 2. Watcher = debounced `reconcile(paths)` — no event-type dispatch

The OS hands us *coalesced changed paths*, not clean create/write/rename/delete
semantics. So don't map event kinds to actions. One function:

```go
func (r *Reconciler) reconcile(ctx context.Context, paths []string) error {
    for _, p := range paths {
        switch fi, err := os.Stat(p); {
        case os.IsNotExist(err):        // gone → delete
            r.db.Delete(ctx, []string{r.docID(p)})
        case err == nil && r.changed(fi, p): // exists & mtime/hash differ → delete-then-ingest
            r.db.Delete(ctx, []string{r.docID(p)})
            r.db.Ingest(ctx, []core.Document{r.load(p)})
        }
        // unchanged → skip
    }
}
```

- **Rename falls out for free:** old path stat-misses (delete), new path appears
  (ingest). doc_id is the relative path, so a rename is genuinely a
  delete+insert.
- **Startup reconciliation is the same function** over the whole tree — heals
  anything that changed while the watcher was down.
- **Same ignore rules as ingest** (skip dotdirs, non-`.md`). Obsidian churns
  `.obsidian/` and swap files constantly.
- **Debounce** per-path ~500ms–1s: editors write in bursts (temp-file + rename,
  multiple saves).
- **Single writer goroutine** drains the debounced channel so ingests/deletes
  serialize (encoder not assumed concurrent-safe; keeps recalibration off the
  write path). Qdrant handles concurrent read (search) vs write server-side.

### Watcher portability (decision: interface + fallbacks)

```go
type Watcher interface {
    Events() <-chan []string // batches of changed absolute paths
    Close() error
}
```

- **darwin:** FSEvents impl (`github.com/fsnotify/fsevents`) — recursive and
  coalescing. Go's plain `fsnotify` uses kqueue on darwin: no recursion,
  per-FD limits, misses new subdirs — not suitable for a vault.
- **other:** polling walk every N seconds (diff mtimes against the index), or
  kqueue/inotify where it fits. Polling is simple and correct; it just latency-
  bounds change pickup to the poll interval.

Both feed the same `reconcile(paths)`.

## 3. Calibration: lazy, triggered off the same change detection

Calibration is a per-corpus affine map; small drift is harmless, so never
recalibrate per-change. It reads only doc text + 150 pseudo-queries (~10s),
never touches the index — safe to run in the background concurrent with search
and reconcile.

- **Startup:** reconcile first, then recalibrate **only if ≥1 doc changed**.
  `Calibrate` is deterministic (`rand.NewSource(1)` + fixed noise queries), so
  an unchanged corpus reproduces identical values — skip it.
- **Runtime:** mark dirty on any changed doc; recalibrate when dirty **AND**
  (vault idle N min **OR** cumulative churn > `max(20 docs, 10%)`). Idle-debounce
  bounds staleness to "a few minutes after your last edit" and batches bursts.
- Always compute off a snapshot in the background, then **atomic-swap** the
  installed calibration, and persist to `.attndb-vault-calibration.json`.

**Data race to fix first:** `SetCalibration` mutates `d.passNorm` with no lock
while `Search`→`normalizerFor` reads it. Guard with an `RWMutex` (or swap the
whole map via `atomic.Pointer[map...]`) before doing this live.

## 4. MCP server (decision: HTTP/SSE always-on daemon)

- **Transport:** long-lived localhost HTTP server with SSE — always-on, shared
  across clients, not spawned per-connection. (SDK choice deferred; needs to
  support the HTTP/SSE server transport.)
- **Startup:** build the DB once (encoders resident) → connect Qdrant → load
  calibration → `SetCalibration` → launch reconciler goroutine → startup
  reconcile.
- **One tool:**

  ```
  search_vault(query: string, k?: int = 5, min?: float = <calibrated default>)
    → [{ path, span:{start,end}, score, snippet }]
  ```

- **Snippets read on demand** from the file at `doc_id` (== relative path),
  sliced by span — always fresh, no cache to invalidate. **Clamp span bounds**
  to `len(text)`: a file edited inside the debounce window has shifted bytes
  until re-ingest.
- **Expose the relevance gate** (`min`) as a param with a calibrated default —
  lets the LLM consumer distinguish "no confident match" from "weak hits."
- **Serialize query encoding behind a mutex** unless the ORT Go wrapper is
  confirmed `Run`-safe. Contention is irrelevant at single-user scale.

## Known rough edge (accepted)

Update = delete + ingest is two steps, so a crash mid-update leaves that one doc
unsearchable until the next reconcile self-heals it. Acceptable for a
single-user tool — not worth span-level diffing.

## Build order

1. `Pool.Delete` (+ DB.Delete) across Qdrant / Memory / fakePool. **Blocks
   everything.**
2. mtime/sha stamping in `loadDocs` + doc-collection scroll for the index-of-record.
3. `RWMutex`/atomic around calibration swap.
4. `Watcher` interface + darwin FSEvents + polling fallback → `reconcile`.
5. Calibration trigger policy (startup-if-changed; runtime idle/churn debounce).
6. HTTP/SSE MCP server wrapping resident DB + `search_vault`.
</content>
</invoke>
