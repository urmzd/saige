// Package graphretriever implements a Retriever backed by a knowledge graph.
package graphretriever

import (
	"context"
	"errors"
	"fmt"
	"slices"

	knowledgetypes "github.com/urmzd/saige/rag/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// filteredFetchFactor is how many more facts Retrieve requests from the
// graph when filters may drop some of them.
const filteredFetchFactor = 4

// Retriever retrieves search hits by searching a knowledge graph for facts
// and resolving their provenance back to document variants.
type Retriever struct {
	graph   knowledgetypes.Graph
	store   ragtypes.Store
	groupID string
}

// Option configures a Retriever.
type Option func(*Retriever)

// WithGroupID scopes the graph search to one group (tenant namespace). The
// host supplies it; an empty value searches every group.
func WithGroupID(id string) Option {
	return func(r *Retriever) { r.groupID = id }
}

// New creates a graph retriever with the given knowledge graph and document store.
func New(graph knowledgetypes.Graph, store ragtypes.Store, opts ...Option) *Retriever {
	r := &Retriever{graph: graph, store: store}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Retrieve searches the knowledge graph for facts matching the query and
// builds SearchHits.
//
// Each fact resolves to the document variant its provenance episode came
// from. Episodes written by the rag pipeline carry the variant UUID in their
// metadata; older episodes are matched by section heading. ContentTypes and
// MetadataFilters apply to the resolved variant. A fact whose provenance
// names no variant or document becomes a synthetic text hit from the fact
// itself, unless filters are set or ContentTypes excludes text: unresolved
// facts cannot be checked against filters, so they are dropped. A fact whose
// provenance lookup fails, or whose named source is missing or rejected, is
// dropped too, since it may belong to another scope.
//
// Hits are scored by graph rank as 1/(61+rank), so scores are small and
// decrease with rank; a MinScore set by a direct caller compares against
// that scale. The rag pipeline applies MinScore to fused scores instead.
//
// When the graph search or a provenance lookup fails partially, the
// surviving hits are returned with an error wrapping
// ragtypes.ErrPartialSearch.
func (r *Retriever) Retrieve(ctx context.Context, query string, opts *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	if opts == nil {
		opts = &ragtypes.SearchOptions{}
	}
	limit := 10
	if opts.Limit > 0 {
		limit = opts.Limit
	}

	fetch := limit
	if len(opts.MetadataFilters) > 0 || len(opts.ContentTypes) > 0 {
		fetch = limit * filteredFetchFactor
	}
	searchOpts := []knowledgetypes.SearchOption{knowledgetypes.WithLimit(fetch)}
	if r.groupID != "" {
		searchOpts = append(searchOpts, knowledgetypes.WithGroupID(r.groupID))
	}

	var partial []error
	result, err := r.graph.SearchFacts(ctx, query, searchOpts...)
	if err != nil {
		if !errors.Is(err, knowledgetypes.ErrPartialSearch) || result == nil {
			return nil, fmt.Errorf("search facts: %w", err)
		}
		partial = append(partial, err)
	}

	res := &resolver{r: r, opts: opts, sections: make(map[string][]ragtypes.Section)}
	hits := make([]ragtypes.SearchHit, 0, min(limit, len(result.Facts)))
	for rank, fact := range result.Facts {
		if len(hits) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		score := 1.0 / float64(60+rank+1)
		if opts.MinScore > 0 && score < opts.MinScore {
			break // scores only decrease with rank
		}

		hit, ok, err := res.resolve(ctx, fact, score)
		if err != nil {
			partial = append(partial, err)
		}
		if ok {
			hits = append(hits, hit)
		}
	}

	if len(partial) > 0 {
		return hits, fmt.Errorf("%w: graph: %w", ragtypes.ErrPartialSearch, errors.Join(partial...))
	}
	return hits, nil
}

// resolver resolves facts for one Retrieve call, caching each document's
// sections for the heading fallback.
type resolver struct {
	r        *Retriever
	opts     *ragtypes.SearchOptions
	sections map[string][]ragtypes.Section
}

// filtered reports whether hits must be checked against metadata filters.
func (res *resolver) filtered() bool {
	return len(res.opts.MetadataFilters) > 0
}

// syntheticAllowed reports whether a hit built from fact text alone, with no
// stored document behind it, may satisfy the scope and time range.
func (res *resolver) syntheticAllowed() bool {
	if !res.opts.Since.IsZero() || !res.opts.Until.IsZero() {
		return false
	}
	return res.opts.Scope == "" || res.opts.Scope == res.r.groupID
}

// allowsType reports whether ContentTypes admits ct.
func (res *resolver) allowsType(ct ragtypes.ContentType) bool {
	return len(res.opts.ContentTypes) == 0 || slices.Contains(res.opts.ContentTypes, ct)
}

// resolve maps a fact to a hit. ok is false when the fact must be dropped.
// err reports a store or graph failure other than a missing variant.
func (res *resolver) resolve(ctx context.Context, fact knowledgetypes.Fact, score float64) (hit ragtypes.SearchHit, ok bool, err error) {
	var errs []error
	episodes, perr := res.r.graph.GetFactProvenance(ctx, fact.UUID)
	if perr != nil {
		errs = append(errs, fmt.Errorf("provenance %s: %w", fact.UUID, perr))
	}
	// attributed is set when provenance names a stored source for the fact.
	// Such a fact belongs to that source's scope, so when the source cannot
	// be resolved the fact is dropped rather than shown as bare text.
	attributed := perr != nil
	for _, ep := range episodes {
		if ep.Metadata["variant_uuid"] != "" || ep.DocumentID != "" {
			attributed = true
		}
		for _, variantUUID := range res.candidates(ctx, ep, &errs) {
			rec, found, lerr := res.lookup(ctx, variantUUID)
			if lerr != nil {
				errs = append(errs, lerr)
				continue
			}
			if !found {
				continue
			}
			if !res.accepts(rec) {
				continue
			}
			prov := rec.Provenance
			if prov.SourceURI == "" {
				prov.SourceURI = ep.Source
			}
			return ragtypes.SearchHit{
				Variant:    rec.Variant,
				Score:      score,
				Provenance: prov,
				Timestamp:  rec.Timestamp,
			}, true, errors.Join(errs...)
		}
	}

	// Fallback: build hit from fact text with synthetic provenance. Only a
	// fact with no attributed source qualifies: one whose provenance lookup
	// failed, or whose episodes name a variant or document that is missing
	// or fails the search conditions, may belong to another scope and is
	// dropped. The synthetic hit has no document metadata or time, so it is
	// dropped whenever filters or a time range are set. It is scoped by the
	// graph group alone, so it is kept for a scoped search only when the
	// scope is that group.
	if attributed || res.filtered() || !res.allowsType(ragtypes.ContentText) || !res.syntheticAllowed() {
		return ragtypes.SearchHit{}, false, errors.Join(errs...)
	}
	return ragtypes.SearchHit{
		Variant: ragtypes.ContentVariant{
			UUID:        fact.UUID,
			ContentType: ragtypes.ContentText,
			Text:        fact.FactText,
		},
		Score: score,
		Provenance: ragtypes.Provenance{
			DocumentTitle: "Knowledge Graph",
		},
	}, true, errors.Join(errs...)
}

// candidates returns the variant UUIDs an episode may refer to. Episodes
// with a variant_uuid in their metadata name it directly. Older episodes are
// matched to text variants of the section whose heading (or "section-N"
// name) equals the episode name.
func (res *resolver) candidates(ctx context.Context, ep knowledgetypes.Episode, errs *[]error) []string {
	if v := ep.Metadata["variant_uuid"]; v != "" {
		return []string{v}
	}
	docID := ep.DocumentID
	if docID == "" {
		docID = ep.GroupID // older ingests used the document UUID as group
	}
	if docID == "" {
		return nil
	}
	sections, ok := res.sections[docID]
	if !ok {
		var err error
		sections, err = res.r.store.GetSections(ctx, docID)
		if err != nil {
			sections = nil
			if !errors.Is(err, ragtypes.ErrDocumentNotFound) {
				*errs = append(*errs, fmt.Errorf("sections %s: %w", docID, err))
			}
		}
		res.sections[docID] = sections
	}
	var out []string
	for _, sec := range sections {
		if sec.Heading != ep.Name && fmt.Sprintf("section-%d", sec.Index) != ep.Name {
			continue
		}
		for _, v := range sec.Variants {
			if v.ContentType == ragtypes.ContentText {
				out = append(out, v.UUID)
			}
		}
	}
	return out
}

// lookup loads a variant with its document's scope, time, and metadata.
// found is false for a variant, or owning document, that no longer exists.
func (res *resolver) lookup(ctx context.Context, variantUUID string) (*ragtypes.VariantRecord, bool, error) {
	rec, err := ragtypes.LoadVariantRecord(ctx, res.r.store, variantUUID)
	if errors.Is(err, ragtypes.ErrVariantNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("variant %s: %w", variantUUID, err)
	}
	return rec, true, nil
}

// accepts applies the scope, time range, ContentTypes, and MetadataFilters
// to a resolved variant. Filters see document metadata merged with variant
// metadata, variant keys taking precedence, matching the stores' semantics.
func (res *resolver) accepts(rec *ragtypes.VariantRecord) bool {
	return res.opts.Admits(rec)
}

// Name reports "graph", implementing ragtypes.Named.
func (r *Retriever) Name() string { return "graph" }
