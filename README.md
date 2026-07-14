# attndb

A per-token, late-interaction (ColBERT-style) semantic search database for
technical/business documents. Instead of one diluted vector per passage, attndb
stores **per-token contextual embeddings** and retrieves with **late-interaction
(MaxSim)** scoring, so matches localize to the exact span — while cheap pooled
single-vectors cover diffuse/topical queries.

## Status

Vertical slice: the full pipeline runs end-to-end in pure Go on in-memory
storage with **stub encoders** (deterministic hash embeddings — they prove the
plumbing and localization, not semantic quality). Real ONNX encoders and a
Qdrant backend implement the same interfaces and drop in unchanged.

```
go run ./cmd/attndb -k 3 "retry backoff when the identity service is unavailable"
```

## Architecture: composable passes

The unit of composition is a **pass** = `Chunker × Encoder × Pool × Role`. All
passes anchor to a shared `(doc_id, byte-span)` coordinate, so passes that chunk
differently still merge cleanly.

- **Chunker** (`internal/chunk`) — `BySection`, `ByParagraph`, `WholeDoc`,
  `FixedWindow`, plus the `ChunkerFunc` adapter.
- **Encoder** (`internal/core`) — `SingleVectorEncoder` (one vector/chunk) or
  `MultiVectorEncoder` (per-token vectors + offsets). Stubs in `internal/encode`.
- **Pool** (`internal/store`) — vector storage behind one interface; `Memory`
  now, Qdrant later.
- **Pass** (`internal/pass`) — `NewSingleVectorPass` / `NewPerTokenPass`, with a
  `WithWeight` option. Each pass just emits scored intervals ("deposits").
- **Heat** (`internal/heat`) — accumulates deposits into per-document peaks.

A `DB` (`internal/db`) composes passes. `Search`:
1. **deposit** — every pass returns scored intervals for the query (per-token
   passes refine each to their best-matching MaxSim sub-span);
2. **normalize** — per pass, per query, onto a common 0–1 scale (default
   **min-max**, which restores contrast for MaxSim's high, narrow score band;
   `Rank` is the robust fallback);
3. **accumulate** — each deposit adds its weight *flat* across its span onto a
   per-document heatmap; a broad match is a low uniform prior, precise matches
   stack on top into peaks;
4. **extract** — non-max suppression pulls the tallest non-overlapping peaks.

Width is handled continuously — no "is this a localizing or a scope pass?"
categories. Adding a level = adding a pass; changing context length = swapping
the chunker; down-weighting a near-clone pass = `WithWeight`.

Default config (`cmd/attndb`): `tok-section` (per-token, section-aligned) +
`para` (paragraph single-vector) + `doc` (whole-doc single-vector).

## Running with Qdrant

```
docker compose up -d                      # starts Qdrant (gRPC :6334, REST :6333)
go run ./cmd/attndb -store qdrant -k 3 "retry backoff when the service is unavailable"
```

`-store memory` (default) needs no server and is what the tests use.

### Calibration & the relevance gate

`ingest` also **calibrates** each pass's score range (a per-corpus noise floor and
strong-match level, sampled from pseudo-queries) and writes it to
`.attndb-calibration.json`. `search` loads it and scales scores with a fixed
affine map instead of per-query min-max — so a query with no real answer scores
*low* instead of being stretched to look confident. Add `-min N` to drop results
below a score (the "no confident match" gate):

```
attndb search -store qdrant -encoder onnx -docs corpus/ -min 1.0 "your question"
```

## Real encoders (ONNX)

The default build uses deterministic *stub* encoders (lexical, for testing the
pipeline). The real semantic encoders live behind the `onnx` build tag because
they link native libraries (ONNX Runtime + HF tokenizers). One-time setup:

1. **Export the models** (the only Python in the project — runs offline, in an
   isolated pipenv env so it never touches your global interpreter):
   ```
   pipenv install     # uses the Pipfile (colbert-export, onnxscript)
   pipenv run python -c "from colbert_export import export_model; export_model('lightonai/GTE-ModernColBERT-v1', output_dir='models', quantize=True)"
   pipenv run python scripts/export_single.py     # gte-modernbert-base -> models/single/
   ```
   This produces `models/` (per-token ColBERT) and `models/single/` (single-vector),
   each with `model.onnx`, `model.onnx.data`, and `tokenizer.json`.

2. **Fetch the tokenizer lib** into `libs/`:
   - **Linux (amd64)**:
     ```
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz | tar -xz -C libs
     ```
   - **macOS (Apple Silicon)**:
     ```
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.darwin-arm64.tar.gz | tar -xz -C libs
     ```

3. **Build and run** (ONNX Runtime is loaded at runtime from the path in
   `internal/encode/onnx`, overridable via `ATTNDB_ORT_LIB`):
   ```
   CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx -o attndb ./cmd/attndb
   ./attndb -encoder onnx -store memory -k 3 "how long do we keep customer information"
   ```

`models/` (~1.4 GB) and `libs/` (~50 MB) are build artifacts and are not committed.

## Metal/CoreML acceleration (macOS)

The ONNX Runtime CoreML Execution Provider runs ops on the Metal GPU. It is
built into the standard macOS ORT dylib (from pip or Homebrew).

```
# point at the ORT dylib; pip-installed ORT is the easiest source
export ATTNDB_ORT_LIB=$(python3 -c \
  "import onnxruntime, pathlib; \
   print(next(pathlib.Path(onnxruntime.__file__).parent.glob('capi/libonnxruntime*.dylib')))")

# one-shot ingest + search on the Metal GPU
./attndb query -encoder onnx -provider coreml -k 3 "retry backoff when the service is unavailable"

# benchmark: compare CPU vs CoreML on the same corpus
time ./attndb query -encoder onnx -provider cpu    -k 3 "..."
time ./attndb query -encoder onnx -provider coreml -k 3 "..."
```

`-provider coreml` enables `MLComputeUnits=CPUAndGPU` (Metal GPU + CPU fallback
for any ops CoreML can't handle). It also enables ORT verbose logging so node-to-EP
assignments appear on stderr — look for `Node ... assigned to CoreMLExecutionProvider`
to confirm ops actually ran on Metal. If CoreML is not compiled into the dylib the
flag errors loud rather than silently falling back.

Inputs are padded to fixed shapes (ColBERT: 48/300 tokens; single-vector: 512 tokens)
so CoreML can compile a static graph. Without static shapes the CoreML EP falls back
to CPU for transformer ops.

## Live index + MCP server (`serve`)

`attndb serve` is a long-running daemon that keeps a directory tree indexed as
its files change and answers searches over MCP (Streamable HTTP). It builds the
DB once so the encoders stay resident — a changed file re-indexes in
sub-seconds, and each query is tens of ms.

```
./attndb serve -store qdrant -encoder onnx -provider cpu -ns vault \
  -docs "/path/to/vault" -calib .attndb-vault-calibration.json -addr localhost:8765
```

- **Watch + reconcile.** A file watcher (FSEvents on macOS, polling elsewhere)
  drives a reconciler that diffs the tree against the index: adds/edits →
  delete-then-ingest (point IDs are span-derived, so a bare re-ingest would
  orphan old spans), deletes → drop. A periodic `-resync` ReconcileAll backstops
  bulk/directory operations the granular watcher can miss. Change detection uses
  an `(mtime, sha)` stamp stored in each point's payload, so restarts don't
  re-ingest unchanged files.
- **Calibration** is refreshed lazily as the corpus drifts (idle debounce +
  churn ceiling), recomputed in the background and swapped in atomically.
- **Tool:** `search_vault(query, k?, min?)` → ranked `{path, span, score,
  snippet}`; snippets are read fresh from disk. `min` exposes the calibrated
  relevance gate so a consumer can tell "no confident match" from "weak hits".

Design notes: `docs/live-vault-index.md`. Namespaces (`-ns`) isolate independent
indexes in one Qdrant (e.g. a vault vs. the sample corpus).

## Guiding an LLM to use the vault

The `search_vault` tool carries enough description to be usable by any MCP client.
Two optional layers make an LLM reach for it at the right moments:

**1. The `attndb-search` skill (Claude Code).** `skills/attndb-search/SKILL.md`
is a Claude Code skill that gets auto-surfaced when a question might be covered by
the vault. It encodes when to search, how to phrase queries, how to read the
calibrated scores ("a low top score means no confident match"), and to cite the
source path. Install it by copying or symlinking it into your skills dir:

```
ln -s "$PWD/skills/attndb-search" ~/.claude/skills/attndb-search
```

**2. A search-first directive.** Add this to your global instructions (for Claude
Code, `~/.claude/CLAUDE.md`) so the model checks the vault before researching a
topic from scratch — the point being to reuse prior work instead of redoing it:

```markdown
## Search my vault before researching
Before researching any topic from scratch, first call the attndb `search_vault`
tool and use any confident match (cite the path). If nothing relevant comes back,
proceed normally. Search only for now — do not auto-write documents to the vault;
that capability comes later.
```

(The "search only" clause is temporary: an autonomous *capture* loop — write
durable research back into the vault — is a planned follow-on; see
`docs/proposal-knowledge-capture.md`.)

## Roadmap

- [x] Pure-Go pass skeleton: chunkers, heatmap accumulator, DB, in-memory pool, stub encoders
- [x] Unit tests (heat / chunk / vec / pass / db end-to-end)
- [x] docker-compose for Qdrant
- [x] Qdrant `Pool` (Go client; native multivector MaxSim + payload filtering)
- [x] Real ONNX encoders: GTE-ModernColBERT (per-token) + gte-modernbert-base (single-vector)
- [x] Mac/Metal acceleration spike: CoreML Execution Provider via ONNX Runtime (`-provider coreml`)
- [x] Namespaced collections (`-ns`) so independent corpora share one Qdrant
- [x] Recursive ingest (subdirectories; relative-path doc IDs)
- [x] Live index daemon: file watcher + reconciler + incremental (re-)index, lazy recalibration
- [x] `search_vault` over MCP (Streamable HTTP), calibrated relevance gate
- [ ] Encoder batching (currently one forward pass per text)
- [ ] Token-level sub-span refinement over Qdrant (store offsets in payload, read vectors back)
- [ ] Eval / bake-off harness sweeping pass configurations
