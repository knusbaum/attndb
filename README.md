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
docker compose up -d qdrant               # starts Qdrant (gRPC :6334, REST :6333)
go run ./cmd/attndb -store qdrant -k 3 "retry backoff when the service is unavailable"
```

`-store memory` (default) needs no server and is what the tests use. To bring up
the whole thing — Qdrant *and* the document-serving MCP endpoint — see
[Docker](#docker-the-whole-stack) below.

## Docker: the whole stack

`docker compose up -d` runs Qdrant plus the `serve` daemon, giving you an MCP
endpoint on `http://localhost:8765` that indexes a directory and keeps it in
sync. Paths are parameters, so the indexed document tree is configured exactly
the way Qdrant's storage is:

```
cp .env.example .env        # then set ATTNDB_DOCS to your vault
docker compose up -d        # builds on demand, then starts Qdrant + the daemon
docker compose logs -f attndb
```

**No host setup is required** — not the Python export, not the native libs. The
image build pulls the encoders from HuggingFace and converts them to ONNX
itself, so no weights need distributing. That stage costs several minutes and a
few GB of RAM the first time; it is a cached layer afterwards. If you already
exported weights and would rather skip it, uncomment the models bind mount in
`docker-compose.yml`.

| Variable | Default | What it is |
|---|---|---|
| `ATTNDB_DOCS` | `./vault` | the document tree to index and serve (read-write) |
| `ATTNDB_STATE` | `./attndb_state` | generated calibration, persisted across restarts |
| `QDRANT_STORAGE` | `./qdrant_storage` | Qdrant's data directory |
| `ATTNDB_PORT` | `8765` | host port for the MCP endpoint |
| `ATTNDB_NS` | `vault` | collection namespace |

The image is multi-arch (`linux/amd64` + `linux/arm64`), and **no emulation is
needed to build it**. Native libraries are fetched per `TARGETARCH`, and the Go
and Python stages both run on the *build* machine: Go cross-compiles with a
Debian cross toolchain, and the model export emits architecture-independent ONNX.
Nothing foreign is executed during a build — it is only written.

```
# a foreign-arch image builds on the default builder, no qemu required
docker buildx build --platform linux/arm64 -t attndb:arm64 --load .

# both at once needs the container driver — the default `docker` driver
# cannot export a multi-platform manifest list
docker buildx create --use --name attndb-builder
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/attndb:v1 --push .
```

You only need qemu/binfmt to **run** a foreign-arch image locally
(`docker run --platform linux/arm64 …`), never to build one:

```
docker run --privileged --rm tonistiigi/binfmt --install arm64
```

Cross-compiling rather than emulating is a correctness choice as much as a speed
one. Building Go under qemu-user is unreliable — the toolchain drives parallel
`compile` subprocesses through raw clone/futex/signals, and emulating that races:
observed here as every child exiting into a zombie while the parent spun at 100%
CPU, livelocked past 26 minutes, after an identical earlier build had happened to
succeed in 149s. Cross-compiled, the same step takes ~11s.

Once it is up, point your MCP client at `http://localhost:8765` — see
[Connecting a client](#connecting-a-client).

One behavioral difference from running natively: a Linux container has **no
FSEvents**, so change detection falls back to the portable poller
(`ATTNDB_POLL`, default 30s) with `ATTNDB_RESYNC` as the bulk-operation
backstop — edits show up in search within a poll interval rather than instantly.

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

2. **Fetch the native libs** into `libs/` — both the tokenizer static lib
   (linked at build) and the ONNX Runtime shared lib (dlopen'd at run time):
   ```
   mkdir -p libs
   ```
   - **Linux (amd64)**:
     ```
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz | tar -xz -C libs
     curl -sSL https://github.com/microsoft/onnxruntime/releases/download/v1.27.0/onnxruntime-linux-x64-1.27.0.tgz -o /tmp/ort.tgz
     tar -xzf /tmp/ort.tgz -C /tmp
     cp -a /tmp/onnxruntime-linux-x64-1.27.0/lib/libonnxruntime.so* libs/
     ```
   - **macOS (Apple Silicon)**:
     ```
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.darwin-arm64.tar.gz | tar -xz -C libs
     curl -sSL https://github.com/microsoft/onnxruntime/releases/download/v1.27.0/onnxruntime-osx-arm64-1.27.0.tgz -o /tmp/ort.tgz
     tar -xzf /tmp/ort.tgz -C /tmp
     cp -a /tmp/onnxruntime-osx-arm64-1.27.0/lib/libonnxruntime.1.27.0.dylib libs/
     ```
   The CoreML execution provider is only in the macOS build of ONNX Runtime; if
   you want `-provider coreml`, a pip- or Homebrew-installed ORT dylib also works
   (see the Metal section below).

3. **Build and run**:
   ```
   make onnx        # or: CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx -o attndb ./cmd/attndb
   ./attndb query -encoder onnx -store memory -k 3 "how long do we keep customer information"
   ```
   ONNX Runtime is dlopen'd at run time. The default path is resolved per
   platform (`libs/libonnxruntime.so.<version>` on Linux,
   `libs/libonnxruntime.<version>.dylib` on macOS — see
   `internal/encode/onnx/colbert.go`); override it with `ATTNDB_ORT_LIB`, which
   `make` sets for you.

`models/` (~1.4 GB) and `libs/` (~70 MB) are build artifacts and are not committed.
Neither is `Pipfile.lock`: a lock holds one entry per package, but torch differs
by platform on the CPU wheel index (Linux `2.11.0+cpu` vs macOS `2.11.0`), so a
committed lock would break whichever OS it wasn't generated on. `pipenv install`
resolves it per machine — on Linux, torch comes from PyTorch's CPU-only index
(PyPI's Linux wheel is the CUDA build and drags in ~2.5 GB of `nvidia-*`
packages the offline, CPU-only export never uses).

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
- **Tools.** `search_vault(query, k?, min?)` → ranked `{path, start_line,
  end_line, score, snippet}`; snippets are read fresh from disk. `min` exposes
  the calibrated relevance gate so a consumer can tell "no confident match" from
  "weak hits". Alongside it, a filesystem set over the same vault:
  `read_doc(path, offset?, limit?)`, `write_doc(path, content, append?)`,
  `edit_doc(path, old_string, new_string, replace_all?)`, and
  `delete_doc(path)` — all confined to the tree by `os.Root`.
- **Reads are verbatim.** `read_doc` returns the file's bytes and reports the
  line range separately (`start_line`/`end_line`/`total_lines`), so text you read
  can go straight back into `edit_doc` as `old_string`. To grow a document, use
  `write_doc` with `append` rather than a read-modify-write round trip.
- **Read before overwrite, enforced.** Streamable HTTP issues each client an
  `Mcp-Session-Id`, so the server can record which documents a session has read
  and the hash it saw. Overwriting an existing file requires a matching prior
  read — which also catches the case bare read-tracking misses: if the file
  changed on disk in between (a human editing the vault), the write is refused
  instead of silently clobbering. Creating and appending are unrestricted, and
  `edit_doc` already carries its own assertion via `old_string`. A client with no
  session id (stateless transport) is allowed through with a log line — this is a
  footgun guard, not a security boundary.

Design notes: `docs/live-vault-index.md`. Namespaces (`-ns`) isolate independent
indexes in one Qdrant (e.g. a vault vs. the sample corpus).

### Connecting a client

The daemon speaks MCP over **Streamable HTTP** at the root path, so the endpoint
is just the listen address — `http://localhost:8765`, no `/mcp` suffix.

**Claude Code.** Register it once for every project (`--scope user` is the
global one; `local` is private to the current project, `project` is shared via
a committed `.mcp.json`):

```
claude mcp add --transport http attndb http://localhost:8765 --scope user
claude mcp list        # attndb: http://localhost:8765 (HTTP) - ✔ Connected
```

Then `/mcp` inside a session shows the tools, and `claude mcp remove attndb -s
user` undoes it.

**Claude Desktop / other clients.** Point them at the same URL. For clients that
only speak stdio, bridge with `npx mcp-remote http://localhost:8765`.

The server has no auth, so keep it bound to `localhost` (the default outside
Docker). The compose setup publishes the port on the host — do not expose it to
an untrusted network as-is: the write tools (`write_doc`, `edit_doc`,
`delete_doc`) modify the vault. Reverse-proxy TLS + token auth is the M2 item
tracked in `docs/proposal-multi-user.md`.

## Guiding an LLM to use the vault

The tool descriptions are usable by any MCP client. Two optional Claude Code
layers make an LLM reach for the vault at the right moments — to *reuse* prior
work and to *grow* the corpus with what it learns:

**1. Skills (Claude Code).** Drop these into your skills dir; they auto-surface by
description:

```
ln -s "$PWD/skills/attndb-search"           ~/.claude/skills/attndb-search
ln -s "$PWD/skills/attndb-research-capture" ~/.claude/skills/attndb-research-capture
```
- `attndb-search` — query technique: when to search, entity-rich phrasing,
  reading calibrated scores, citing `path:line`.
- `attndb-research-capture` — the ratchet: search the vault before researching,
  and after substantial research write a durable note back into it (dedup via
  `read_doc`/`edit_doc`, `Research/` placement, provenance stamp, notify-after).

**2. A standing directive.** Add this to your global instructions
(`~/.claude/CLAUDE.md`) so the model both reuses and records without being asked:

```markdown
## Vault: search first, capture after
1. Before researching a topic from scratch, call attndb `search_vault` first; on
   a confident match, `read_doc` it and use/extend it rather than redoing the
   work. Cite path:line.
2. After substantial research (multi-source work, a synthesized conclusion, a
   non-obvious answer that took real effort), record it: follow the
   `attndb-research-capture` skill to `write_doc` a durable note into `/Research/`
   (or `edit_doc` an existing one), then tell me one line about what you saved.
   Skip trivia and quick lookups.
```

This is the flagship loop: research either *uses* prior knowledge or *adds* to
it. See `docs/proposal-knowledge-capture.md`.

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
- [x] Docker image + compose: `up` gives a document-serving MCP endpoint (multi-arch amd64/arm64)
- [ ] Encoder batching (currently one forward pass per text)
- [ ] Token-level sub-span refinement over Qdrant (store offsets in payload, read vectors back)
- [ ] Eval / bake-off harness sweeping pass configurations
