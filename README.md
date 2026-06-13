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
   ```
   curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz | tar -xz -C libs
   ```

3. **Build and run** (ONNX Runtime is loaded at runtime from the path in
   `internal/encode/onnx`, overridable via `ATTNDB_ORT_LIB`):
   ```
   CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx -o attndb ./cmd/attndb
   ./attndb -encoder onnx -store memory -k 3 "how long do we keep customer information"
   ```

`models/` (~1.4 GB) and `libs/` (~50 MB) are build artifacts and are not committed.

## Roadmap

- [x] Pure-Go pass skeleton: chunkers, heatmap accumulator, DB, in-memory pool, stub encoders
- [x] Unit tests (heat / chunk / vec / pass / db end-to-end)
- [x] docker-compose for Qdrant
- [x] Qdrant `Pool` (Go client; native multivector MaxSim + payload filtering)
- [x] Real ONNX encoders: GTE-ModernColBERT (per-token) + gte-modernbert-base (single-vector)
- [ ] AMD GPU acceleration spike: Vulkan (llama.cpp) vs ROCm (ONNX Runtime)
- [ ] Encoder batching (currently one forward pass per text)
- [ ] Token-level sub-span refinement over Qdrant (store offsets in payload, read vectors back)
- [ ] Eval / bake-off harness sweeping pass configurations
