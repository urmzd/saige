// Package memstore provides an in-memory implementation of types.Store.
package memstore

import (
	"context"
	"maps"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/rag/types"
)

var (
	_ types.Store                = (*Store)(nil)
	_ types.DocumentReplacer     = (*Store)(nil)
	_ types.VariantRecordGetter  = (*Store)(nil)
	_ types.VariantRecordsGetter = (*Store)(nil)
	_ types.DocumentLister       = (*Store)(nil)
	_ types.SourceFinder         = (*Store)(nil)
	_ types.SourceLister         = (*Store)(nil)
)

// Store is a thread-safe in-memory document store with brute-force cosine similarity search.
type Store struct {
	mu           sync.RWMutex
	docs         map[string]*types.Document
	originals    map[string][]byte
	fingerprints map[string]string // fingerprint -> doc UUID
}

// New creates a new in-memory store.
func New() *Store {
	return &Store{
		docs:         make(map[string]*types.Document),
		originals:    make(map[string][]byte),
		fingerprints: make(map[string]string),
	}
}

// CreateDocument stores doc. It returns types.ErrDuplicateDocument, and
// stores nothing, when another document already holds doc's non-empty
// fingerprint, matching the unique fingerprint index of the PostgreSQL store.
func (s *Store) CreateDocument(_ context.Context, doc *types.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.fingerprints[doc.Fingerprint]; ok && doc.Fingerprint != "" && owner != doc.UUID {
		return types.ErrDuplicateDocument
	}
	s.docs[doc.UUID] = doc
	if doc.Fingerprint != "" {
		s.fingerprints[doc.Fingerprint] = doc.UUID
	}
	return nil
}

func (s *Store) GetDocument(_ context.Context, uuid string) (*types.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[uuid]
	if !ok {
		return nil, types.ErrDocumentNotFound
	}
	return doc, nil
}

func (s *Store) FindByFingerprint(_ context.Context, fingerprint string) (*types.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uuid, ok := s.fingerprints[fingerprint]
	if !ok {
		return nil, types.ErrDocumentNotFound
	}
	return s.docs[uuid], nil
}

// ReplaceDocument atomically swaps the document identified by oldUUID for doc
// under a single lock, implementing types.DocumentReplacer: readers never
// observe a state where the old document is gone but the new one is absent.
//
// It returns types.ErrDuplicateDocument, leaving both documents untouched,
// when doc's fingerprint belongs to a third document.
func (s *Store) ReplaceDocument(_ context.Context, oldUUID string, doc *types.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.fingerprints[doc.Fingerprint]; ok && doc.Fingerprint != "" && owner != oldUUID && owner != doc.UUID {
		return types.ErrDuplicateDocument
	}
	if old, ok := s.docs[oldUUID]; ok {
		delete(s.fingerprints, old.Fingerprint)
		delete(s.docs, oldUUID)
		delete(s.originals, oldUUID)
	}
	s.docs[doc.UUID] = doc
	if doc.Fingerprint != "" {
		s.fingerprints[doc.Fingerprint] = doc.UUID
	}
	return nil
}

func (s *Store) DeleteDocument(_ context.Context, uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[uuid]
	if ok {
		delete(s.fingerprints, doc.Fingerprint)
		delete(s.docs, uuid)
		delete(s.originals, uuid)
	}
	return nil
}

func (s *Store) StoreOriginal(_ context.Context, documentUUID string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.originals[documentUUID] = data
	return nil
}

func (s *Store) GetOriginal(_ context.Context, documentUUID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.originals[documentUUID]
	if !ok {
		return nil, types.ErrDocumentNotFound
	}
	return data, nil
}

func (s *Store) CreateSection(_ context.Context, section *types.Section) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[section.DocumentUUID]
	if !ok {
		return types.ErrDocumentNotFound
	}
	doc.Sections = append(doc.Sections, *section)
	return nil
}

func (s *Store) GetSections(_ context.Context, documentUUID string) ([]types.Section, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[documentUUID]
	if !ok {
		return nil, types.ErrDocumentNotFound
	}
	return doc.Sections, nil
}

func (s *Store) CreateVariant(_ context.Context, variant *types.ContentVariant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, doc := range s.docs {
		for j, sec := range doc.Sections {
			if sec.UUID == variant.SectionUUID {
				s.docs[i].Sections[j].Variants = append(s.docs[i].Sections[j].Variants, *variant)
				return nil
			}
		}
	}
	return types.ErrDocumentNotFound
}

func (s *Store) UpdateVariantEmbedding(_ context.Context, variantUUID string, embedding []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, doc := range s.docs {
		for i := range doc.Sections {
			for j := range doc.Sections[i].Variants {
				if doc.Sections[i].Variants[j].UUID == variantUUID {
					doc.Sections[i].Variants[j].Embedding = embedding
					return nil
				}
			}
		}
	}
	return types.ErrDocumentNotFound
}

func (s *Store) GetVariant(_ context.Context, variantUUID string) (*types.ContentVariant, *types.Provenance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, doc := range s.docs {
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if v.UUID == variantUUID {
					prov := &types.Provenance{
						DocumentUUID:   doc.UUID,
						DocumentTitle:  doc.Title,
						SourceURI:      doc.SourceURI,
						SectionUUID:    sec.UUID,
						SectionHeading: sec.Heading,
						SectionIndex:   sec.Index,
					}
					return &v, prov, nil
				}
			}
		}
	}
	return nil, nil, types.ErrVariantNotFound
}

// GetVariantRecord returns the variant with its provenance and the owning
// document's metadata and timestamp, implementing types.VariantRecordGetter.
func (s *Store) GetVariantRecord(_ context.Context, variantUUID string) (*types.VariantRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, doc := range s.docs {
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if v.UUID != variantUUID {
					continue
				}
				return &types.VariantRecord{
					Variant: v,
					Provenance: types.Provenance{
						DocumentUUID:   doc.UUID,
						DocumentTitle:  doc.Title,
						SourceURI:      doc.SourceURI,
						SectionUUID:    sec.UUID,
						SectionHeading: sec.Heading,
						SectionIndex:   sec.Index,
					},
					DocumentMetadata: maps.Clone(doc.Metadata),
					Timestamp:        doc.EffectiveTime(),
					Scope:            doc.Scope,
				}, nil
			}
		}
	}
	return nil, types.ErrVariantNotFound
}

// GetVariantRecords returns the records for many variants in one pass over
// the store, implementing types.VariantRecordsGetter. UUIDs with no stored
// variant are absent from the result.
func (s *Store) GetVariantRecords(_ context.Context, variantUUIDs []string) (map[string]*types.VariantRecord, error) {
	want := make(map[string]bool, len(variantUUIDs))
	for _, u := range variantUUIDs {
		want[u] = true
	}
	out := make(map[string]*types.VariantRecord, len(variantUUIDs))
	if len(want) == 0 {
		return out, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, doc := range s.docs {
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if !want[v.UUID] {
					continue
				}
				out[v.UUID] = &types.VariantRecord{
					Variant: v,
					Provenance: types.Provenance{
						DocumentUUID:   doc.UUID,
						DocumentTitle:  doc.Title,
						SourceURI:      doc.SourceURI,
						SectionUUID:    sec.UUID,
						SectionHeading: sec.Heading,
						SectionIndex:   sec.Index,
					},
					DocumentMetadata: maps.Clone(doc.Metadata),
					Timestamp:        doc.EffectiveTime(),
					Scope:            doc.Scope,
				}
				if len(out) == len(want) {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// ListDocumentUUIDs returns the UUID of every stored document in a stable
// order, implementing types.DocumentLister.
func (s *Store) ListDocumentUUIDs(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uuids := make([]string, 0, len(s.docs))
	for uuid := range s.docs {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	return uuids, nil
}

// FindBySourceURI returns the documents of scope with the given source URI,
// most recently updated first, implementing types.SourceFinder.
func (s *Store) FindBySourceURI(_ context.Context, scope, sourceURI string) ([]types.SourceDocument, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []types.SourceDocument{}
	for _, doc := range s.docs {
		if doc.Scope == scope && doc.SourceURI == sourceURI {
			out = append(out, sourceDocument(doc))
		}
	}
	sortSourceDocuments(out)
	return out, nil
}

// ListSourceDocuments returns the documents of scope whose source URI starts
// with uriPrefix, implementing types.SourceLister.
func (s *Store) ListSourceDocuments(_ context.Context, scope, uriPrefix string) ([]types.SourceDocument, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []types.SourceDocument{}
	for _, doc := range s.docs {
		if doc.Scope == scope && strings.HasPrefix(doc.SourceURI, uriPrefix) {
			out = append(out, sourceDocument(doc))
		}
	}
	sortSourceDocuments(out)
	return out, nil
}

func sourceDocument(doc *types.Document) types.SourceDocument {
	return types.SourceDocument{
		UUID:             doc.UUID,
		Scope:            doc.Scope,
		SourceURI:        doc.SourceURI,
		Fingerprint:      doc.Fingerprint,
		SourceModifiedAt: doc.SourceModifiedAt,
		UpdatedAt:        docTimestamp(doc),
	}
}

// sortSourceDocuments orders by source URI, then most recently updated
// first, then UUID for a stable order.
func sortSourceDocuments(docs []types.SourceDocument) {
	sort.Slice(docs, func(i, j int) bool {
		a, b := docs[i], docs[j]
		if a.SourceURI != b.SourceURI {
			return a.SourceURI < b.SourceURI
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.UUID < b.UUID
	})
}

// SearchByEmbedding scores every embedded variant by cosine similarity.
// Scope, time range, content-type, metadata, and min-score conditions are
// applied before the result is cut to the limit.
func (s *Store) SearchByEmbedding(_ context.Context, embedding []float32, opts *types.SearchOptions) ([]types.SearchHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}
	if opts == nil {
		opts = &types.SearchOptions{}
	}

	typeFilter := make(map[types.ContentType]bool)
	if opts != nil {
		for _, ct := range opts.ContentTypes {
			typeFilter[ct] = true
		}
	}

	var results []types.SearchHit
	for _, doc := range s.docs {
		if doc.Scope != opts.Scope || !opts.InTimeRange(doc.EffectiveTime()) {
			continue
		}
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if len(typeFilter) > 0 && !typeFilter[v.ContentType] {
					continue
				}
				if len(v.Embedding) == 0 {
					continue
				}

				// Apply metadata filters (merged doc + variant metadata).
				if opts != nil && len(opts.MetadataFilters) > 0 {
					merged := mergeMetadata(doc.Metadata, v.Metadata)
					if !matchFilters(merged, opts.MetadataFilters) {
						continue
					}
				}

				score := cosineSimilarity(embedding, v.Embedding)

				// Apply min score filter.
				if opts != nil && opts.MinScore > 0 && score < opts.MinScore {
					continue
				}

				results = append(results, types.SearchHit{
					Variant:   v,
					Score:     score,
					Timestamp: doc.EffectiveTime(),
					Provenance: types.Provenance{
						DocumentUUID:   doc.UUID,
						DocumentTitle:  doc.Title,
						SourceURI:      doc.SourceURI,
						SectionUUID:    sec.UUID,
						SectionHeading: sec.Heading,
						SectionIndex:   sec.Index,
					},
				})
			}
		}
	}

	// Sort by score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (s *Store) Close(_ context.Context) error {
	return nil
}

// docTimestamp returns the document's recency timestamp, preferring UpdatedAt
// and falling back to CreatedAt. The zero value means "unknown".
func docTimestamp(doc *types.Document) time.Time {
	if !doc.UpdatedAt.IsZero() {
		return doc.UpdatedAt
	}
	return doc.CreatedAt
}

func mergeMetadata(docMeta, variantMeta map[string]string) map[string]string {
	merged := make(map[string]string)
	for k, v := range docMeta {
		merged[k] = v
	}
	for k, v := range variantMeta {
		merged[k] = v
	}
	return merged
}

func matchFilters(meta map[string]string, filters []types.MetadataFilter) bool {
	for _, f := range filters {
		val, ok := meta[f.Key]
		switch f.Op {
		case types.FilterEq:
			if !ok || val != f.Value {
				return false
			}
		case types.FilterNeq:
			if ok && val == f.Value {
				return false
			}
		case types.FilterContains:
			if !ok || !strings.Contains(val, f.Value) {
				return false
			}
		}
	}
	return true
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
