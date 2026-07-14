# Proposal: distribution & install (public open-source)

**Goal:** a stranger can go from "found the repo" to "searching my docs" in a few
minutes, on macOS or Linux, without a C toolchain, without Python, and without
hand-fetching native libraries or 1 GB of model weights.

Status: proposal. Tags below: **[decision]** needs your call, **[eval]** needs
measurement, **[work]** is just implementation.

## The friction today

Everything real about attndb lives behind the `onnx` build tag, which drags in
native dependencies. A new user currently has to:

1. Install a C toolchain and build with CGO (`CGO_LDFLAGS=-L./libs go build -tags onnx`).
2. Fetch the **ONNX Runtime dylib** (per-OS/arch) — loaded at runtime via
   `ATTNDB_ORT_LIB`.
3. Fetch/build **libtokenizers** (per-platform static lib, linked at build).
4. Run a **Python/pipenv** export step to produce ~1.15 GB of ONNX weights
   (`models/` ColBERT ≈ 571 MB + `models/single/` ≈ 575 MB).
5. Bring up **Qdrant** via Docker.

That's five obstacles, three of them platform-specific. Steps 1–4 are the ones
that stop a casual user cold.

## What "installable" requires

- **A prebuilt binary** per platform — the user never compiles.
- **Native libs bundled** with that binary — no manual dylib/lib fetching.
- **Models fetched on demand** to a cache — not committed, not built locally.
- **A documented, one-command datastore** — Qdrant without ceremony.
- **A first-run that guides**, not one that errors on a missing dep.

## Approach

### 1. Prebuilt release binaries (CI matrix) — [work]

CGO cross-compilation for this stack (ORT + tokenizers) is painful and fragile.
Sidestep it: **build natively on each target** in CI (GitHub Actions
`macos-14` arm64, `macos-13` amd64, `ubuntu` amd64), statically linking
tokenizers, and publish archives to GitHub Releases. Each archive contains the
`attndb` binary + the matching ONNX Runtime shared library, laid out so the
binary resolves the lib **relative to itself** (fall back to `ATTNDB_ORT_LIB`
when set). Result: download, unpack, run — no build, no `-L`, no env var.

### 2. Models fetched on first run — DECIDED

Add `attndb pull` (and an implicit prompt on first `serve`/`query` when models
are absent) that downloads the encoder weights to an XDG cache
(`~/.cache/attndb/models`), verified by checksum. Not committed, not per-user
rebuilt.

**Decision: publish our ONNX export as GitHub Release assets; `attndb pull`
fetches from there.** No self-hosted or third-party hosting to maintain — GitHub
Releases is the CDN.

- **Licensing is clear.** Both source models are **Apache 2.0**
  (`lightonai/GTE-ModernColBERT-v1`, `Alibaba-NLP/gte-modernbert-base`), which
  permits redistributing our ONNX derivative. Obligation: ship the upstream
  `LICENSE`/`NOTICE` with the assets, attribute, and state that we exported/
  modified. [work]
- **Mechanics.** Attach the exported weights to a dedicated, versioned release
  tag (e.g. `models-v1`) so model assets version independently of code releases.
  Asset sizes fit comfortably (ColBERT `.data` ≈ 569 MB, `single/` ≈ 575 MB;
  GitHub's per-file limit is 2 GB). `attndb pull` resolves the release for its
  build, downloads, checksums, and caches. [work]
- The pipenv export script stays in-repo as the **producer** of those assets
  (maintainer-run at release time), not something users touch.

### 3. Smaller/faster mode via int8 — [eval], not a default

A 145 MB int8 ColBERT export already exists (`model_int8.onnx`) but isn't wired.
Wiring int8 for both encoders would cut the download ~1.15 GB → ~300 MB. **But
the 10/10 retrieval result was measured on the fp model; int8 embeddings are
lossy and the quality delta is unmeasured.** Ship **fp as the default**, offer
int8 as an opt-in `--quantized` "smaller/faster" mode, and gate making it the
default behind a retrieval eval. That eval is the dormant roadmap item
("eval / bake-off harness") — worth doing anyway.

### 4. Datastore without ceremony — DECIDED: Qdrant-only for v1

Ship the existing `docker-compose.yml` and make it the documented path
(`docker compose up -d` → `attndb serve`). **Decision: Qdrant is the sole
supported store for v1** — keep the surface small. Revisit an optional
zero-dependency embedded store (pure-Go persistent `Pool`, brute-force, no
Docker) only if the Docker requirement turns out to be the actual adoption
blocker.

### 5. Packaging surface — [work]

- **Install script** (`curl … | sh`) that grabs the right release archive and
  drops the binary + lib on PATH. The 80% path.
- **Homebrew tap** (`brew install kjn/tap/attndb`) — formula installs binary +
  lib; runs `attndb pull` post-install. `go install` won't work (CGO + bundled
  lib), so document that it's intentionally not supported.
- **`attndb doctor`** — checks lib resolution, model presence, Qdrant
  reachability; prints exactly what's missing. Turns "it errored" into "here's
  the one thing to fix."

## Recommended path & phasing

- **Phase 1 (makes it installable):** CI release binaries with bundled ORT lib;
  `attndb pull` fetching ONNX weights from `models-v1` GitHub Release assets
  (with upstream LICENSE/NOTICE); `attndb doctor`.
- **Phase 2 (makes it pleasant):** install script + Homebrew tap; first-run
  auto-pull.
- **Phase 3 (later):** int8 opt-in mode once [eval] clears it as
  default-worthy. (Embedded store deferred unless Docker proves to be the
  adoption blocker.)

## Decisions made
- **Model delivery:** publish our ONNX export as GitHub Release assets (§2);
  both source models are Apache 2.0, so redistribution is permitted.
- **Datastore:** Qdrant-only for v1 (§4); embedded store deferred.

## Open items
- **[eval]** int8 retrieval quality vs fp (gates int8-as-default).
- **[work]** CI build matrix, ORT-lib bundling, `attndb pull`, `attndb doctor`,
  install script, Homebrew tap, attach model assets + LICENSE/NOTICE to a
  `models-v1` release.
