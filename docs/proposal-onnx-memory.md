# Proposal: bounding ONNX Runtime memory in the `serve` daemon

**Problem:** the long-running `attndb serve` daemon grows to tens of GiB of RAM
(observed: **68 GiB**, forcing a kill; a later run sat at ~19 GiB) and never
gives it back. The memory is consumed by **large-document encoding** and is not
needed to serve queries, but ONNX Runtime holds onto it for the session's
lifetime.

Status: investigated in depth; a fix is chosen (session recycling) but **one
other option is still to be explored** before implementing (see Open items).
Nothing here is committed yet.

## Symptom & where it comes from

- Steady-state querying is cheap (~1–1.5 GiB). The footprint only balloons when
  the daemon **encodes documents** (ingest / reconcile), and then stays high.
- The whole-doc pass feeds up to `maxSingleLen = 8192` tokens through
  gte-modernbert. Attention is ~O(n²), so **a single 8192-token encode has a
  ~9–17 GiB working set** (measured). That working set becomes the process's
  memory high-water and is never released.
- It is bounded, not infinite: it climbs to a plateau over many encodes (which is
  why prod took ~19 h to reach 68 GiB) and the plateau **scales with
  `maxSingleLen`** (measured: 8192 → tens of GiB; 2048 → ~4.5 GiB; 1024 → ~2.5
  GiB). So it "calms down" only to a very high ceiling.

## How it was diagnosed (macOS, on the live process)

Method, for future reference:
- **`vmmap -summary <pid>`** — region breakdown. `MALLOC_LARGE` dominated (GBs);
  Go-managed regions (`VM_ALLOCATE`) were ~12 MB. → the growth is **native
  (cgo/ORT)**, not the Go heap.
- **`heap <pid>`** — ~419k "non-object" C allocations totalling ~18 GB, with
  `libonnxruntime` C++ types present (`MatMul<float>`, `NodeProto`). → it is
  ONNX Runtime.
- **`leaks <pid>`** — only ~16 MB *truly* leaked (unreferenced); the GBs are
  **still referenced**. → not a classic leak; ORT is holding it deliberately.
- **RSS is a misleading metric here.** macOS deflates RSS via swap/compression
  (prod read 87 MiB resident while its committed footprint was ~19 GiB). Track
  **`Physical footprint`** (from `vmmap`, or `task_info`/`TASK_VM_INFO`
  `phys_footprint` in-process) — that is what Activity Monitor shows and what
  forces an OOM kill.

## What did NOT work (all measured, none moved the plateau)

Every ORT- and OS-level knob was tried against the grow/plateau harness and had
**no effect** on the retained footprint:

1. `SessionOptions.SetCpuMemArena(false)` — disable the CPU arena.
2. `SessionOptions.SetMemPattern(false)` — disable the shape-based planner.
3. **`memory.enable_memory_arena_shrinkage` RunOption** — ORT's purpose-built
   "shrink the arena after this run." Not exposed by `yalue/onnxruntime_go` at
   any version, so a **fork** (`knusbaum/onnxruntime_go`) added an
   `AddRunConfigEntry` wrapper and attndb ran every encode with it. **It still
   didn't release the memory** — the plateau was identical. This makes the fork a
   dead end for this problem.
4. `malloc_zone_pressure_relief(malloc_default_zone(), 0)` — force libmalloc to
   return cached free memory. No effect → the memory isn't in libmalloc's
   drainable cache; ORT hasn't freed it.
5. `SetIntraOpNumThreads(1)` — rule out per-thread scratch. No effect.

Crucially, the footprint grew even under **identical repeated queries** and then
plateaued. Identical shape ⇒ the arena/pattern would reuse, not grow — so the
retention is **session-lifetime working memory ORT reserves for its largest
encode and will not hand back via any configuration.**

## The fix: a persistent query session + an ephemeral ingest session

The only thing that releases the memory is **destroying the session**. Rather
than recycling one shared session (which would pause queries during the swap),
split by role: keep the **query session** alive and small, and create an
**ephemeral ingest session** for each reconcile, closing it when the import
finishes. Same model, two sessions — ORT fully supports multiple independent
sessions from one environment, each with its own arena (verified).

Measured (single-vector, cap 8192, `>8192`-token docs, identical input):

```
                          peak during ingest → after ingest Close()+GC
  single shared session:  ~30 GiB            →  ~5.0 GiB
  query + ingest session: ~33 GiB            →  ~6.7 GiB (query stays alive)
  New()/session reload:   ~170 ms
```

Key points, corrected from an earlier draft:
- **Peak during 8192 ingest is ~30 GiB either way** — inherent to the 8192 cap,
  *not* caused by the second session. (An earlier note claimed ~5 GiB for a
  single session; that came from a smaller-token test and was wrong.)
- The two approaches are **comparable on memory**; the second (query) session
  adds only its ~0.5 GiB weights + some fragmentation.
- **The ephemeral-ingest design wins operationally: queries never pause.** That's
  the deciding factor.
- Post-close floor is ~5–7 GiB (a residual that scales with encode size and
  isn't fully returned by libmalloc immediately; reclaimable under pressure).
  Bounded and far below the 68 GiB runaway, but above the ~1–2 GiB ideal — an
  open item.
- Uses only stock `Close()`/`New()`, so **the fork is not needed**; revert to
  upstream `yalue/onnxruntime_go`.

### Design

- The single-vector encoder is the hog (ColBERT is capped at 300 tokens —
  negligible). Give the daemon a **persistent query encoder** and, per reconcile,
  a **separate ingest encoder** created at the start of the reconcile and
  `Close()`d when it finishes. The reconciler/ingest path uses the ingest
  encoder; `search_vault` uses the query encoder — they never share a session.
- Model reload for the ingest session is ~170 ms, paid once per reconcile
  (background, off the query path).
- Follow the ingest `Close()` with `releaseNativeMemory()`
  (`malloc_zone_pressure_relief`) in case it helps return the post-close residual.
- Multiple sessions confirmed safe: query + ingest ran concurrently, the query
  session kept serving during and after ingest, and closing the ingest session
  freed ~26 GiB.

### Instrumentation (keep)

- `serve` gained `-memlog` (periodic memory log) and `-debug-addr` (pprof). Fix
  `-memlog` to report **Physical footprint** (committed), not just RSS, since RSS
  hid the problem.

## Alternatives considered

- **Reduce `maxSingleLen`** (e.g. 1024 → ~2.5 GiB steady). Rejected: it lowers
  the ceiling but degrades whole-doc vector quality, and doesn't give memory
  *back* — it just shrinks what's held. Kept as an env knob (`ATTNDB_MAX_SINGLE_LEN`,
  default 8192), not the fix.
- **`ArenaCfg` + `CreateAndRegisterAllocator`** (v1.31) with a `max_mem` cap.
  *Bounds* the ceiling (prevents a 68 GiB runaway) but doesn't release-after, and
  the cap must exceed one big encode (~17 GiB) so steady state stays high.
- **Fork-and-die process model** — fork a subprocess for the reconciler/encoding,
  let it consume memory, then let the whole process exit so the OS reclaims
  everything. Guaranteed release, but requires IPC + synchronization we'd rather
  avoid. Made unnecessary by session recycling working.

## Option evaluated and RULED OUT: a custom ONNX allocator (mmap large / malloc small)

Hypothesis (Kyle): the retained GBs are large buffers that ORT *frees* per-run,
but macOS **libmalloc caches freed large blocks and won't return them to the
OS** (`malloc_zone_pressure_relief` doesn't drain them). A custom `OrtAllocator`
that routes large allocations straight to `mmap` and `munmap`s them on free —
small ones to `malloc` — hands memory back to the OS immediately, giving a low
steady state **without** session recycling (no reload latency, continuously low).

The evidence supports this: `leaks` shows the GBs are *not* leaked (freed-but-
retained), the footprint plateaus/reuses (freed-and-recycled), and `Close()`
releases it (big contiguous regions libmalloc can return). All consistent with
"ORT frees, libmalloc hoards."

Prerequisites / risks:
- Register a custom `OrtAllocator` (our C `Alloc`/`Free` fn-pointers, with a
  size-tracking header so `Free` knows the mapping to `munmap`) via
  `RegisterAllocator` + session config `session.use_env_allocators=1`. Needs a
  fork wrapper (more cgo than `AddRunConfigEntry`).
- **Arena must be OFF** — otherwise ORT's arena requests big chunks from our
  allocator once and holds them, so our `Free` never runs.
- **Risk:** if ORT holds the big buffers *in use* for the session (not freed
  per-run), our `Free` never fires and this doesn't help — then session recycling
  is the fallback.

**Validated with `malloc_zone_statistics` (default 8192 cap, large docs) — the
allocator won't help.** In both arena configs the freed-but-cached portion is
negligible (~80 MiB); the retained memory is **in-use**, i.e. held by ORT, not
sitting in a libmalloc cache:
- **Arena ON:** footprint ~5.3 GiB, `malloc_in_use` ~3.8 GiB, **cached ~80 MiB**.
  ORT keeps ~3.8 GiB of working buffers allocated and reuses them across encodes.
- **Arena OFF:** *worse* — footprint spikes to ~18 GiB and settles ~13 GiB, with
  only ~3.8 GiB in the malloc zone (ORT mmaps the rest directly, outside
  libmalloc), **cached still ~80 MiB**, and it's not returned.

So the premise (libmalloc hoarding freed large blocks) is false — there's nothing
freed-but-cached to reclaim, and ORT never calls `free`/our-`Free` on the held
buffers. A custom allocator can't touch memory ORT is holding in-use. Session
recycling remains the only mechanism that frees it (it destroys the session).

## Decision

**Persistent query session + ephemeral per-reconcile ingest session** (destroy
the ingest session when the import finishes). Session destruction is the only
thing that frees the held memory; splitting query/ingest into separate sessions
gets that release **without pausing queries**. The allocator route is ruled out,
arena-shrinkage is a dead end, and arena-disable makes peak worse.

## Measurement caveat (to reconcile before trusting absolute numbers)

Two runs disagree on the single-session 8192 peak: the `malloc_zone_statistics`
test reported ~5.3 GiB footprint, while the clean single-vs-two A/B reported
~30 GiB — for nominally identical config. The **single-vs-two comparison is still
sound** (both measured in the same process/run: ~30 vs ~33 GiB), but the absolute
peak/floor figures and the allocator ruling-out (whose malloc breakdown came from
the ~5.3 GiB run) should be re-confirmed with **one clean consolidated
measurement** that logs token count, footprint, and the malloc in-use/cached
breakdown together at the true peak. (The cached-≈80 MiB finding did hold across
two different footprints — 5.3 and 18 GiB — so the allocator conclusion is likely
robust, but confirm it.)

## Open items

- **[measure]** One consolidated 8192 measurement (token count + footprint +
  malloc breakdown) to reconcile the 5.3-vs-30 GiB discrepancy and re-confirm the
  allocator ruling-out at the true peak.
- **[work]** Persistent query encoder + per-reconcile ephemeral ingest encoder in
  the daemon; ingest path uses the ingest encoder and `Close()`s it when the
  reconcile finishes (+ `releaseNativeMemory()` after). Keep the arena ENABLED.
- **[work]** Fix `-memlog` to report Physical footprint, not just RSS.
- **[cleanup]** Revert the fork `replace` in go.mod and the arena-shrinkage
  wiring (done in code); the `knusbaum/onnxruntime_go` fork is no longer needed.

## Key facts to remember

- Native ORT memory, referenced-not-leaked, session-lifetime, scales with
  `maxSingleLen`; a single 8192-token encode ≈ 9–17 GiB.
- No ORT/OS knob releases it; only session destruction does.
- `Close()` → back to ~1 GiB; `New()` reload ≈ 170 ms.
- Diagnose with `vmmap`/`heap`/`leaks` and **Physical footprint**, not RSS.

---

## ROOT CAUSE FOUND (2026-07-20): MLAS ArmKleidiAI GEMM run-buffers

Earlier sections chased the wrong stream. With per-alloc logging in the custom
allocator + a disciplined multi-pass floor test + `malloc_history` byte-accounting,
the leak is now identified conclusively.

### Method
- Custom mmap CPU allocator logs every Alloc/Free (`ATTNDB_ALLOC_LOG`).
- One daemon, one vault, measured RSS + **committed footprint** + `MALLOC_LARGE`
  in-use + `heap` live-node bytes after each of N reconcile (re-ingest) passes.
- `MallocStackLogging=1` + `malloc_history` on every `MALLOC_LARGE` region,
  classified by owner and **summed by bytes** (not by largest-region sampling).

### Findings (measured, not inferred)
- The custom mmap allocator **works**: 141k allocs / 141k frees, peak ~14 GiB,
  final live 0.23 GiB — it munmaps its stream back to the OS cleanly.
- But the floor **accumulates** in a *second* stream it never sees:
  committed 0.9 G (baseline) → 16.3 G (pass 1) → 30.2 G (pass 2), ~14–15 GiB/pass.
  RSS looked low only because macOS compressed/swapped it — still committed.
  Linear ⇒ ~4 passes ≈ 60 GiB = the original 68 GiB OOM.
- Byte-weighted `malloc_history` of the 4.04 GiB one-doc floor:
  **RUN/MLAS-GEMM 3.88 GiB (96%, 207/220 regions)**, RUN/other 0.14 G, prepack ~0.
- Owner backtrace of the bulk (128 MiB × 24 regions):
  `OrtApis::Run → InferenceSession::Run → ExecuteKernel → MatMul<float>::Compute
  → MlasGemmBatch → ArmKleidiAI::MlasGemmBatch → operator new → _malloc`.

### Why our allocator missed it
MLAS's KleidiAI GEMM path allocates its packing/working buffers with **raw
`operator new` / `malloc`**, *not* through the `OrtAllocator` interface. So
`RegisterAllocator` + `session.use_env_allocators` + `SetCpuMemArena(false)` can
never route them — they go straight to libmalloc `MALLOC_LARGE`, and the buffers
are **pooled beyond session lifetime** (still live after `session.Destroy()`),
so each ephemeral ingest session's runs pile a fresh ~14 GiB on the heap.

### Consequences for the fix
- The **ephemeral-ingest-session design does not help** and the custom mmap
  allocator, while correct, targets the innocent stream. Neither addresses the leak.
- A **long-lived/reused session is not a clean fix**: different doc token-counts
  produce different GEMM shapes, and MLAS pools a buffer per shape → still grows
  (bounded only by #distinct shapes × buffer size, i.e. large).
- Candidate real fixes (to evaluate): (a) **disable KleidiAI** in MLAS (build-time
  `--use_kleidiai OFF`, or a runtime/session flag if one exists) and confirm the
  standard SGEMM path frees/bounds; (b) **fork-and-die** ingest in a child process
  that exits per pass (OS reclaims everything regardless of MLAS pooling) —
  guaranteed, independent of ORT/MLAS internals; (c) cap GEMM working-set size via
  `maxSingleLen` (rejected: quality) or MLAS tuning knobs if any exist.
- Self-validating: apply a fix, rerun the multi-pass floor test, confirm the
  `MALLOC_LARGE`/committed floor stays flat across passes.

## FIX CONFIRMED (2026-07-20): mlas.disable_kleidiai

Added `opts.AddSessionConfigEntry("mlas.disable_kleidiai", "1")` to every ONNX
session (`buildSessionOptions` in internal/encode/onnx/colbert.go). The dylib
exposes this key, so no ORT rebuild is needed.

Multi-pass floor test (6× 84 KB docs, ns=memtest, repeated edit→reconcile),
measuring the **libmalloc `MALLOC_LARGE` leak stream** after each ingest cycle:

| config              | pass 1  | pass 2  | pass 3  |
|---------------------|---------|---------|---------|
| KleidiAI ON (before)| 16.3 G  | 30.2 G  | (→ ~68 G OOM) |
| KleidiAI OFF (fix)  | 0.99 G  | 0.99 G  | 1.00 G  |

The accumulating MALLOC_LARGE stream is gone; the floor is flat at ~1 GiB across
passes. Between-encode RSS returns to ~1.5 GiB. The only remaining large memory is
the **transient** per-encode attention working set (~14 GiB at maxSingleLen 8192),
which flows through the custom mmap allocator and is munmap'd back to the OS after
each encode — a transient peak, not a leak. Ingest speed is unchanged (~5 min for
6 docs, same as KleidiAI on — the earlier "regression" was a measurement artifact
of overlapping background timers).

### Net state of the memory work
- **Leak: fixed** by one session-config line; verified flat across 3 passes.
- The custom mmap allocator (fork) returned the ~14 GiB transient per-encode
  working set to the OS between encodes (RSS → ~1.5 GiB). Without it that
  transient sits in libmalloc until the ingest session is closed.
- **DECIDED (2026-07-20): the fork is dropped.** The `replace
  github.com/yalue/onnxruntime_go => ../onnxruntime_go` was experimental and
  should never have been committed — it made the `onnx` build impossible to
  compile without an undocumented sibling checkout. attndb now uses upstream
  `yalue/onnxruntime_go v1.31.0`, which carries the same ORT 1.26 upgrade the
  fork was based on and every API used here except `RegisterMmapCpuAllocator`.
  What remains bounding ingest memory: `mlas.disable_kleidiai` (the actual leak
  fix), the ephemeral per-reconcile ingest session (closed each pass, so the
  transient is released then), `SetCpuMemArena(false)` on that session, and
  `ATTNDB_MAX_SINGLE_LEN` to cap the working set.
  **Not re-measured on Apple Silicon since the change** — the transient peak
  during a pass is expected to be higher than with the mmap allocator, while the
  flat across-pass floor (the thing that caused the OOM) is unaffected. Re-run
  the multi-pass floor test on a Mac before trusting the daemon with a large
  vault; if the transient proves painful, lower `ATTNDB_MAX_SINGLE_LEN` rather
  than reviving the fork.
- Follow-ups: `-memlog` should report committed footprint (not RSS).
