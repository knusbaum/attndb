// Package pass defines the unit of composition in attndb. A pass ties together a
// Chunker, an Encoder, and a Pool, and for a query emits scored intervals
// ("deposits") onto the heatmap. Single-vector and per-token passes differ only
// in what they deposit; the accumulator treats them uniformly. Adding a level or
// trying a new chunking strategy is just adding or reconfiguring a pass.
package pass

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjn/attndb/internal/chunk"
	"github.com/kjn/attndb/internal/core"
	"github.com/kjn/attndb/internal/store"
)

// Pass is one indexing+retrieval lane over the corpus.
type Pass interface {
	Name() string
	// Weight scales this pass's deposits in the heatmap (down-weight near-clone
	// passes so they don't double-count one viewpoint).
	Weight() float64
	Ingest(ctx context.Context, docs []core.Document) error
	// Delete removes every record for docID from this pass's pool.
	Delete(ctx context.Context, docID string) error
	// Stamps returns per-doc change-detection stamps from this pass's pool.
	Stamps(ctx context.Context) (map[string]store.DocStamp, error)
	// Deposits returns this pass's scored intervals for a query (up to k).
	Deposits(ctx context.Context, q core.Query, k int) ([]core.Candidate, error)
}

type options struct {
	weight float64
}

// Option mutates pass options.
type Option func(*options)

// WithWeight sets the pass's heatmap weight (default 1).
func WithWeight(w float64) Option { return func(o *options) { o.weight = w } }

func resolve(opts []Option) options {
	o := options{weight: 1}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// --- SingleVectorPass ---

// SingleVectorPass chunks, encodes one vector per chunk, and deposits chunk
// spans scored by cosine similarity.
type SingleVectorPass struct {
	name string
	ch   chunk.Chunker
	enc  core.SingleVectorEncoder
	pool store.Pool
	opt  options
}

func NewSingleVectorPass(name string, ch chunk.Chunker, enc core.SingleVectorEncoder, pool store.Pool, opts ...Option) *SingleVectorPass {
	return &SingleVectorPass{name: name, ch: ch, enc: enc, pool: pool, opt: resolve(opts)}
}

func (p *SingleVectorPass) Name() string    { return p.name }
func (p *SingleVectorPass) Weight() float64 { return p.opt.weight }

func (p *SingleVectorPass) Delete(ctx context.Context, docID string) error {
	return p.pool.Delete(ctx, docID)
}

func (p *SingleVectorPass) Stamps(ctx context.Context) (map[string]store.DocStamp, error) {
	return p.pool.Stamps(ctx)
}

func (p *SingleVectorPass) Ingest(ctx context.Context, docs []core.Document) error {
	for _, d := range docs {
		chunks := p.ch.Chunk(d)
		if len(chunks) == 0 {
			continue
		}
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = encodeText(c)
		}
		vecs, err := p.enc.EncodeDocs(ctx, texts)
		if err != nil {
			return fmt.Errorf("%s: encode: %w", p.name, err)
		}
		recs := make([]store.Record, len(chunks))
		for i, c := range chunks {
			recs[i] = store.Record{
				ID:      recordID(p.name, c),
				DocID:   c.DocID,
				Span:    c.Span,
				Vector:  vecs[i],
				Payload: payload(d, c),
			}
		}
		if err := p.pool.Upsert(ctx, recs); err != nil {
			return err
		}
	}
	return nil
}

func (p *SingleVectorPass) Deposits(ctx context.Context, q core.Query, k int) ([]core.Candidate, error) {
	vec, err := p.enc.EncodeQuery(ctx, q.Text)
	if err != nil {
		return nil, err
	}
	hits, err := p.pool.SearchSingle(ctx, vec, k, q.Filters)
	if err != nil {
		return nil, err
	}
	out := make([]core.Candidate, len(hits))
	for i, h := range hits {
		out[i] = core.Candidate{DocID: h.Rec.DocID, Span: h.Rec.Span, Score: h.Score, Source: p.name}
	}
	return out, nil
}

// --- PerTokenPass ---

// PerTokenPass chunks, encodes per-token vectors, recalls by MaxSim, and
// deposits the refined best-matching sub-span of each hit (a narrow, high-density
// peak).
type PerTokenPass struct {
	name string
	ch   chunk.Chunker
	enc  core.MultiVectorEncoder
	pool store.Pool
	opt  options
}

func NewPerTokenPass(name string, ch chunk.Chunker, enc core.MultiVectorEncoder, pool store.Pool, opts ...Option) *PerTokenPass {
	return &PerTokenPass{name: name, ch: ch, enc: enc, pool: pool, opt: resolve(opts)}
}

func (p *PerTokenPass) Name() string    { return p.name }
func (p *PerTokenPass) Weight() float64 { return p.opt.weight }

func (p *PerTokenPass) Delete(ctx context.Context, docID string) error {
	return p.pool.Delete(ctx, docID)
}

func (p *PerTokenPass) Stamps(ctx context.Context) (map[string]store.DocStamp, error) {
	return p.pool.Stamps(ctx)
}

func (p *PerTokenPass) Ingest(ctx context.Context, docs []core.Document) error {
	for _, d := range docs {
		chunks := p.ch.Chunk(d)
		if len(chunks) == 0 {
			continue
		}
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = encodeText(c)
		}
		tvs, err := p.enc.EncodeDocs(ctx, texts)
		if err != nil {
			return fmt.Errorf("%s: encode: %w", p.name, err)
		}
		recs := make([]store.Record, len(chunks))
		for i, c := range chunks {
			recs[i] = store.Record{
				ID:      recordID(p.name, c),
				DocID:   c.DocID,
				Span:    c.Span,
				Tokens:  toDocCoords(tvs[i], c, texts[i]),
				Payload: payload(d, c),
			}
		}
		if err := p.pool.Upsert(ctx, recs); err != nil {
			return err
		}
	}
	return nil
}

func (p *PerTokenPass) Deposits(ctx context.Context, q core.Query, k int) ([]core.Candidate, error) {
	qt, err := p.enc.EncodeQuery(ctx, q.Text)
	if err != nil {
		return nil, err
	}
	hits, err := p.pool.SearchMulti(ctx, qt, k, q.Filters)
	if err != nil {
		return nil, err
	}
	// MaxSim sums one best-match similarity per query token, so the raw score
	// scales with query length. Divide by the query token count to get the mean
	// per-token similarity — a length-invariant score, so calibration learned
	// from (long) pseudo-queries transfers to (short) real queries instead of
	// flooring every candidate to zero.
	norm := float32(1)
	if n := len(qt.Vecs); n > 0 {
		norm = float32(n)
	}
	out := make([]core.Candidate, 0, len(hits))
	for _, h := range hits {
		span := h.Rec.Span
		if _, matched := core.MaxSim(qt, h.Rec.Tokens); len(matched) > 0 {
			if s := matchedSpan(h.Rec.Tokens, matched); s.Len() > 0 {
				span = s // refine to the best-matching sub-span
			}
		}
		out = append(out, core.Candidate{DocID: h.Rec.DocID, Span: span, Score: h.Score / norm, Source: p.name})
	}
	return out, nil
}

// --- shared helpers ---

func encodeText(c core.Chunk) string {
	if len(c.HeadingPath) == 0 {
		return c.Text
	}
	// Prepend the heading trail as context. Offsets shift by len(prefix); the
	// per-token pass translates them back to document coordinates in toDocCoords.
	return strings.Join(c.HeadingPath, " › ") + "\n\n" + c.Text
}

// toDocCoords rewrites token offsets from encoded-text coordinates into document
// coordinates and drops tokens that fall inside the heading-trail prefix (we keep
// heading context during encoding but don't store heading tokens as separately
// matchable vectors). encText is the exact string passed to the encoder.
func toDocCoords(tv core.TokenVecs, c core.Chunk, encText string) core.TokenVecs {
	prefix := len(encText) - len(c.Text)
	if prefix < 0 {
		prefix = 0
	}
	shift := c.Span.Start - prefix
	var out core.TokenVecs
	for i, off := range tv.Offsets {
		if off.Start < prefix {
			continue
		}
		out.Vecs = append(out.Vecs, tv.Vecs[i])
		out.Offsets = append(out.Offsets, core.Span{Start: off.Start + shift, End: off.End + shift})
	}
	return out
}

// matchedSpan returns the bounding document span of the document tokens that were
// the best match for some query token.
func matchedSpan(d core.TokenVecs, matched []int) core.Span {
	start, end := -1, -1
	for _, j := range matched {
		if j < 0 || j >= len(d.Offsets) {
			continue
		}
		o := d.Offsets[j]
		if start < 0 || o.Start < start {
			start = o.Start
		}
		if o.End > end {
			end = o.End
		}
	}
	if start < 0 {
		return core.Span{}
	}
	return core.Span{Start: start, End: end}
}

func recordID(pass string, c core.Chunk) string {
	return fmt.Sprintf("%s:%s:%d-%d", pass, c.DocID, c.Span.Start, c.Span.End)
}

func payload(d core.Document, c core.Chunk) map[string]any {
	m := map[string]any{"doc_id": d.ID}
	for k, v := range d.Meta {
		m[k] = v
	}
	if len(c.HeadingPath) > 0 {
		m["heading"] = strings.Join(c.HeadingPath, " › ")
	}
	return m
}
