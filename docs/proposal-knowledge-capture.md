# Proposal: autonomous knowledge capture

**Goal:** the corpus grows itself. As a byproduct of researching *with* an LLM,
the durable things learned get recorded into the searchable corpus **without the
human having to ask** — and prior research is *found* before it's redone.

This is the flagship reason the project exists: not just faster search over a
corpus, but a corpus that accumulates what you learn. It extends the skills
proposal (`proposal-llm-skills.md`) and depends on the filesystem tools from the
multi-user proposal (`proposal-multi-user.md`).

Status: proposal, decisions made in review. Tags: **[work]** implementation,
**[eval]** needs observation in practice.

## The two failure points it closes

The pain — "re-researching because I never recorded it" — has two halves, and
both must be in the behavior or the loop leaks:

1. **Search-first** — before starting *any* research, query the corpus; a
   confident prior answer is used/extended instead of redone. *(prevents redo)*
2. **Capture-after** — when real research happens, produce a durable doc and
   write it in. *(prevents loss)*

Together they're a ratchet: every research interaction either *uses* prior
knowledge or *adds* to it.

## Not a new silo

`add_doc` writes to the watched folder, which is the user's existing note vault
(for the primary user, the Obsidian vault). Captured research becomes ordinary
vault notes, indexed by attndb, integrating with the existing note system rather
than creating a parallel store. The loop closes on infrastructure already in
place: **research → write a note into the vault → attndb indexes it → the next
search finds it.**

## The central risk: precision

A knowledge base's entire value is precision. Over-capture — a vault full of
half-baked, duplicative auto-notes — makes every search noisier and erodes trust
in the whole tool. So the hard part is not the writing; it is the **judgment**:
what is worth recording, and not duplicating what already exists. The threshold
and dedup rules below exist to protect precision.

## Decisions made

- **Trigger:** a **standing directive in global `CLAUDE.md`** ("after substantial
  research, capture to attndb") plus a **`SKILL.md`** holding the procedure.
  Judgment-based (matches how the primary user already drives behavior), not a
  hook. Accepted tradeoff: the model will occasionally forget; the notify-after
  line (below) is the detector, and a Stop-hook nudge is the documented fallback
  if it proves too leaky. **[eval]**
- **Consent:** **autonomous + notify-after.** The LLM writes when it judges the
  material worthy, then reports what it recorded and where. Matches "without
  intervention" while keeping the human informed and able to correct/prune.
- **Placement:** a **dedicated `Research/` folder** with dated, topic-slugged
  filenames, and a **provenance stamp** marking auto-captured docs — keeping
  machine notes visually separable and easy to review/prune.

## The procedure (skill content)

### Search-first (works against today's server)
Before starting research, `search_vault` the question. A confident hit (top
score above the calibrated gate) → read it (`retrieve_doc`) and use/extend it
rather than redoing the work. This half needs no new tools — `search_vault`
exists today.

### Capture threshold — the precision guard
- **Capture:** multi-source research; a synthesized conclusion; a decision plus
  its rationale; a non-obvious answer that took real work; anything the user
  reacted to or built on.
- **Skip:** quick factual lookups, trivia, conversational back-and-forth, and
  anything already well-covered in the corpus.

### Dedup — update, don't clone
Always `search_vault` the topic first. If a closely related doc exists,
`retrieve_doc` it and `add_doc` to the **same path** with merged/updated content
(overwrite = the delete-then-ingest already built). Otherwise create a new file.
Semantic dedup is never perfect; the dedicated `Research/` folder is the review
valve — periodically skim and prune.

### Document structure — self-contained for out-of-context retrieval
1. H1 title
2. One-line summary
3. The question / goal that prompted the research
4. Findings / conclusion (the durable content)
5. Caveats & confidence
6. Sources (links)
7. Provenance footer (auto-captured, date, brief session context)

### Placement & provenance
`Research/<YYYY-MM-DD>-<topic-slug>.md` in the watched vault. Frontmatter
`source: attndb-auto-capture` + date so machine notes are separable and prunable
(ties to the provenance stamping in `proposal-multi-user.md`).

### Notify-after
One line after writing: "Recorded research to `Research/…`" (or "updated `…`").
Keeps visibility without intervention, and doubles as a health signal — if
research-heavy sessions produce no capture notices, the directive isn't firing.

## Dependencies & phasing
- **Search-first half → M1.** Works today (`search_vault`); ships with the query
  skill.
- **Capture half → M2.** Gated on `add_doc` / `retrieve_doc` from the multi-user
  proposal. This **subsumes the "write skill"** in `proposal-llm-skills.md` — it
  *is* that skill, elevated to an autonomous loop.

## Open items
- **[work]** The standing `CLAUDE.md` directive + the `attndb-research-capture`
  SKILL.md (search-first, threshold, dedup, doc template, notify).
- **[work]** Provenance stamping (shared with `proposal-multi-user.md`).
- **[eval]** In practice: is the judgment-based trigger reliable enough, or is
  the Stop-hook fallback needed? Is the threshold catching the right material
  without polluting the corpus? Watch the `Research/` folder's signal-to-noise.
