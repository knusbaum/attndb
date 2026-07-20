---
name: attndb-research-capture
description: >
  Record durable research into the user's attndb vault so it is never lost or
  redone. Use in two moments: (1) BEFORE researching a topic from scratch, search
  the vault first and reuse a confident prior answer; (2) AFTER doing substantial
  research — multi-source investigation, a synthesized conclusion, a decision plus
  rationale, or a non-obvious answer that took real work — write it back into the
  vault as a durable note (autonomously, then tell the user what was recorded).
  Trigger whenever a session involves real research, investigation, or a
  conclusion worth keeping; also on "save/remember this" or "write that up".
---

# attndb-research-capture — grow the vault as you work

The point of this skill: the corpus should *accumulate what you learn*. Every
research interaction should either **reuse** prior knowledge or **add** to it — a
ratchet, so the user stops re-researching things they (or you) already worked
out. It pairs the `search_vault` read path with the `write_doc`/`edit_doc` write
path (see also the `attndb-search` skill for query technique).

Capture is **autonomous**: when material clears the bar below, write it without
being asked, then report one line about what you recorded. Do not ask permission
first — but do tell the user after.

## 1. Search-first (before researching)

Before starting research from scratch, `search_vault` the question. On a
confident hit (top score above the calibrated gate), `read_doc` the cited file
(results are `path` + a start line) and **use or extend** it rather than redoing
the work. This half needs nothing new — it works today.

## 2. The capture threshold — protect precision

A knowledge base's whole value is precision; a vault full of half-baked,
duplicative auto-notes makes every search noisier and erodes trust. So judgment
about *what* to record matters more than the writing. 

**Capture** when the work is durable and would be painful to redo:
- Multi-source research or investigation
- A synthesized conclusion or recommendation
- A decision plus its rationale
- A non-obvious answer that took real effort
- Anything the user reacted to, built on, or asked you to remember

**Skip** (do not capture):
- Quick factual lookups and trivia
- Conversational back-and-forth
- Anything already well-covered in the vault (search first — see dedup)

## 3. Dedup — update in place, don't clone

Always `search_vault` the topic before writing. If a closely related note
already exists:
- `read_doc` it, then **update it in place**. Pick the narrowest tool that does
  the job:
  - **adding to the end** (a new finding, a dated entry) → `write_doc` with
    `append: true`. No need to read the document back first.
  - **changing a section** → `edit_doc`. `read_doc` returns text **verbatim**, so
    paste what you read straight in as `old_string` — no stripping or reformatting.
  - **restructuring the whole note** → `write_doc` to the **same path** with
    merged content (overwrite is fine; the index reconciles it).
- Only create a **new** file when nothing close exists.

Semantic dedup is never perfect; the dedicated `Research/` folder (below) is the
review valve — the user periodically skims and prunes it.

## 4. Where to write, and provenance

- Path: **`/Research/<YYYY-MM-DD>-<topic-slug>.md`** (absolute vault path;
  `<YYYY-MM-DD>` is today's date, `<topic-slug>` a short kebab-case topic).
- Stamp machine-written notes so they're separable and prunable — frontmatter at
  the top of the file:
  ```
  ---
  source: attndb-auto-capture
  date: <YYYY-MM-DD>
  ---
  ```

## 5. Document structure — self-contained for out-of-context retrieval

The note will be retrieved later with no memory of this session, so make it
stand alone:
1. `# H1 title`
2. One-line summary
3. The question / goal that prompted the research
4. Findings / conclusion (the durable content)
5. Caveats & confidence
6. Sources (links)
7. Provenance footer (auto-captured, date, brief session context)

## 6. Confirm, then notify

`write_doc` is eventually-consistent: the note becomes searchable a beat after
it's written, not instantly. If you want to confirm it landed, `search_vault`
for it again after a moment (retry briefly) rather than expecting an immediate
hit. Then tell the user **one line**:

> Recorded research to `/Research/2026-07-14-topic.md` (or "updated `/Research/…`").

This keeps the user informed without asking permission, and doubles as a health
signal: research-heavy sessions that produce no capture notice mean the loop
isn't firing.
