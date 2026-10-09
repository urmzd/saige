// Package bm25retriever implements a BM25 lexical retriever with an in-memory inverted index.
package bm25retriever

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/urmzd/saige/rag/types"
)

// Config holds BM25 parameters.
type Config struct {
	K1 float64
	B  float64
}

// DefaultConfig returns the standard BM25 parameters.
func DefaultConfig() *Config {
	return &Config{K1: 1.2, B: 0.75}
}

type posting struct {
	variantUUID string
	termFreq    float64
}

// ErrRebuildUnsupported is returned by RebuildIndex when the store cannot
// enumerate its documents (it does not implement types.DocumentLister).
var ErrRebuildUnsupported = errors.New("bm25: store does not list documents; index cannot be rebuilt")

// Retriever implements types.Retriever, types.Indexer, and
// types.IndexRebuilder using BM25 scoring over an in-memory inverted index.
type Retriever struct {
	mu       sync.RWMutex
	store    types.Store
	cfg      Config
	index    map[string][]posting // term -> postings
	docLen   map[string]float64   // variantUUID -> document length (token count)
	avgDL    float64
	docCount int
	// Track which variants belong to which document for Remove.
	docVariants map[string][]string // documentUUID -> []variantUUID

	// rebuildMu serializes RebuildIndex calls.
	rebuildMu sync.Mutex
	// pending is non-nil while RebuildIndex runs. It records the latest
	// Index (document) or Remove (nil) call per document UUID so the rebuild
	// can replay them onto the fresh index before swapping it in. Guarded by
	// mu.
	pending map[string]*types.Document
}

// New creates a BM25 retriever. If cfg is nil, defaults are used.
//
// The index is held in process memory and starts empty. Over a persistent
// store, call RebuildIndex after startup so documents ingested by an earlier
// process are searchable.
func New(store types.Store, cfg *Config) *Retriever {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Retriever{
		store:       store,
		cfg:         *cfg,
		index:       make(map[string][]posting),
		docLen:      make(map[string]float64),
		docVariants: make(map[string][]string),
	}
}

// tokenize splits text into lowercase tokens.
func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Index indexes all text variants in a document. It is idempotent: postings
// already held for doc.UUID are replaced, so re-indexing a document never
// double-counts its variants.
func (r *Retriever) Index(_ context.Context, doc *types.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(doc.UUID)
	r.indexLocked(doc)
	r.recomputeAvgDL()
	if r.pending != nil {
		r.pending[doc.UUID] = doc
	}
	return nil
}

// indexLocked adds doc's text variants to the index. The caller holds r.mu
// and recomputes avgDL afterwards.
func (r *Retriever) indexLocked(doc *types.Document) {
	for _, sec := range doc.Sections {
		for _, v := range sec.Variants {
			if v.ContentType != types.ContentText || v.Text == "" {
				continue
			}
			if _, seen := r.docLen[v.UUID]; seen {
				continue
			}
			tokens := tokenize(v.Text)
			dl := float64(len(tokens))

			// Count term frequencies.
			tf := make(map[string]float64)
			for _, t := range tokens {
				tf[t]++
			}

			for term, freq := range tf {
				r.index[term] = append(r.index[term], posting{
					variantUUID: v.UUID,
					termFreq:    freq,
				})
			}

			r.docLen[v.UUID] = dl
			r.docVariants[doc.UUID] = append(r.docVariants[doc.UUID], v.UUID)
			r.docCount++
		}
	}
}

// Remove removes all postings for variants belonging to the given document.
func (r *Retriever) Remove(_ context.Context, documentUUID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removeLocked(documentUUID) {
		r.recomputeAvgDL()
	}
	if r.pending != nil {
		r.pending[documentUUID] = nil
	}
	return nil
}

// removeLocked drops documentUUID's postings and reports whether it held
// any. The caller holds r.mu.
func (r *Retriever) removeLocked(documentUUID string) bool {
	variants, ok := r.docVariants[documentUUID]
	if !ok {
		return false
	}

	variantSet := make(map[string]bool, len(variants))
	for _, v := range variants {
		variantSet[v] = true
	}

	// Remove postings.
	for term, postings := range r.index {
		filtered := postings[:0]
		for _, p := range postings {
			if !variantSet[p.variantUUID] {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			delete(r.index, term)
		} else {
			r.index[term] = filtered
		}
	}

	// Remove doc lengths and variant tracking.
	for _, v := range variants {
		delete(r.docLen, v)
		r.docCount--
	}
	delete(r.docVariants, documentUUID)
	return true
}

// RebuildIndex replaces the in-memory index with one built from every
// document in the store. The index lives only in process memory, so a
// retriever over a persistent store starts empty and must be rebuilt after a
// restart. The store must implement types.DocumentLister; otherwise
// RebuildIndex returns ErrRebuildUnsupported and leaves the index unchanged.
// Searches keep using the previous index until the new one is complete.
// Index and Remove calls made while the rebuild runs are replayed onto the
// new index before it is swapped in, so they are not lost.
func (r *Retriever) RebuildIndex(ctx context.Context) error {
	lister, ok := r.store.(types.DocumentLister)
	if !ok {
		return ErrRebuildUnsupported
	}
	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()

	// Start recording writes before listing, so any document the listing
	// misses was written after this point and is replayed below.
	r.mu.Lock()
	r.pending = make(map[string]*types.Document)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.pending = nil
		r.mu.Unlock()
	}()

	uuids, err := lister.ListDocumentUUIDs(ctx)
	if err != nil {
		return fmt.Errorf("list documents: %w", err)
	}

	fresh := New(r.store, &r.cfg)
	for _, uuid := range uuids {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, err := r.store.GetDocument(ctx, uuid)
		if errors.Is(err, types.ErrDocumentNotFound) {
			continue // deleted since it was listed
		}
		if err != nil {
			return fmt.Errorf("load document %s: %w", uuid, err)
		}
		fresh.indexLocked(doc)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for uuid, doc := range r.pending {
		fresh.removeLocked(uuid)
		if doc != nil {
			fresh.indexLocked(doc)
		}
	}
	fresh.recomputeAvgDL()
	r.index = fresh.index
	r.docLen = fresh.docLen
	r.docVariants = fresh.docVariants
	r.docCount = fresh.docCount
	r.avgDL = fresh.avgDL
	return nil
}

func (r *Retriever) recomputeAvgDL() {
	if r.docCount == 0 {
		r.avgDL = 0
		return
	}
	total := 0.0
	for _, dl := range r.docLen {
		total += dl
	}
	r.avgDL = total / float64(r.docCount)
}

// Retrieve computes BM25 scores for each variant matching the query terms.
//
// Scope, time-range, content-type, and metadata filters run before the
// result is cut to the limit, so a selective filter still returns a full page
// when enough variants match. When the store implements types.VariantRecordsGetter or
// types.VariantRecordGetter, hits carry the document timestamp and metadata
// filters see the document's metadata merged with the variant's, as in a
// store vector search. Other stores get the same record from GetVariant and
// GetDocument; a variant whose document is missing is skipped. Candidates are
// resolved in batches, in one lookup per batch when the store implements
// types.VariantRecordsGetter. A candidate
// missing from the store is skipped; any other store error is returned.
func (r *Retriever) Retrieve(ctx context.Context, query string, opts *types.SearchOptions) ([]types.SearchHit, error) {
	r.mu.RLock()
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 || r.docCount == 0 {
		r.mu.RUnlock()
		return nil, nil
	}

	scores := make(map[string]float64)
	N := float64(r.docCount)
	k1 := r.cfg.K1
	b := r.cfg.B

	for _, term := range queryTokens {
		postings, ok := r.index[term]
		if !ok {
			continue
		}
		df := float64(len(postings))
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))

		for _, p := range postings {
			dl := r.docLen[p.variantUUID]
			tf := p.termFreq
			score := idf * (tf * (k1 + 1)) / (tf + k1*(1-b+b*dl/r.avgDL))
			scores[p.variantUUID] += score
		}
	}
	r.mu.RUnlock()

	limit := 10
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}

	type scoredVariant struct {
		uuid  string
		score float64
	}
	ranked := make([]scoredVariant, 0, len(scores))
	for uuid, score := range scores {
		if opts != nil && opts.MinScore > 0 && score < opts.MinScore {
			continue
		}
		ranked = append(ranked, scoredVariant{uuid, score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].uuid < ranked[j].uuid
	})

	hits := make([]types.SearchHit, 0, min(limit, len(ranked)))
	// Resolve candidates in batches until the page is full. Batches grow
	// with the limit so a selective filter costs a few lookups, not one per
	// candidate.
	batch := max(limit*2, minLookupBatch)
	for start := 0; start < len(ranked) && len(hits) < limit; start += batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ranked[start:min(start+batch, len(ranked))]
		uuids := make([]string, len(chunk))
		for i, sv := range chunk {
			uuids[i] = sv.uuid
		}
		records, err := r.lookupRecords(ctx, uuids)
		if err != nil {
			return nil, err
		}
		for _, sv := range chunk {
			if len(hits) >= limit {
				break
			}
			rec, ok := records[sv.uuid]
			if !ok {
				continue // removed from the store since it was indexed
			}
			if !opts.Admits(rec) {
				continue
			}
			hits = append(hits, types.SearchHit{
				Variant:    rec.Variant,
				Score:      sv.score,
				Provenance: rec.Provenance,
				Timestamp:  rec.Timestamp,
			})
		}
	}

	return hits, nil
}

// minLookupBatch is the smallest number of candidates Retrieve resolves
// against the store at a time.
const minLookupBatch = 32

// lookupRecords resolves variant UUIDs to records using the richest lookup
// the store offers. Variants missing from the store are absent from the
// result; any other store error is returned.
func (r *Retriever) lookupRecords(ctx context.Context, uuids []string) (map[string]*types.VariantRecord, error) {
	if batch, ok := r.store.(types.VariantRecordsGetter); ok {
		recs, err := batch.GetVariantRecords(ctx, uuids)
		if err != nil {
			return nil, fmt.Errorf("bm25: load variants: %w", err)
		}
		return recs, nil
	}
	recordGetter, hasRecords := r.store.(types.VariantRecordGetter)
	out := make(map[string]*types.VariantRecord, len(uuids))
	// docs caches owning documents for stores without record lookups, so a
	// batch of variants from one document loads it once.
	docs := make(map[string]*types.Document)
	for _, uuid := range uuids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hasRecords {
			rec, err := recordGetter.GetVariantRecord(ctx, uuid)
			if errors.Is(err, types.ErrVariantNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("bm25: load variant %s: %w", uuid, err)
			}
			out[uuid] = rec
			continue
		}
		variant, prov, err := r.store.GetVariant(ctx, uuid)
		if errors.Is(err, types.ErrVariantNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bm25: load variant %s: %w", uuid, err)
		}
		// The owning document supplies the scope, time, and metadata the
		// filters need; without it the variant cannot be checked, so it is
		// skipped like a missing one.
		doc, cached := docs[prov.DocumentUUID]
		if !cached {
			doc, err = r.store.GetDocument(ctx, prov.DocumentUUID)
			if err != nil && !errors.Is(err, types.ErrDocumentNotFound) {
				return nil, fmt.Errorf("bm25: load document %s: %w", prov.DocumentUUID, err)
			}
			docs[prov.DocumentUUID] = doc
		}
		if doc == nil {
			continue
		}
		out[uuid] = types.NewVariantRecord(variant, prov, doc)
	}
	return out, nil
}

// Name reports "bm25", implementing types.Named.
func (r *Retriever) Name() string { return "bm25" }
