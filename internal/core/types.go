// Package core holds the shared domain types and vector math used across all
// passes. Everything in attndb is anchored to a (DocID, Span) coordinate so
// that passes which chunk the corpus differently still produce intervals that
// accumulate on a common axis.
package core

// Span is a half-open [Start,End) byte range into a Document's Text.
type Span struct {
	Start int
	End   int
}

// Len returns the byte length of the span.
func (s Span) Len() int { return s.End - s.Start }

// Overlaps reports whether two spans share any bytes.
func (s Span) Overlaps(o Span) bool { return s.Start < o.End && o.Start < s.End }

// Union returns the smallest span covering both s and o.
func (s Span) Union(o Span) Span {
	return Span{Start: min(s.Start, o.Start), End: max(s.End, o.End)}
}

// Document is the unit of ingestion.
type Document struct {
	ID   string
	Text string
	Meta map[string]any
	// Sections is an optional structural parse (heading bodies) used by
	// structure-aware chunkers. May be nil.
	Sections []Span
}

// Chunk is a slice of a Document produced by a Chunker. Text is what actually
// gets encoded (it may include a heading-trail prefix); Span is the byte range
// of the chunk body in the parent Document.
type Chunk struct {
	DocID       string
	Span        Span
	Text        string
	HeadingPath []string
}

// TokenVecs is the output of a MultiVectorEncoder: one vector per token plus the
// byte offset of each token within the encoded text.
type TokenVecs struct {
	Vecs    [][]float32
	Offsets []Span
}

// Len returns the number of token vectors.
func (t TokenVecs) Len() int { return len(t.Vecs) }

// Query is a search request. Filters constrain candidates by payload.
type Query struct {
	Text    string
	Filters map[string]any
}

// Candidate is a scored interval emitted by a pass for a query (a "deposit"
// before normalization): the document region the pass matched and its raw
// likeness score.
type Candidate struct {
	DocID  string
	Span   Span
	Score  float32
	Source string // pass name that produced it
}

// Result is a final, ranked, localized search hit (a heatmap peak). Snippet
// rendering is the caller's job (slice the document by Span) — the DB does not
// hold document text.
type Result struct {
	DocID string
	Span  Span
	Score float32
}
