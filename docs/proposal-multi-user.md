# Proposal: multi-user collaboration (self-hosted shared server)

**Goal:** a small team runs one attndb instance on a shared box; members query a
common index — and add documents to it — over MCP, from their own machines.

Status: proposal. Tags: **[decision]** your call, **[eval]** needs measurement,
**[work]** implementation.

## Where we are today

`attndb serve` is a single process that: builds the encoders once (resident),
talks to a local Qdrant, watches one local docs directory, reads snippets from
that local filesystem, binds `localhost`, and has **no auth**. Critically, it is
**one process = one namespace = one reconciler = one calibration state = one
watch loop**. That last fact shapes everything below.

## What multi-user actually needs

Three capabilities, in increasing cost:
1. **Reachable + safe over a network** (bind beyond localhost, TLS, auth).
2. **A way for users to add documents** to the shared index.
3. **Isolation** between teams/topics (optional; this is the expensive one).

## Phase 1 — one shared namespace (the "simplest real multi-user")

Everyone reads and writes **the same index**. This is genuine collaboration with
**near-zero architectural change** — it's today's daemon, exposed safely.

- **Network + TLS + auth — [work], not an SDK dependency.** The MCP server is a
  plain `http.Handler` (Streamable HTTP), so authentication is a
  middleware/reverse-proxy concern, not a code change to the server. Put
  **Caddy or nginx** in front for TLS termination + a bearer-token / basic-auth
  gate; bind attndb to `127.0.0.1` behind it. Static per-user tokens are the
  phase-1 answer; OAuth is a later refinement.
- **Deployment — [work].** Extend `docker-compose.yml` to two services (attndb +
  Qdrant) on one box, plus the reverse proxy. `docker compose up -d` brings up
  the whole stack.
- **Concurrency is fine here.** The encoder mutex serializes all encode calls
  (query-encode and ingest-encode), which is correct for small-team,
  roughly-single-user-at-a-time load. **[eval/scaling lever]** if concurrent use
  grows, encoder **batching** (the open roadmap item) is the throughput fix —
  name it, don't build it yet.

### Adding documents — DECIDED: the watched folder is the only sync entry point

**Decision:** the **watched folder stays the single sync entry point** to the
index. *How* documents get into that folder is deliberately decoupled from the
sync mechanism — sync always flows through the existing watcher → reconciler
path (exactly the add/delete loop validated by the sourdough test). We do not
build a separate upload store or a second ingest path.

The remote interface is a thin **read/write API over the backing filesystem**,
three MCP tools that operate on files in the watched folder:

- **`add_doc(path, content)`** — write a file into the watched tree. Indexing
  follows automatically via the watcher; the tool does not ingest directly.
- **`delete_doc(path)`** — remove a file from the tree (watcher drops it).
- **`retrieve_doc(path)`** — read a file's full content back. Complements
  `search_vault` (which returns spans/snippets): the search→read-full-context
  chain becomes two tools instead of requiring host filesystem access.

Because these are just filesystem operations, the server owns no new storage
model and snippets keep reading from the same on-disk copy. The design keeps one
invariant: **the filesystem is the source of truth; the index is a projection of
it.** Anything that can put a file in the folder (these tools, a synced vault, a
git push, a future mount) feeds the same sync.

*Note — searchability is eventually-consistent by design.* `add_doc` returns once
the file is written; the doc becomes searchable a beat later when the watcher
reconciles it (sub-second in practice). A caller that needs confirm-on-return
(e.g. an LLM upload skill that verifies by searching) can poll `search_vault`/
`retrieve_doc` briefly. We are intentionally *not* coupling `add_doc` to the
ingest path — keeping the folder as the sole sync entry point is worth the small
window.

**Later:** expose the backing folder over **SMB or another filesystem protocol**
so users can mount the foreign file tree directly and manage documents with
ordinary file operations — still the same single sync entry point, no new ingest
path.

## Isolation — DECIDED: out of scope

One shared index, no per-user/per-team isolation. Everyone reads and writes the
same namespace. This keeps v1 to a single DB / reconciler / calibration state and
avoids the real cost of multi-namespace (N reconcilers, N calibration schedules,
identity→namespace routing). Revisit only if a concrete need for private indexes
appears; the `-ns` flag already partitions collections in Qdrant if we ever do.

## Cross-cutting concerns
- **Backups — [work].** Qdrant snapshots + the watched docs directory.
- **Doc provenance — [work].** With shared write access, consider stamping
  author/source into the payload (the `Meta` → payload path already exists) for
  attribution.
- **Concurrency/throughput.** The encoder mutex serializes encode calls — fine
  for small-team load; **batching** (open roadmap item) is the lever if it grows.

## Recommended path & phasing
- **Phase 1:** compose stack (attndb + Qdrant + Caddy), TLS + static-token auth,
  one shared index, watched-folder sync + the `add_doc`/`delete_doc`/
  `retrieve_doc` filesystem tools. Delivers "a team queries and grows a shared
  index."
- **Phase 2 (later):** mountable backing folder over SMB / a filesystem protocol,
  so users manage docs as ordinary files — still the one sync entry point.
- **Phase 3 (if load demands):** OAuth, quotas, encoder batching for throughput.

## Decisions made
- **Doc-add:** watched folder is the sole sync entry point; expose it via thin
  `add_doc`/`delete_doc`/`retrieve_doc` filesystem tools. No separate upload store.
- **Isolation:** out of scope — one shared index, no per-user/team namespaces.

## Open items
- **[eval]** Concurrent-query throughput before batching becomes necessary.
- **[work]** Reverse-proxy TLS + static-token auth, two-service compose stack,
  the three filesystem tools, Qdrant snapshot + docs-dir backups.
- **[later]** SMB/mountable backing folder; provenance stamping.
