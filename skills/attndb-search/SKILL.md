---
name: attndb-search
description: >
  Search the user's personal document vault / knowledge corpus (indexed by attndb,
  exposed via the search_vault MCP tool) before answering from general knowledge or
  researching a topic from scratch. Use whenever a question might be covered by the
  user's own notes, prior research, decisions, or saved documents — people, projects,
  error codes, rationale, anything specific to them. Trigger it at the start of research
  to check whether the answer is already recorded, and any time the user asks about
  "my notes", "what we decided", "did I write down", or their own material.
---

# attndb-search — query the personal vault

attndb indexes the user's document vault (for the primary user, an Obsidian note
vault) and exposes one MCP tool, `search_vault`, which returns calibrated
relevance scores. This skill is about reaching for it at the right moments and
reading the results correctly.

## When to search

- **Before researching anything from scratch.** If the user asks a question that
  their own corpus plausibly covers — a past decision, a project detail, prior
  research, a name, an error code — search the vault *first*. A confident prior
  answer is worth more than fresh general-knowledge guessing, and it avoids
  redoing work the user already recorded.
- Any time the user references their own material: "my notes on…", "what did we
  decide about…", "did I write down…", "the doc about…".
- Prefer a search over a guess about the user's *specific* situation. General
  facts you can answer directly; anything particular to them, check the vault.

Skip it for pure general knowledge with no plausible vault coverage, and for
transient conversational back-and-forth.

## How to phrase queries

- **Natural language, entity-rich.** Include the specific, distinctive terms:
  names, error codes, project names, distinctive nouns — specific terms retrieve
  far better than vague ones.
- It matches **meaning, not exact keywords** — paraphrase the concept rather than
  guessing the exact wording the note used.
- One focused question per search. If you have several angles, run several
  searches rather than one broad one.

## Parameters

- `query` — the natural-language query.
- `k` — max results. Use **3–5** for a focused answer; go larger (10+) only when
  you're deliberately surveying what exists on a topic.
- `min` — drop results below this calibrated score. Use it when a wrong answer is
  costly (you'd rather get "nothing" than a weak hit). Omit to use the server
  default.

## Reading the results — the important part

Results are **spans**, not whole documents: each hit is a file path, the line
range it matched (`start_line`/`end_line`), a **calibrated** score, and a text
snippet.

- **Scores are calibrated.** A **low top score means there is no confident
  match.** When that happens, say "I didn't find anything relevant in your vault"
  rather than dressing up a weak hit as if it answered the question. Don't
  over-trust a marginal result.
- A strong top hit is a genuine grounding signal — use it.

## Chaining to full context

A snippet is a fragment. For real synthesis — summarizing a note, quoting
accurately, understanding surrounding context — **open the cited file** rather
than answering from the snippet alone.

Use **`read_doc`** with the hit's `path`, passing its `start_line` as `offset` to
land on the matched region. It needs no host filesystem access, so it works from
any MCP client. It returns text **verbatim** and reports the range separately as
`start_line`/`end_line`/`total_lines` — so if you go on to change the document,
what you read can be handed straight to `edit_doc` as `old_string`.

## Always cite

Cite the **path** and line for anything you draw from the vault, so the user can
open the source and verify. Grounded-and-cited beats confident-and-vague.
