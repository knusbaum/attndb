# Architecture

How attndb retrieves, for anyone reading the code or extending it. If you just
want to run attndb, see the [README](../README.md) instead.

## Composable passes

The unit of composition is a **pass** = `Chunker × Encoder × Pool × Role`. All
passes anchor to a shared `(doc_id, byte-span)` coordinate, so passes that chunk
differently still merge cleanly.

- **Chunker** (`internal/chunk`) — `BySection`, `ByParagraph`, `WholeDoc`,
  `FixedWindow`, plus the `ChunkerFunc` adapter.
- **Encoder** (`internal/core`) — `SingleVectorEncoder` (one vector/chunk) or
  `MultiVectorEncoder` (per-token vectors + offsets). Stubs in `internal/encode`
  (deterministic hash embeddings — enough to prove the plumbing and
  localization, not semantic quality); real ONNX encoders in
  `internal/encode/onnx` implement the same interfaces and drop in unchanged.
- **Pool** (`internal/store`) — vector storage behind one interface; `Memory`
  for tests, `Qdrant` for anything persistent.
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

## Why per-token, not just pooled vectors

Pooling a passage into one vector is cheap but dilutes: a long section about
five different things gets one blurry average, and a query about any one of
them scores it mediocre-for-everything rather than excellent-for-the-relevant-
part. attndb's `tok-section` pass keeps a vector **per token** instead, and
scores a query with **MaxSim** (late interaction): each query token finds its
single best-matching document token, and the scores sum. A match localizes to
the exact span that earned it, not the whole section it came from.

The pooled passes (`para`, `doc`) still matter — they catch diffuse, topical
queries a narrow per-token match would miss — which is the whole reason to run
both and let the heatmap decide where precision beats coverage.

## Calibration & the relevance gate

A raw cosine or MaxSim score means nothing on its own — it needs a per-pass
scale to compare against. `ingest` (and the `serve` daemon, lazily as the corpus
drifts) samples the corpus for pseudo-queries and calibrates each pass's score
range: a noise floor (`Lo`, anchored on *unrelated* queries) and a strong-match
level (`Hi`, anchored on in-corpus positives). Search then maps a raw score
through `(s - Lo) / (Hi - Lo)`, clamped to `[0,1]`, instead of per-query min-max
— so a query with no real answer in the corpus scores low instead of being
stretched to look confident. `-min` (CLI) / `min` (`search_vault`) exposes this
as a gate: drop anything below a threshold rather than return a plausible-
looking weak match. See `internal/db/db.go`'s `Calibrate` for the exact anchoring
logic.

## The live daemon

`attndb serve` keeps a directory tree indexed as it changes and serves search
over MCP — file watcher, reconciler, and the lazy recalibration policy. Design
notes: [`live-vault-index.md`](live-vault-index.md).

## Roadmap and decisions

Milestone plan: [`build-plan.md`](build-plan.md). Standing design proposals
(multi-user/auth, distribution/packaging, the search-first + capture-after LLM
loop): `proposal-*.md` in this directory.
