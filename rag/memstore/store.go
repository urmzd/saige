// Package memstore provides an in-memory implementation of ragtypes.Store.
package memstore

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

// Store is a thread-safe in-memory document store with brute-force cosine similarity search.
type Store struct {
	mu           sync.RWMutex
	docs         map[string]*ragtypes.Document
	originals    map[string][]byte
	fingerprints map[string]string // fingerprint -> doc UUID
}

// New creates a new in-memory store.
func New() *Store {
	return &Store{
		docs:         make(map[string]*ragtypes.Document),
		originals:    make(map[string][]byte),
		fingerprints: make(map[string]string),
	}
}

func (s *Store) CreateDocument(_ context.Context, doc *ragtypes.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[doc.UUID] = doc
	if doc.Fingerprint != "" {
		s.fingerprints[doc.Fingerprint] = doc.UUID
	}
	return nil
}

func (s *Store) GetDocument(_ context.Context, uuid string) (*ragtypes.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[uuid]
	if !ok {
		return nil, ragtypes.ErrDocumentNotFound
	}
	return doc, nil
}

func (s *Store) FindByFingerprint(_ context.Context, fingerprint string) (*ragtypes.Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uuid, ok := s.fingerprints[fingerprint]
	if !ok {
		return nil, ragtypes.ErrDocumentNotFound
	}
	return s.docs[uuid], nil
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
		return nil, ragtypes.ErrDocumentNotFound
	}
	return data, nil
}

func (s *Store) CreateSection(_ context.Context, section *ragtypes.Section) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[section.DocumentUUID]
	if !ok {
		return ragtypes.ErrDocumentNotFound
	}
	doc.Sections = append(doc.Sections, *section)
	return nil
}

func (s *Store) GetSections(_ context.Context, documentUUID string) ([]ragtypes.Section, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[documentUUID]
	if !ok {
		return nil, ragtypes.ErrDocumentNotFound
	}
	return doc.Sections, nil
}

func (s *Store) CreateVariant(_ context.Context, variant *ragtypes.ContentVariant) error {
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
	return ragtypes.ErrDocumentNotFound
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
	return ragtypes.ErrDocumentNotFound
}

func (s *Store) GetVariant(_ context.Context, variantUUID string) (*ragtypes.ContentVariant, *ragtypes.Provenance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, doc := range s.docs {
		for _, sec := range doc.Sections {
			for _, v := range sec.Variants {
				if v.UUID == variantUUID {
					prov := &ragtypes.Provenance{
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
	return nil, nil, ragtypes.ErrVariantNotFound
}

func (s *Store) SearchByEmbedding(_ context.Context, embedding []float32, opts *ragtypes.SearchOptions) ([]ragtypes.SearchHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	typeFilter := make(map[ragtypes.ContentType]bool)
	if opts != nil {
		for _, ct := range opts.ContentTypes {
			typeFilter[ct] = true
		}
	}

	var results []ragtypes.SearchHit
	for _, doc := range s.docs {
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

				results = append(results, ragtypes.SearchHit{
					Variant: v,
					Score:   score,
					Provenance: ragtypes.Provenance{
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

func matchFilters(meta map[string]string, filters []ragtypes.MetadataFilter) bool {
	for _, f := range filters {
		val, ok := meta[f.Key]
		switch f.Op {
		case ragtypes.FilterEq:
			if !ok || val != f.Value {
				return false
			}
		case ragtypes.FilterNeq:
			if ok && val == f.Value {
				return false
			}
		case ragtypes.FilterContains:
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
