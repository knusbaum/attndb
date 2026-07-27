# attndb

attndb is a private, self-hosted search index over your own documents — notes,
internal docs, a wiki export, an Obsidian vault — that plugs into your AI
assistant over MCP. Point it at a folder and it gives your assistant tools to
**search, read, write, and edit** those documents, kept in sync automatically as
files change. Only Markdown (`.md`) files are indexed and addressable, for now.

What makes it worth using over "just embed everything": matches localize to the
**exact passage** that answered the question, not just the document, and scores
are calibrated — searches return relevant passages across your entire document
corpus, allowing agents to quickly find the information they need.

## Quickstart

Requirements: Docker and Docker Compose. Nothing else —  the image build handles
all dependencies itself.

```
git clone <this repo> && cd attndb
cp .env.example .env        # then set ATTNDB_DOCS to the folder you want indexed
docker compose up -d
docker compose logs -f attndb
```

The first `up` builds the image, which includes converting the encoder models —
several minutes and a few GB of RAM, once. After that you have an MCP endpoint
at `http://localhost:8765` that indexes `ATTNDB_DOCS` and keeps it current as
files change.

## Connect it to your AI assistant

**Claude Code:**
```
claude mcp add --transport http attndb http://localhost:8765 --scope user
```
`/mcp` inside a session shows the tools; `claude mcp remove attndb -s user` to
undo.

**Other MCP clients:** point them at the same URL (Streamable HTTP, no path
suffix). For a client that only speaks stdio, bridge with
`npx mcp-remote http://localhost:8765`.

The server has no built-in auth, so leave it on `localhost` unless you put a
reverse proxy with TLS + a token in front of it (see
`docs/proposal-multi-user.md`) — the write tools below can modify your
documents.

## What your assistant can do with it

| Tool | What it does |
|---|---|
| `search_vault(query, k?, min?)` | Semantic search. Ranked hits with path, matched line range, a calibrated score, and a snippet. A low top score means no confident match. |
| `list_docs(pattern?, limit?)` | List documents, optionally glob-filtered (`*`, `**`, `?`), most-recently-modified first. Browsing, not content search. |
| `read_doc(path, offset?, limit?)` | Read a document's exact text, paged. |
| `write_doc(path, content, append?)` | Create, overwrite, or (`append: true`) add to the end of a document. |
| `edit_doc(path, old_string, new_string, replace_all?)` | Exact-string replacement, for changing part of a document without rewriting it. |
| `delete_doc(path)` | Remove a document. |

A change made through these tools becomes searchable a moment later, not
instantly — the folder is the single source of truth, and the index is a
projection of it that also updates from any other change to the folder (a sync
client, a git pull, you editing a file directly).

## Get proactive search-and-capture behavior (optional, Claude Code)

Two skills teach an LLM to reach for the vault at the right moments — searching
it before researching a topic from scratch, and writing durable findings back
into it afterward:

```
ln -s "$PWD/skills/attndb-search"           ~/.claude/skills/attndb-search
ln -s "$PWD/skills/attndb-research-capture" ~/.claude/skills/attndb-research-capture
```

Pair with a standing instruction in `~/.claude/CLAUDE.md` so this happens
without being asked each time:

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

## Configuration

Set these in `.env` (see `.env.example`); each has a working default so
`docker compose up -d` runs with none of them set.

| Variable | Default | What it is |
|---|---|---|
| `ATTNDB_DOCS` | `./vault` | the document tree to index and serve (`.md` files only) — the one setting worth changing |
| `ATTNDB_STATE` | `./attndb_state` | generated calibration, persisted across restarts |
| `QDRANT_STORAGE` | `./qdrant_storage` | Qdrant's data directory |
| `ATTNDB_PORT` | `8765` | host port for the MCP endpoint |
| `ATTNDB_NS` | `vault` | collection namespace — isolates this index from others sharing the same Qdrant |
| `ATTNDB_POLL` | `30s` | change-detection interval (a container has no native file-change events) |
| `ATTNDB_RESYNC` | `5m` | full-resync backstop for bulk/directory changes the poller can miss |
| `ATTNDB_MIN` | `0` | default relevance gate for `search_vault` (`0` = no gate) |

## Further reading

- `docs/architecture.md` — how retrieval actually works (composable passes,
  per-token late-interaction scoring, calibration)
- `docs/building.md` — building without Docker: native ONNX encoders, Metal/
  CoreML acceleration, multi-arch images
- `docs/live-vault-index.md` — the live daemon's watch/reconcile/recalibration
  design
- `docs/proposal-multi-user.md` — exposing this beyond `localhost` (auth,
  reverse proxy)
- `docs/build-plan.md` — roadmap
