// Package graphretriever implements a Retriever backed by a knowledge graph.
package graphretriever

import (
	"context"
	"errors"
	"fmt"

	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Retriever retrieves search hits by searching a knowledge graph for facts
// and resolving their provenance back to document variants via episode source/name.
type Retriever struct {
	graph knowledgetypes.Graph
	store ragtypes.Store
}

// New creates a graph retriever with the given knowledge graph and document store.
func New(graph knowledgetypes.Graph, store ragtypes.Store) *Retriever {
	return &Retriever{graph: graph, store: store}
}

// Retrieve searches the knowledge graph for facts matching the query and builds
// SearchHits. It attempts to resolve provenance episodes back to store variants
// by matching episode GroupID (document UUID) and Name (section heading).
// Falls back to synthetic hits from fact text when resolution fails.
func (r *Retriever) Retrieve(ctx context.Context, query string, opts *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	var searchOpts []knowledgetypes.SearchOption
	result, err := r.graph.SearchFacts(ctx, query, searchOpts...)
	if err != nil && !errors.Is(err, knowledgetypes.ErrPartialSearch) {
		return nil, fmt.Errorf("search facts: %w", err)
	}

	hits := make([]ragtypes.SearchHit, 0, len(result.Facts))
	for rank, fact := range result.Facts {
		score := 1.0 / float64(60+rank+1)
		if opts != nil && opts.MinScore > 0 && score < opts.MinScore {
			continue
		}

		hit := r.resolveFactToHit(ctx, fact, score, opts)
		hits = append(hits, hit)
	}

	if len(hits) > limit {
		hits = hits[:limit]
	}

	return hits, nil
}

// resolveFactToHit tries to resolve a fact back to a store variant via provenance episodes.
// Falls back to a synthetic hit from the fact text.
func (r *Retriever) resolveFactToHit(ctx context.Context, fact knowledgetypes.Fact, score float64, opts *ragtypes.SearchOptions) ragtypes.SearchHit {
	episodes, err := r.graph.GetFactProvenance(ctx, fact.UUID)
	if err == nil {
		for _, ep := range episodes {
			// Episode.GroupID is the document UUID (set during ingest).
			// Try to find the variant by looking up sections in that document.
			if ep.GroupID == "" {
				continue
			}
			sections, err := r.store.GetSections(ctx, ep.GroupID)
			if err != nil {
				continue
			}
			for _, sec := range sections {
				// Match by section heading or name.
				if sec.Heading != ep.Name && fmt.Sprintf("section-%d", sec.Index) != ep.Name {
					continue
				}
				for _, v := range sec.Variants {
					if v.ContentType != ragtypes.ContentText {
						continue
					}
					// Apply content type filters.
					if opts != nil && len(opts.ContentTypes) > 0 {
						match := false
						for _, ct := range opts.ContentTypes {
							if v.ContentType == ct {
								match = true
								break
							}
						}
						if !match {
							continue
						}
					}
					return ragtypes.SearchHit{
						Variant: v,
						Score:   score,
						Provenance: ragtypes.Provenance{
							DocumentUUID:   ep.GroupID,
							SectionUUID:    sec.UUID,
							SectionHeading: sec.Heading,
							SectionIndex:   sec.Index,
							SourceURI:      ep.Source,
						},
					}
				}
			}
		}
	}

	// Fallback: build hit from fact text with synthetic provenance.
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
	}
}
