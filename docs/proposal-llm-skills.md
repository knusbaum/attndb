# Proposal: LLM skills for querying & uploading

**Goal:** an LLM using attndb (via the MCP server) reaches for it at the right
moments, phrases queries well, reads the calibrated scores correctly, and — once
uploads exist — knows when and how to add a document.

Status: proposal. Tags: **[decision]** your call, **[work]** implementation.
Note the dependency: the **query skill works against today's server**; the
**upload skill is gated on the upload path from the multi-user proposal**
(`proposal-multi-user.md`).

## Why guidance is needed

`search_vault` already carries a good description and a calibrated relevance
gate, but an LLM left to itself tends to: under-use retrieval (answer from
memory when it should ground); over-trust weak hits; ignore the "no confident
match" signal; and cite vaguely. A skill turns the tool's affordances into
behavior.

## Delivery — DECIDED: tool descriptions + Claude Code skills

Two layers:

1. **Strong tool descriptions (baseline, all clients).** The `search_vault` /
   `add_doc` / `delete_doc` / `retrieve_doc` descriptions are always in context
   for *any* MCP client, so they carry the essential when/how guidance (when to
   search, reading calibrated scores, the search→`retrieve_doc` chain). This is
   the universal floor and worth investing in regardless. [work]
2. **Claude Code skills (proactive layer).** `SKILL.md` files in a `skills/` dir
   users drop into `~/.claude/skills`. Unlike user-invoked MCP prompts, skills
   are **auto-surfaced by description** — the model pulls them in when relevant,
   which is exactly the goal ("reach for the tool at the right moment"). Richer
   than a description (examples, multi-step guidance). Claude-Code-specific, which
   is fine — it's the primary client. [work]

Not doing MCP prompts for now: they're user-invoked, so weak at prompting timely
use, and the description+skill layers cover the need. Revisit only if
cross-client portability of the *proactive* guidance becomes a real requirement.

## Skill 1 — querying (works today)

Content the guidance should encode:
- **When to search:** any question plausibly answered by the user's own
  corpus/notes — before answering from general knowledge. Prefer a search over a
  guess about the user's specific material.
- **How to phrase:** natural-language, entity-rich queries (names, error codes,
  specific nouns). Late-interaction retrieval rewards specific terms; it's not
  keyword-exact, so paraphrase the *meaning*.
- **Reading scores — the important part:** scores are calibrated, so a **low top
  score means no confident match** — say "I didn't find anything relevant"
  rather than dressing up a weak hit. Use the `min` gate to enforce this when the
  cost of a wrong answer is high.
- **k discipline:** small `k` (3–5) for a focused answer; larger only when
  surveying.
- **Chaining:** results are *spans* (path + byte range + snippet). For real
  synthesis, call **`retrieve_doc(path)`** to read the whole file for surrounding
  context rather than answering from the snippet alone (no host filesystem access
  needed — it's a tool). **Always cite** path (and span) so the user can verify.

## Skill 2 — writing (gated on the filesystem tools)

Depends on the `add_doc`/`delete_doc`/`retrieve_doc` tools from the multi-user
proposal (phase 1). Content:
- **When to add:** the user says "save/remember this," produces a document worth
  retrieving later, or the LLM synthesizes something durable. Not for transient
  chatter.
- **Format conventions:** Markdown, a clear H1 title, meaningful path/filename;
  factual and self-contained (it'll be retrieved out of context later).
- **The contract (eventually-consistent):** `add_doc` writes the file; indexing
  follows via the watcher a beat later, so it becomes searchable shortly after —
  not synchronously. If the skill wants to **confirm by searching** (the
  add-then-verify loop from the sourdough validation), it should **retry the
  search briefly** rather than expect an immediate hit. Confirm to the user with
  the path.
- **Care:** don't duplicate existing docs (`search_vault` first); in a shared
  index, never write secrets or another person's private content.

## Recommended path & phasing
- **Phase 1 (today):** tighten the `search_vault` description; ship the **query**
  `SKILL.md` (works against the server as it exists now).
- **Phase 2 (with the FS tools):** write descriptions for `add_doc`/`delete_doc`/
  `retrieve_doc` and ship the **write** `SKILL.md`, once those tools land.

## Decisions made
- **Delivery:** strong tool descriptions (all clients) + Claude Code `SKILL.md`
  files (proactive layer). No MCP prompts for now.

## Open items
- **[work]** Author the query SKILL.md + tighten `search_vault` description.
- **Dependency:** write skill blocked on the `add_doc`/`delete_doc`/`retrieve_doc`
  tools from `proposal-multi-user.md` (phase 1).
