// Package chunk provides pluggable chunking strategies. A Chunker splits a
// Document into Chunks, each carrying the byte Span of its body so that chunks
// from different strategies share one coordinate system.
package chunk

import (
	"strings"

	"github.com/kjn/attndb/internal/core"
)

// Chunker splits a document into chunks.
type Chunker interface {
	Chunk(doc core.Document) []core.Chunk
}

// ChunkerFunc adapts a plain function to the Chunker interface.
type ChunkerFunc func(core.Document) []core.Chunk

func (f ChunkerFunc) Chunk(d core.Document) []core.Chunk { return f(d) }

// WholeDoc emits a single chunk covering the entire document. Use for a
// document-level single-vector pass (coarse topical recall).
func WholeDoc() Chunker {
	return ChunkerFunc(func(d core.Document) []core.Chunk {
		return []core.Chunk{{
			DocID: d.ID,
			Span:  core.Span{Start: 0, End: len(d.Text)},
			Text:  d.Text,
		}}
	})
}

// ByParagraph splits on blank lines. Use for the primary paragraph-level
// single-vector pass (the diffuse-meaning workhorse).
func ByParagraph() Chunker {
	return ChunkerFunc(func(d core.Document) []core.Chunk {
		var chunks []core.Chunk
		for _, sp := range splitBlankLines(d.Text) {
			body := d.Text[sp.Start:sp.End]
			if strings.TrimSpace(body) == "" {
				continue
			}
			chunks = append(chunks, core.Chunk{DocID: d.ID, Span: sp, Text: body})
		}
		return chunks
	})
}

// FixedWindow emits whitespace-token windows of the given size with the given
// overlap (both measured in tokens). A general-purpose per-token chunker.
func FixedWindow(size, overlap int) Chunker {
	if size <= 0 {
		size = 256
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}
	return ChunkerFunc(func(d core.Document) []core.Chunk {
		toks := tokenize(d.Text)
		return window(d, toks, size, overlap, nil)
	})
}

// BySection parses Markdown ATX headings and emits one chunk per section body,
// tagged with its heading path. Sections longer than maxTokens are split into
// overlapping windows. This is the recommended per-token chunker: large,
// topically coherent, section-aligned.
func BySection(maxTokens, overlap int) Chunker {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return ChunkerFunc(func(d core.Document) []core.Chunk {
		secs := parseSections(d.Text)
		var chunks []core.Chunk
		for _, sec := range secs {
			body := d.Text[sec.span.Start:sec.span.End]
			if strings.TrimSpace(body) == "" {
				continue
			}
			toks := tokenizeOffset(body, sec.span.Start)
			if len(toks) <= maxTokens {
				chunks = append(chunks, core.Chunk{
					DocID:       d.ID,
					Span:        sec.span,
					Text:        body,
					HeadingPath: sec.path,
				})
				continue
			}
			chunks = append(chunks, window(d, toks, maxTokens, overlap, sec.path)...)
		}
		return chunks
	})
}

// --- helpers ---

type token struct {
	span core.Span // byte range in the document
}

// tokenize splits text on whitespace, recording byte spans relative to text.
func tokenize(text string) []token { return tokenizeOffset(text, 0) }

// tokenizeOffset is like tokenize but adds base to every span (so spans are in
// document coordinates when text is a sub-slice starting at base).
func tokenizeOffset(text string, base int) []token {
	var toks []token
	start := -1
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if start >= 0 {
				toks = append(toks, token{core.Span{Start: base + start, End: base + i}})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		toks = append(toks, token{core.Span{Start: base + start, End: base + len(text)}})
	}
	return toks
}

// window groups tokens into overlapping windows and emits a chunk per window,
// spanning from the first token's start to the last token's end.
func window(d core.Document, toks []token, size, overlap int, path []string) []core.Chunk {
	var chunks []core.Chunk
	step := size - overlap
	if step <= 0 {
		step = size
	}
	for i := 0; i < len(toks); i += step {
		j := min(i+size, len(toks))
		sp := core.Span{Start: toks[i].span.Start, End: toks[j-1].span.End}
		chunks = append(chunks, core.Chunk{
			DocID:       d.ID,
			Span:        sp,
			Text:        d.Text[sp.Start:sp.End],
			HeadingPath: path,
		})
		if j == len(toks) {
			break
		}
	}
	return chunks
}

// splitBlankLines returns the spans of text between blank-line separators.
func splitBlankLines(text string) []core.Span {
	var spans []core.Span
	start := 0
	i := 0
	for i < len(text) {
		// a blank-line boundary is "\n\n" (optionally with spaces between)
		if text[i] == '\n' {
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			if j < len(text) && text[j] == '\n' {
				if start < i {
					spans = append(spans, core.Span{Start: start, End: i})
				}
				start = j + 1
				i = j + 1
				continue
			}
		}
		i++
	}
	if start < len(text) {
		spans = append(spans, core.Span{Start: start, End: len(text)})
	}
	return spans
}

type section struct {
	span core.Span
	path []string
}

// parseSections splits Markdown text into sections by ATX headings, tracking the
// heading hierarchy as a path. The body of a section is the content between its
// heading and the next heading of equal-or-higher level.
func parseSections(text string) []section {
	type head struct {
		level int
		title string
	}
	var stack []head
	var secs []section
	var bodyStart int
	var curPath []string

	flush := func(end int) {
		if end > bodyStart {
			secs = append(secs, section{
				span: core.Span{Start: bodyStart, End: end},
				path: append([]string(nil), curPath...),
			})
		}
	}

	lineStart := 0
	for lineStart <= len(text) {
		nl := strings.IndexByte(text[lineStart:], '\n')
		var line string
		var lineEnd int
		if nl < 0 {
			line = text[lineStart:]
			lineEnd = len(text)
		} else {
			line = text[lineStart : lineStart+nl]
			lineEnd = lineStart + nl
		}
		if lvl, title, ok := atxHeading(line); ok {
			flush(lineStart)
			for len(stack) > 0 && stack[len(stack)-1].level >= lvl {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, head{lvl, title})
			curPath = curPath[:0]
			for _, h := range stack {
				curPath = append(curPath, h.title)
			}
			curPath = append([]string(nil), curPath...)
			bodyStart = lineEnd + 1
		}
		if nl < 0 {
			break
		}
		lineStart = lineEnd + 1
	}
	flush(len(text))
	return secs
}

// atxHeading parses a Markdown ATX heading line ("## Title").
func atxHeading(line string) (level int, title string, ok bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return 0, "", false
	}
	return i, strings.TrimSpace(line[i+1:]), true
}
