// Package parentretriever wraps any Retriever to expand hits with full parent section text.
package parentretriever

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// Retriever wraps an inner retriever and expands each hit with the full parent section text.
type Retriever struct {
	inner types.Retriever
	store types.Store
}

var (
	_ types.Indexer        = (*Retriever)(nil)
	_ types.IndexRebuilder = (*Retriever)(nil)
)

// New creates a parent-context retriever wrapping the given inner retriever.
func New(inner types.Retriever, store types.Store) *Retriever {
	return &Retriever{inner: inner, store: store}
}

// Index forwards to the inner retriever when it implements types.Indexer, so
// wrapping a lexical retriever such as BM25 does not hide it from ingest. It
// is a no-op otherwise.
func (r *Retriever) Index(ctx context.Context, doc *types.Document) error {
	if indexer, ok := r.inner.(types.Indexer); ok {
		return indexer.Index(ctx, doc)
	}
	return nil
}

// Remove forwards to the inner retriever when it implements types.Indexer.
// It is a no-op otherwise.
func (r *Retriever) Remove(ctx context.Context, documentUUID string) error {
	if indexer, ok := r.inner.(types.Indexer); ok {
		return indexer.Remove(ctx, documentUUID)
	}
	return nil
}

// Name reports the inner retriever's name, so stats and weighted fusion see
// the wrapped retriever under the name it has without the wrapper. It is
// empty when the inner retriever has no name.
func (r *Retriever) Name() string {
	if n, ok := r.inner.(types.Named); ok {
		return n.Name()
	}
	return ""
}

// RebuildIndex forwards to the inner retriever when it implements
// types.IndexRebuilder. It is a no-op otherwise.
func (r *Retriever) RebuildIndex(ctx context.Context) error {
	if rebuilder, ok := r.inner.(types.IndexRebuilder); ok {
		return rebuilder.RebuildIndex(ctx)
	}
	return nil
}

// Retrieve calls the inner retriever, deduplicates by section UUID (keeping the highest score
// per section), then replaces each hit's text with the concatenation of all variant texts
// in the parent section. An expanded hit keeps the matching child's Variant.UUID and records
// it in Provenance.ExpandedFromVariantUUID, which the pipeline uses to merge hits from the
// same section across retrievers and queries.
//
// Sections are loaded once per document. When a document's sections cannot be loaded, its
// hits are returned unexpanded together with an error wrapping types.ErrPartialSearch.
func (r *Retriever) Retrieve(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	hits, err := r.inner.Retrieve(ctx, query, opts)
	if err != nil && (!errors.Is(err, types.ErrPartialSearch) || len(hits) == 0) {
		return nil, fmt.Errorf("inner retrieve: %w", err)
	}
	var errs []error
	if err != nil {
		errs = append(errs, fmt.Errorf("inner retrieve: %w", err))
	}

	if len(hits) == 0 {
		return hits, nil
	}

	// Dedupe by section UUID, keeping highest score. Hits without a section
	// keep their own identity.
	bestBySection := make(map[string]types.SearchHit)
	var order []string
	for _, hit := range hits {
		key := hit.Provenance.SectionUUID
		if key == "" {
			key = "variant:" + hit.Variant.UUID
		}
		existing, ok := bestBySection[key]
		if !ok {
			order = append(order, key)
		}
		if !ok || hit.Score > existing.Score {
			bestBySection[key] = hit
		}
	}

	// Expand each hit with full section text, loading each document's
	// sections once.
	type docSections struct {
		sections []types.Section
		err      error
	}
	loaded := make(map[string]docSections)
	result := make([]types.SearchHit, 0, len(bestBySection))
	for _, key := range order {
		hit := bestBySection[key]
		docUUID := hit.Provenance.DocumentUUID
		if hit.Provenance.SectionUUID == "" || docUUID == "" {
			result = append(result, hit)
			continue
		}
		ds, ok := loaded[docUUID]
		if !ok {
			ds.sections, ds.err = r.store.GetSections(ctx, docUUID)
			loaded[docUUID] = ds
			if ds.err != nil {
				errs = append(errs, fmt.Errorf("load sections of document %s: %w", docUUID, ds.err))
			}
		}
		if ds.err == nil {
			expandHit(&hit, ds.sections)
		}
		result = append(result, hit)
	}

	sort.SliceStable(result, func(i, j int) bool { return result[i].Score > result[j].Score })

	if len(errs) > 0 {
		return result, fmt.Errorf("%w: %w", types.ErrPartialSearch, errors.Join(errs...))
	}
	return result, nil
}

// expandHit replaces hit's text with the joined text of its parent section
// when that section is found and its text differs from the hit's. A section
// holding only the hit's own chunk, as the recursive chunker produces, leaves
// the hit unchanged and is not recorded as an expansion.
func expandHit(hit *types.SearchHit, sections []types.Section) {
	for _, sec := range sections {
		if sec.UUID != hit.Provenance.SectionUUID {
			continue
		}
		var texts []string
		offset := -1
		for _, v := range sec.Variants {
			if v.Text != "" {
				if v.UUID == hit.Variant.UUID {
					offset = len(strings.Join(texts, "\n\n"))
					if len(texts) > 0 {
						offset += len("\n\n")
					}
				}
				texts = append(texts, v.Text)
			}
		}
		joined := strings.Join(texts, "\n\n")
		if len(texts) == 0 || joined == hit.Variant.Text {
			return
		}
		hit.Variant.Text = joined
		hit.Provenance.ExpandedFromVariantUUID = hit.Variant.UUID
		if offset >= 0 {
			hit.Highlight = hit.Highlight.Shifted(offset)
		} else if hit.Highlight != nil {
			// The matched text is not part of the section text, so its
			// spans point nowhere in it.
			hit.Highlight = &types.Highlight{Snippet: hit.Highlight.Snippet}
		}
		return
	}
}
