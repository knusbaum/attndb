# Build plan

Sequencing for building all four proposals:
[distribution](proposal-distribution.md) · [multi-user](proposal-multi-user.md) ·
[llm-skills](proposal-llm-skills.md) · [knowledge-capture](proposal-knowledge-capture.md).

**Approach: dependency-ordered milestones, not one-proposal-at-a-time.** Doing a
whole proposal at a time would strand the cheapest, highest-value work (query +
search-first guidance works *today*) behind unrelated packaging, and force the
capture loop to wait on distribution for no reason. Instead the work reorders
around the real dependency graph, with two deliberate cross-proposal interleaves
and two parallel de-risking tasks.

## Dependency graph

- **Query + search-first guidance** — no deps; works against today's daemon.
- **Filesystem tools** (`add_doc`/`delete_doc`/`retrieve_doc`) — no deps; new
  handlers on `serve`. **The pivot: everything downstream needs these.**
- **Auth + reverse proxy + compose** — no code deps; must precede exposing the
  write tools on a network.
- **Autonomous capture loop** — depends on the filesystem tools (its capture
  half) but its search-first half ships early.
- **Distribution/packaging** — structurally independent, but packages whatever
  the *final* surface is, so it goes last (build the release pipeline once).
- **int8 eval**, **CI/CGO spike** — independent; run in parallel as de-risking.

Critical path to the flagship (auto-capture): **FS tools → capture loop.**
Everything else hangs off that spine or runs beside it.

## M1 — Guided & self-searching locally

Fast, no server dependencies; makes the daemon that exists today better.

- **[skills]** Tighten the `search_vault` description; ship the **query
  `SKILL.md`**.
- **[capture, search-first half]** The standing `CLAUDE.md` directive's
  "search the corpus before researching" rule + the search side of the skill.
  Works now because `search_vault` exists — this alone starts killing the
  re-research pain for anything already in the vault.
- **Parallel de-risking:**
  - **[eval]** int8 retrieval quality vs fp (gates int8-as-default later).
  - **[spike]** prove the onnx binary builds on one CI runner (CGO + ORT +
    tokenizers) — surfaces the scariest unknown before it's on the critical path.

*Outcome:* the LLM searches before it researches and avoids redoing recorded work.

## M2 — Collaborative server + autonomous capture (the bulk)

- **[multi-user]** In order:
  1. **Filesystem tools** `add_doc` / `delete_doc` / `retrieve_doc` over the
     watched folder (the pivot — unlocks capture *and* the write skill).
  2. **Auth** — reverse proxy (Caddy/nginx) TLS + static token; land before the
     write tools are network-exposed.
  3. **Two-service compose** (attndb + Qdrant + proxy) + **backups** (Qdrant
     snapshot + docs dir) + **provenance stamping**.
- **[capture, capture half]** Once `add_doc`/`retrieve_doc` exist, wire the
  **capture directive + `attndb-research-capture` SKILL.md**: threshold, dedup
  (update-in-place), `Research/` placement, provenance, notify-after. This is
  also Proposal 3's write skill — the interleave.

*Outcome:* a shared index a team can query and grow; research auto-captured into
the vault without the user asking.

## M3 — Ship it public (packaging last)

- **[distribution]** Full CI build matrix + bundled ORT lib; `attndb pull`
  fetching the `models-v1` GitHub Release assets (with upstream LICENSE/NOTICE);
  `attndb doctor`; install script; Homebrew tap.
- Flip int8 to default **iff** the M1 eval cleared it.

*Outcome:* a stranger installs and runs the complete feature set in minutes.

## Work → milestone map

| Work item | Proposal | Milestone | Depends on |
|---|---|---|---|
| Query SKILL.md + description polish | skills | M1 | — |
| Search-first directive/rule | capture | M1 | — |
| int8 eval | distribution | M1 (parallel) | — |
| CI/CGO build spike | distribution | M1 (parallel) | — |
| `add_doc`/`delete_doc`/`retrieve_doc` | multi-user | M2 | — |
| Reverse-proxy TLS + token auth | multi-user | M2 | — |
| Two-service compose + backups | multi-user | M2 | auth |
| Provenance stamping | multi-user / capture | M2 | FS tools |
| Capture directive + capture SKILL.md | capture | M2 | FS tools |
| CI matrix + ORT bundling | distribution | M3 | CI spike |
| `attndb pull` + `models-v1` release | distribution | M3 | — |
| `attndb doctor` | distribution | M3 | — |
| install script + Homebrew tap | distribution | M3 | CI matrix |
| int8 opt-in / default flip | distribution | M3 | int8 eval |

## Deferred (explicitly out of scope for now)
- Per-user/team **namespace isolation** (multi-user) — one shared index.
- **Embedded store** (distribution) — Qdrant-only unless Docker is the blocker.
- **SMB / mountable** backing folder (multi-user).
- **Stop-hook** capture fallback (capture) — only if the judgment trigger proves
  too leaky.
- **MCP prompts** (skills) — descriptions + Claude Code skills instead.

## Before building
Take an advisor pass over M2's server work in particular (auth surface, the FS
tools' interaction with the reconciler, per-request behavior) before writing
code — same discipline that caught the symlink and calibration-race issues in
the live-index build.
