package types

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// CitationKind says where a citation came from, because the provenance
// guarantees differ and a consumer usually wants to render them differently.
type CitationKind string

const (
	// CitationWeb: a page returned by a web search, whether the search ran
	// provider-side (Anthropic web_search, Gemini google_search grounding,
	// OpenAI web_search) or locally through one of this SDK's tools.
	CitationWeb CitationKind = "web"
	// CitationDocument: a document supplied in the request, e.g. an Anthropic
	// document block with citations enabled.
	CitationDocument CitationKind = "document"
	// CitationTool: a source a locally-executed tool attributed its output to.
	CitationTool CitationKind = "tool"
	// CitationRetrieval: a chunk from a RAG or knowledge-graph retriever.
	CitationRetrieval CitationKind = "retrieval"
)

// Citation is one attribution: a span of the answer, and the source it came
// from.
//
// It is deliberately one type for every producer. Provider-native search,
// locally-executed search tools, and RAG retrieval all attribute claims to
// sources, and a consumer that had to handle three shapes would end up
// rendering three different footnote schemes in one answer.
type Citation struct {
	// Ordinal is the marker number ("[1]"), assigned by a CitationRegistry.
	// Zero means unregistered: producers leave it zero and the registry fills
	// it, so numbering is stable and shared across every source in a run.
	Ordinal int `json:"ordinal,omitempty"`
	// Kind records the provenance class.
	Kind CitationKind `json:"kind,omitempty"`
	// URI is the source location. Empty for sources that have none, in which
	// case Title carries the identity.
	URI string `json:"uri,omitempty"`
	// Title is the human-readable source name.
	Title string `json:"title,omitempty"`
	// Quote is the exact cited text from the source, when the producer supplies
	// one. Providers that return quotes make verification cheap; those that do
	// not leave this empty.
	Quote string `json:"quote,omitempty"`
	// Start and End bound the cited span within the assistant's text. Both are
	// -1 when the producer does not report a span, which is the common case:
	// most providers attribute a whole message, not a character range.
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	// Producer names what emitted this: a provider name ("anthropic"), or a
	// tool name ("web_search"). Kept so a mixed answer can be audited.
	Producer string `json:"producer,omitempty"`
	// Meta carries producer-specific extras (search rank, page number, chunk
	// ID) without forcing them into this struct.
	Meta map[string]any `json:"meta,omitempty"`
}

// NewCitation builds a citation with no reported span, the common case.
func NewCitation(kind CitationKind, uri, title string) Citation {
	return Citation{Kind: kind, URI: uri, Title: title, Start: -1, End: -1}
}

// HasSpan reports whether the citation identifies a character range in the
// answer rather than attributing the message as a whole.
func (c Citation) HasSpan() bool { return c.Start >= 0 && c.End > c.Start }

// Marker renders the reference marker, e.g. "[3]". Unregistered citations have
// no number yet and render empty rather than "[0]".
func (c Citation) Marker() string {
	if c.Ordinal <= 0 {
		return ""
	}
	return fmt.Sprintf("[%d]", c.Ordinal)
}

// sourceKey is the identity a registry deduplicates on. Two citations of the
// same page with different quotes are one source with two quotes, not two
// sources: a bibliography that listed the same URL twice would be wrong.
func (c Citation) sourceKey() string {
	if c.URI != "" {
		return strings.ToLower(c.URI)
	}
	return string(c.Kind) + "\x00" + strings.ToLower(c.Title)
}

// ── Registry ────────────────────────────────────────────────────────

// CitationRegistry assigns stable, gap-free ordinals to sources across a whole
// run and remembers every quote attributed to each.
//
// One registry per run is the point. Provider-native web search, a local search
// tool, and a RAG retriever can all cite the same page during one answer; each
// producer registers what it found, and the registry decides that they are one
// source and hands back the same number. Without it each producer would invent
// its own numbering and the rendered answer would contain three conflicting
// footnote schemes.
//
// Safe for concurrent use: tools are fanned out in parallel goroutines and all
// of them may cite.
type CitationRegistry struct {
	mu      sync.RWMutex
	byKey   map[string]int // source key -> ordinal
	sources []Citation     // index = ordinal-1; the canonical source record
	quotes  map[int][]Citation
	nextOrd int
}

// NewCitationRegistry returns an empty registry.
func NewCitationRegistry() *CitationRegistry {
	return &CitationRegistry{
		byKey:   map[string]int{},
		quotes:  map[int][]Citation{},
		nextOrd: 1,
	}
}

// Add registers a citation and returns it with its Ordinal filled in. A source
// already seen keeps its existing number; a citation carrying a quote or span
// is also recorded against that source so Quotes can return it later.
//
// A citation with neither URI nor Title has no identity to deduplicate on and
// is rejected: it would produce a footnote that points nowhere.
func (r *CitationRegistry) Add(c Citation) (Citation, bool) {
	if c.URI == "" && c.Title == "" {
		return c, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := c.sourceKey()
	ord, seen := r.byKey[key]
	if !seen {
		ord = r.nextOrd
		r.nextOrd++
		r.byKey[key] = ord
		canonical := c
		canonical.Ordinal = ord
		canonical.Quote = "" // the source record holds identity, not one quote
		canonical.Start, canonical.End = -1, -1
		r.sources = append(r.sources, canonical)
	} else if r.sources[ord-1].Title == "" && c.Title != "" {
		// A later mention may carry the title an earlier bare URL lacked.
		r.sources[ord-1].Title = c.Title
	}

	c.Ordinal = ord
	if c.Quote != "" || c.HasSpan() {
		r.quotes[ord] = append(r.quotes[ord], c)
	}
	return c, true
}

// AddAll registers many citations, returning them with ordinals assigned.
// Citations with no identity are dropped rather than silently numbered.
func (r *CitationRegistry) AddAll(cs []Citation) []Citation {
	out := make([]Citation, 0, len(cs))
	for _, c := range cs {
		if reg, ok := r.Add(c); ok {
			out = append(out, reg)
		}
	}
	return out
}

// Sources returns one record per distinct source, ordered by ordinal. This is
// the bibliography.
func (r *CitationRegistry) Sources() []Citation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Citation, len(r.sources))
	copy(out, r.sources)
	return out
}

// Quotes returns every quoted or spanned citation recorded for a source.
func (r *CitationRegistry) Quotes(ordinal int) []Citation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Citation, len(r.quotes[ordinal]))
	copy(out, r.quotes[ordinal])
	return out
}

// Lookup returns the source record for an ordinal.
func (r *CitationRegistry) Lookup(ordinal int) (Citation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ordinal < 1 || ordinal > len(r.sources) {
		return Citation{}, false
	}
	return r.sources[ordinal-1], true
}

// Ordinal returns the number already assigned to a source, without adding it.
func (r *CitationRegistry) Ordinal(c Citation) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ord, ok := r.byKey[c.sourceKey()]
	return ord, ok
}

// Len returns the number of distinct sources.
func (r *CitationRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sources)
}

// Bibliography renders the sources as numbered lines, in ordinal order. It is
// the one rendering worth sharing: every consumer wants "[1] Title - URI".
func (r *CitationRegistry) Bibliography() string {
	sources := r.Sources()
	sort.Slice(sources, func(i, j int) bool { return sources[i].Ordinal < sources[j].Ordinal })
	var b strings.Builder
	for _, s := range sources {
		b.WriteString(s.Marker())
		if s.Title != "" {
			b.WriteString(" " + s.Title)
		}
		if s.URI != "" {
			b.WriteString(" - " + s.URI)
		}
		b.WriteString("\n")
	}
	return b.String()
}
