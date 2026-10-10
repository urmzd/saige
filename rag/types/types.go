// Package types defines the core types and interfaces for rag.
package types

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	agenttypes "github.com/urmzd/saige/agent/types"
)

// --- Errors ---

// RAG errors.
var (
	ErrDocumentNotFound = errors.New("document not found")
	ErrVariantNotFound  = errors.New("variant not found")

	// ErrDuplicateDocument is returned by Store.CreateDocument and
	// DocumentReplacer.ReplaceDocument when another document already holds the
	// same non-empty fingerprint. Concurrent ingests of identical content race
	// on this check; the pipeline resolves the loser to the winning document.
	ErrDuplicateDocument = errors.New("duplicate document")

	// ErrEmbeddingShape is returned when an embedder's output does not match
	// its input: a different vector count, an empty vector, or vectors of
	// different dimensions from one embedder. Such output would otherwise
	// produce variants that vector search can never find.
	ErrEmbeddingShape = errors.New("embedding output shape mismatch")

	ErrNoExtractor         = errors.New("content extractor not configured")
	ErrNoStore             = errors.New("store not configured")
	ErrNoRetriever         = errors.New("no retriever configured")
	ErrUnsupportedMIMEType = errors.New("unsupported MIME type")

	// ErrPartialSearch is returned (wrapped) by Pipeline.Search alongside a
	// non-nil result when some, but not all, retrievers failed. Callers that
	// tolerate partial results should errors.Is-check for it and keep the hits;
	// callers that treat any error as fatal retain today's fail-fast behavior.
	ErrPartialSearch = errors.New("partial search failure")

	// ErrPartialIngest is returned (wrapped) by Pipeline.Ingest and
	// Pipeline.Update alongside a non-nil result when the document was durably
	// written to the RAG store but a later stage failed: storing the original
	// bytes, indexing into a retriever that implements Indexer, or knowledge
	// graph ingestion. The document is committed and retrievable by vector
	// search; the original bytes, lexical index entries, or derived graph
	// facts may be missing. Callers can errors.Is-check for it to distinguish
	// this from a failed write.
	//
	// Re-ingesting identical content under DedupSkip returns the committed
	// document without repeating these stages. To repair a partial ingest,
	// Delete the document and ingest it again, or ingest with DedupReplace.
	ErrPartialIngest = errors.New("partial ingest failure")

	// ErrScopeMismatch is returned when a request names a scope that differs
	// from the fixed scope of the pipeline that receives it.
	ErrScopeMismatch = errors.New("scope mismatch")

	// ErrSyncUnsupported is returned by SyncSource when the store cannot
	// enumerate documents by source URI (see SourceLister).
	ErrSyncUnsupported = errors.New("store does not support source sync")
)

// --- Content types ---

// ContentType represents the modality of a content variant.
type ContentType string

// Content types.
const (
	ContentText     ContentType = "text"
	ContentImage    ContentType = "image"
	ContentTable    ContentType = "table"
	ContentAudio    ContentType = "audio"
	ContentVideo    ContentType = "video"
	ContentDocument ContentType = "document"
)

// --- Core data model ---

// ContentVariant is a specific modality representation of a section.
type ContentVariant struct {
	UUID        string            `json:"uuid"`
	SectionUUID string            `json:"section_uuid"`
	ContentType ContentType       `json:"content_type"`
	MIMEType    string            `json:"mime_type"`
	Data        []byte            `json:"data"`
	Text        string            `json:"text"`
	Embedding   []float32         `json:"embedding,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Section is an ordered slice of a document containing content variants.
type Section struct {
	UUID         string           `json:"uuid"`
	DocumentUUID string           `json:"document_uuid"`
	Index        int              `json:"index"`
	Heading      string           `json:"heading,omitempty"`
	Variants     []ContentVariant `json:"variants"`
}

// Document is the top-level unit in the rag data model.
type Document struct {
	UUID string `json:"uuid"`
	// Scope is the isolation boundary (for example a tenant) the document
	// belongs to. The empty string is the default scope. Stores keep it, and
	// searches only see documents of the scope they name. See RawDocument.Scope.
	Scope       string            `json:"scope,omitempty"`
	SourceURI   string            `json:"source_uri"`
	Fingerprint string            `json:"fingerprint"`
	Title       string            `json:"title,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Sections    []Section         `json:"sections"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	// SourceModifiedAt is when the source last changed the content, as
	// reported by the source (file mtime, Last-Modified). The zero value means
	// unknown. It takes precedence over UpdatedAt for recency and time-range
	// filters, because re-ingesting old content does not make it new.
	SourceModifiedAt time.Time `json:"source_modified_at,omitzero"`
}

// EffectiveTime returns the time used for recency scoring and time-range
// filters: SourceModifiedAt when known, else UpdatedAt, else CreatedAt. The
// zero value means unknown.
func (d *Document) EffectiveTime() time.Time {
	switch {
	case !d.SourceModifiedAt.IsZero():
		return d.SourceModifiedAt
	case !d.UpdatedAt.IsZero():
		return d.UpdatedAt
	default:
		return d.CreatedAt
	}
}

// RawDocument represents unprocessed input from a source.
type RawDocument struct {
	SourceURI string            `json:"source_uri"`
	MIMEType  string            `json:"mime_type"`
	Data      []byte            `json:"data"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	// Scope places the document in an isolation boundary such as a tenant.
	// Deduplication only matches documents of the same scope, so identical
	// bytes ingested under two scopes become two documents. Empty means the
	// pipeline's scope (see rag.WithScope), or the default scope.
	Scope string `json:"scope,omitempty"`
	// SourceModifiedAt is the source's last-modified time for the content.
	// Zero means unknown. It is copied to Document.SourceModifiedAt.
	SourceModifiedAt time.Time `json:"source_modified_at,omitzero"`
}

// --- Provenance and citation types ---

// Provenance tracks the origin of a search hit for citation purposes.
type Provenance struct {
	DocumentUUID   string `json:"document_uuid"`
	DocumentTitle  string `json:"document_title,omitempty"`
	SourceURI      string `json:"source_uri,omitempty"`
	SectionUUID    string `json:"section_uuid"`
	SectionHeading string `json:"section_heading,omitempty"`
	SectionIndex   int    `json:"section_index"`
	// ExpandedFromVariantUUID is set when the hit's Variant.Text was replaced
	// by the full text of its parent section. It names the variant whose match
	// caused the expansion, so a caller can tell that looking up that variant
	// alone returns only the shorter child text.
	ExpandedFromVariantUUID string `json:"expanded_from_variant_uuid,omitempty"`
	// Window is set when the hit's text was widened with neighboring
	// sections of the same document (see WithNeighborWindow). It names the
	// inclusive range of section indexes the text now covers.
	Window *SectionWindow `json:"window,omitempty"`
}

// SectionWindow is an inclusive range of section indexes within one document.
type SectionWindow struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

// SearchHit is a scored content variant with full provenance for citation tracking.
type SearchHit struct {
	Variant    ContentVariant `json:"variant"`
	Score      float64        `json:"score"`
	Provenance Provenance     `json:"provenance"`
	// Timestamp is the source document's effective time (see
	// Document.EffectiveTime): the source's last-modified time when known,
	// else its last-updated or created time. It is populated by stores during
	// search and used by WithRecency and WithTimeRange. The zero value means
	// "unknown".
	Timestamp time.Time `json:"timestamp,omitempty"`
	// Highlight is the matched part of the variant text, set by keyword
	// searches that ask for it (see KeywordQuery.Highlight).
	Highlight *Highlight `json:"highlight,omitempty"`
}

// FilterOp defines metadata filter comparison operations.
type FilterOp string

// Filter operators.
const (
	FilterEq       FilterOp = "eq"
	FilterNeq      FilterOp = "neq"
	FilterContains FilterOp = "contains"
)

// MetadataFilter filters search results by metadata key-value conditions.
type MetadataFilter struct {
	Key   string   `json:"key"`
	Op    FilterOp `json:"op"`
	Value string   `json:"value"`
}

// ContextBlock is an individual citation block with exact source text.
type ContextBlock struct {
	Text       string     `json:"text"`
	Citation   string     `json:"citation"`
	Provenance Provenance `json:"provenance"`
}

// AssembledContext is the full prompt with inline citations and source blocks.
type AssembledContext struct {
	Prompt     string         `json:"prompt"`
	Blocks     []ContextBlock `json:"blocks"`
	TokenCount int            `json:"token_count"`
}

// SearchPipelineResult holds the full result of a pipeline search.
type SearchPipelineResult struct {
	Query              string            `json:"query"`
	TransformedQueries []string          `json:"transformed_queries,omitempty"`
	Hits               []SearchHit       `json:"hits"`
	Context            *AssembledContext `json:"context,omitempty"`
	// Retrievals reports one entry per retriever and query pair, in
	// retriever order, so a caller can see which arm was slow, empty, or
	// failed without tracing.
	Retrievals []RetrievalStat `json:"retrievals,omitempty"`
}

// RetrievalStat describes one retriever call made by a search.
type RetrievalStat struct {
	// Retriever is the retriever's name (see Named), or "retriever-N" for a
	// retriever without one, N being its position in the pipeline.
	Retriever string `json:"retriever"`
	// QueryIndex is the position of the query in the transformed query list;
	// 0 is the original query when no transformer is configured.
	QueryIndex int `json:"query_index"`
	// Hits is the number of hits the call returned.
	Hits     int           `json:"hits"`
	Duration time.Duration `json:"duration_ns"`
	// Error is the call's error text, empty on success. A call can return
	// hits and an error when it failed only in part.
	Error string `json:"error,omitempty"`
}

// --- Search options ---

// SearchOption configures a search query.
type SearchOption func(*SearchConfig)

// SearchConfig holds parsed search options for pipeline search.
type SearchConfig struct {
	// Scope limits the search to documents of one scope. See WithScope.
	Scope string
	// Since and Until bound the hits' effective document time. See
	// WithTimeRange.
	Since time.Time
	Until time.Time
	// DedupContent collapses hits with identical content. See
	// WithContentDedup.
	DedupContent bool
	// NeighborWindow widens each hit's text with this many sections on each
	// side. See WithNeighborWindow.
	NeighborWindow int

	ContentTypes    []ContentType
	Limit           int
	MetadataFilters []MetadataFilter
	MinScore        float64
	AssembleContext bool
	MaxTokens       int

	// CandidateK is how many fused candidates the pipeline keeps for the
	// reranker before it cuts the result to Limit. Zero means the default:
	// max(4*Limit, 20) when a reranker is configured, otherwise Limit.
	CandidateK int

	// FusionK is the k constant for Reciprocal Rank Fusion. Zero means the
	// pipeline default (60).
	FusionK int
	// FusionWeights overrides the fuser's per-retriever weights for this
	// search. See WithFusionWeights.
	FusionWeights map[string]float64

	// Keyword shapes the keyword retrievers' query. See WithKeywordQuery.
	Keyword *KeywordQuery

	// Recency time-decay scoring. Enabled only when RecencyHalfLife > 0.
	RecencyHalfLife time.Duration
	RecencyWeight   float64
	// RecencyNow overrides the reference time for age computation (test seam).
	// Zero value means time.Now() is used.
	RecencyNow time.Time
}

// WithContentTypes filters search results to specific content types.
func WithContentTypes(types ...ContentType) SearchOption {
	return func(c *SearchConfig) { c.ContentTypes = types }
}

// WithLimit sets the maximum number of results.
func WithLimit(n int) SearchOption {
	return func(c *SearchConfig) { c.Limit = n }
}

// WithMetadataFilter adds a metadata filter to the search.
func WithMetadataFilter(key string, op FilterOp, value string) SearchOption {
	return func(c *SearchConfig) {
		c.MetadataFilters = append(c.MetadataFilters, MetadataFilter{Key: key, Op: op, Value: value})
	}
}

// WithFusionK sets the k constant used by Reciprocal Rank Fusion when merging
// results from multiple retrievers (default 60). Smaller values weight
// top-ranked hits from individual retrievers more heavily; larger values
// favor hits that appear across many retrievers. Values <= 0 are ignored,
// keeping the default.
func WithFusionK(k int) SearchOption {
	return func(c *SearchConfig) {
		if k > 0 {
			c.FusionK = k
		}
	}
}

// WithFusionWeights weights each retriever's lists in fusion for this
// search, keyed by retriever name (see Named), such as "bm25" and "vector".
// A weight overrides the one the pipeline or fuser sets for that retriever;
// retrievers not named keep theirs. A weight <= 0 leaves the retriever's
// lists out of fusion.
func WithFusionWeights(weights map[string]float64) SearchOption {
	return func(c *SearchConfig) { c.FusionWeights = weights }
}

// WithKeywordQuery runs the keyword retrievers with q instead of a plain
// text match: phrase, prefix, or fuzzy matching, boolean clauses, field
// boosts, and highlighting. An empty q.Text means the search query. Other
// retrievers ignore it. An invalid q fails the keyword retrievers with
// ErrInvalidKeywordQuery.
func WithKeywordQuery(q KeywordQuery) SearchOption {
	return func(c *SearchConfig) { c.Keyword = &q }
}

// WithMinScore sets a minimum relevance in [0,1] for fused results. The
// pipeline compares it with each hit's fused score divided by the highest
// fused score a hit could reach (rank one in every retrieved list), so 1.0
// keeps only hits ranked first everywhere and 0.5 keeps hits that earn at
// least half of that. The filter runs before reranking. Hit.Score keeps its
// fused (or reranked) value; the normalized value is used only for this
// comparison. Thresholds on raw retriever scores, such as cosine similarity
// or BM25, belong in the retriever that produces them.
func WithMinScore(score float64) SearchOption {
	return func(c *SearchConfig) { c.MinScore = score }
}

// WithCandidatePool sets how many fused candidates reach the reranker. Every
// retriever is asked for at least k hits, fusion keeps the top k, and the
// pipeline cuts the reranked list to the search limit. A value below the
// limit is raised to the limit. Without a reranker, a pool larger than the
// limit still deepens each retriever's list before fusion. Values <= 0 keep
// the default of max(4*limit, 20) with a reranker and the limit without one.
func WithCandidatePool(k int) SearchOption {
	return func(c *SearchConfig) {
		if k > 0 {
			c.CandidateK = k
		}
	}
}

// WithScope limits the search to documents ingested under scope. The empty
// string is the default scope. A pipeline built with rag.WithScope rejects a
// different scope with ErrScopeMismatch.
func WithScope(scope string) SearchOption {
	return func(c *SearchConfig) { c.Scope = scope }
}

// WithTimeRange keeps hits whose effective document time (see
// Document.EffectiveTime) is at or after since and before until. A zero bound
// is open. When either bound is set, hits with an unknown time are dropped.
func WithTimeRange(since, until time.Time) SearchOption {
	return func(c *SearchConfig) {
		c.Since = since
		c.Until = until
	}
}

// WithContentDedup collapses fused hits whose content is identical (same
// content type and the same trimmed text, or the same bytes for non-text
// variants), keeping the highest-ranked one. Fusion counts duplicate copies
// as independent votes, which is right for ranking but repeats the same
// passage in the result.
func WithContentDedup() SearchOption {
	return func(c *SearchConfig) { c.DedupContent = true }
}

// WithNeighborWindow widens each final text hit with up to n neighboring
// sections on each side from the same document, in section order, and
// records the covered range in Provenance.Window. A hit whose section is
// already covered by the window of a higher-ranked hit is dropped, so the
// result never repeats a section. Values <= 0 disable expansion.
func WithNeighborWindow(n int) SearchOption {
	return func(c *SearchConfig) {
		if n > 0 {
			c.NeighborWindow = n
		}
	}
}

// WithContextAssembly enables context assembly with inline citations.
func WithContextAssembly(maxTokens int) SearchOption {
	return func(c *SearchConfig) {
		c.AssembleContext = true
		c.MaxTokens = maxTokens
	}
}

// WithRecency enables time-decay recency scoring after RRF fusion. Each hit's
// fused score is blended with an exponential recency factor
// exp(-ln2 * age / halfLife) derived from the hit's source-document timestamp.
//
// halfLife is the age at which the recency factor reaches 0.5; weight in [0,1]
// controls how much recency influences the final score:
//
//	score = (1-weight)*score + weight*score*recencyFactor
//
// A non-positive halfLife is a no-op, preserving today's behavior. Hits with a
// zero timestamp are treated as having no decay (recency factor 1.0).
func WithRecency(halfLife time.Duration, weight float64) SearchOption {
	return func(c *SearchConfig) {
		c.RecencyHalfLife = halfLife
		c.RecencyWeight = weight
	}
}

// --- Search options for Store ---

// SearchOptions configures a vector search query at the store level.
type SearchOptions struct {
	ContentTypes    []ContentType
	Limit           int
	MetadataFilters []MetadataFilter
	MinScore        float64
	// Scope limits results to documents of this scope. Stores and retrievers
	// must apply it as an exact match before the limit, and the empty string
	// matches only documents of the default scope.
	Scope string
	// Since and Until bound the document's effective time: at or after Since
	// and before Until. A zero bound is open. When either is set, documents
	// with an unknown time are excluded.
	Since time.Time
	Until time.Time
	// Keyword, when set, is the structured query a keyword search runs. Its
	// empty Text means the query text passed with the options. Vector
	// search ignores it.
	Keyword *KeywordQuery
}

// KeywordQueryFor returns the keyword query a search for text runs: opts'
// Keyword with an empty Text filled in from text, or a plain query for text
// when opts sets none.
func KeywordQueryFor(text string, opts *SearchOptions) KeywordQuery {
	if opts == nil || opts.Keyword == nil {
		return PlainKeywordQuery(text)
	}
	q := *opts.Keyword
	if q.Text == "" {
		q.Text = text
	}
	return q
}

// --- Store interface ---

// Store is the storage interface for rag's document hierarchy and vector search.
type Store interface {
	// Document CRUD
	CreateDocument(ctx context.Context, doc *Document) error
	GetDocument(ctx context.Context, uuid string) (*Document, error)
	FindByFingerprint(ctx context.Context, fingerprint string) (*Document, error)
	DeleteDocument(ctx context.Context, uuid string) error

	// Original byte storage
	StoreOriginal(ctx context.Context, documentUUID string, data []byte) error
	GetOriginal(ctx context.Context, documentUUID string) ([]byte, error)

	// Incremental operations
	CreateSection(ctx context.Context, section *Section) error
	GetSections(ctx context.Context, documentUUID string) ([]Section, error)
	CreateVariant(ctx context.Context, variant *ContentVariant) error
	UpdateVariantEmbedding(ctx context.Context, variantUUID string, embedding []float32) error

	// Variant lookup
	GetVariant(ctx context.Context, variantUUID string) (*ContentVariant, *Provenance, error)

	// Multi-modal vector search
	SearchByEmbedding(ctx context.Context, embedding []float32, opts *SearchOptions) ([]SearchHit, error)

	Close(ctx context.Context) error
}

// DocumentReplacer is an optional Store interface for atomically replacing a
// document. Implementations must remove the document identified by oldUUID and
// persist doc as a single all-or-nothing operation: if the write fails, the
// old document must survive untouched. doc.UUID may equal oldUUID. The
// pipeline uses it for DedupReplace and Update so a mid-write failure never
// destroys the prior document. Stores that do not implement it fall back to a
// non-atomic delete-then-create, which narrows the unprotected window to the
// store write itself.
type DocumentReplacer interface {
	ReplaceDocument(ctx context.Context, oldUUID string, doc *Document) error
}

// KeywordSearcher is an optional Store interface for stores that run lexical
// (BM25) search themselves, over an index kept with the data. It applies the
// same scope, time-range, content-type, and metadata filters as
// SearchByEmbedding, before the limit. A pipeline built with WithBM25 over a
// store that implements it searches through the store instead of an
// in-memory index, so the index survives restarts and is shared across
// processes.
//
// When opts.Keyword is set, the search runs that structured query (see
// KeywordQueryFor) and returns ErrInvalidKeywordQuery when it is invalid.
type KeywordSearcher interface {
	SearchByKeyword(ctx context.Context, query string, opts *SearchOptions) ([]SearchHit, error)
}

// StoreUnwrapper is implemented by Store decorators, such as tracing or
// caching wrappers, to expose the store they wrap. The pipeline follows it
// to find optional capabilities the wrapper does not implement itself, such
// as KeywordSearcher. A wrapper that must see those calls implements the
// capability itself instead.
type StoreUnwrapper interface {
	Unwrap() Store
}

// AsStore finds the first store of type T in the chain that starts at s and
// follows StoreUnwrapper. T is usually an optional interface such as
// KeywordSearcher. It reports false when no store in the chain has type T.
func AsStore[T any](s Store) (T, bool) {
	for s != nil {
		if v, ok := s.(T); ok {
			return v, true
		}
		u, ok := s.(StoreUnwrapper)
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	var zero T
	return zero, false
}

// GraphEpisodeDeleter is an optional interface for knowledge graphs that
// support deleting all episodes (and their derived facts) belonging to a
// group. A graph that cannot delete by document (see GraphDocumentDeleter)
// gets each document ingested under its own group, keyed by document UUID,
// and the pipeline calls DeleteEpisodes with that UUID on document
// delete/replace so graph facts do not outlive their source document. The
// pipeline also calls it to clean up documents ingested that way by older
// versions. Graphs that implement neither interface leave derived facts
// behind.
type GraphEpisodeDeleter interface {
	DeleteEpisodes(ctx context.Context, groupID string) error
}

// GraphDocumentDeleter is an optional interface for knowledge graphs that
// can delete one document's episodes inside a group, keeping entities and
// relations that other documents still support. When the graph implements
// it, the pipeline ingests every document of a pipeline into one group (the
// graph namespace), so an entity named in many documents is one node, and
// deletes a document with DeleteDocumentEpisodes.
type GraphDocumentDeleter interface {
	DeleteDocumentEpisodes(ctx context.Context, groupID, documentID string) error
}

// GraphDocumentDeletionReporter is an optional interface for graphs that
// implement GraphDocumentDeleter but can only delete by document when their
// backing store supports it. When SupportsDocumentDeletion returns false, the
// pipeline treats the graph as if it did not implement GraphDocumentDeleter:
// each document gets its own group and is removed with DeleteEpisodes.
type GraphDocumentDeletionReporter interface {
	SupportsDocumentDeletion() bool
}

// VariantRecord is a variant with its provenance and the owning document's
// metadata and timestamp. Retrievers that score variants outside the store,
// such as BM25, use it to filter on merged document and variant metadata and
// to report the same Timestamp a store search would.
type VariantRecord struct {
	Variant    ContentVariant
	Provenance Provenance
	// DocumentMetadata is the owning document's metadata. Filters evaluate it
	// merged with Variant.Metadata, with variant keys taking precedence.
	DocumentMetadata map[string]string
	// Timestamp is the document's effective time (see
	// Document.EffectiveTime).
	Timestamp time.Time
	// Scope is the owning document's scope.
	Scope string
}

// VariantRecordGetter is an optional Store interface that returns a variant
// together with its document's metadata and timestamp in one lookup.
type VariantRecordGetter interface {
	GetVariantRecord(ctx context.Context, variantUUID string) (*VariantRecord, error)
}

// LoadVariantRecord returns the record for variantUUID using the store's
// VariantRecordGetter when it has one. Otherwise it loads the variant with
// GetVariant and its owning document with GetDocument, so the record always
// carries the document's scope, timestamp, and metadata. A record built from
// the variant alone would report the default scope and no time, and would
// leak other scopes' variants into default-scope searches. When the owning
// document is missing the result is ErrVariantNotFound.
func LoadVariantRecord(ctx context.Context, store Store, variantUUID string) (*VariantRecord, error) {
	if rg, ok := store.(VariantRecordGetter); ok {
		return rg.GetVariantRecord(ctx, variantUUID)
	}
	v, prov, err := store.GetVariant(ctx, variantUUID)
	if err != nil {
		return nil, err
	}
	doc, err := store.GetDocument(ctx, prov.DocumentUUID)
	if errors.Is(err, ErrDocumentNotFound) {
		return nil, fmt.Errorf("%w: document %s of variant %s", ErrVariantNotFound, prov.DocumentUUID, variantUUID)
	}
	if err != nil {
		return nil, err
	}
	return NewVariantRecord(v, prov, doc), nil
}

// NewVariantRecord builds the record for variant v of doc, taking scope,
// timestamp, and metadata from doc.
func NewVariantRecord(v *ContentVariant, prov *Provenance, doc *Document) *VariantRecord {
	return &VariantRecord{
		Variant:          *v,
		Provenance:       *prov,
		DocumentMetadata: maps.Clone(doc.Metadata),
		Timestamp:        doc.EffectiveTime(),
		Scope:            doc.Scope,
	}
}

// VariantRecordsGetter is an optional Store interface that returns the
// records for many variants in one lookup. The result is keyed by variant
// UUID; UUIDs with no stored variant are absent from the map rather than
// reported as an error. Retrievers that score outside the store use it to
// resolve a page of candidates without one round trip per candidate.
type VariantRecordsGetter interface {
	GetVariantRecords(ctx context.Context, variantUUIDs []string) (map[string]*VariantRecord, error)
}

// DocumentLister is an optional Store interface that enumerates stored
// documents. In-memory indexes use it to rebuild themselves from the store,
// for example after a process restart.
type DocumentLister interface {
	ListDocumentUUIDs(ctx context.Context) ([]string, error)
}

// --- Source interface ---

// Source fetches raw documents from external systems.
type Source interface {
	Fetch(ctx context.Context) ([]RawDocument, error)
}

// --- ContentExtractor interface ---

// ContentExtractor converts raw bytes into a structured Document with sections and variants.
type ContentExtractor interface {
	Extract(ctx context.Context, raw *RawDocument) (*Document, error)
}

// --- Chunker interface ---

// Chunker refines sections by splitting long ones.
type Chunker interface {
	Chunk(ctx context.Context, doc *Document) (*Document, error)
}

// --- Embedder interfaces ---

// EmbedPurpose tells an embedder why it is embedding its input. Asymmetric
// embedding models produce different vectors for a search query and for the
// passage it should match (for example Gemini RETRIEVAL_QUERY versus
// RETRIEVAL_DOCUMENT, or e5 "query: " versus "passage: " prefixes). It is the
// agent/types purpose, so provider embedders read the same value.
type EmbedPurpose = agenttypes.EmbedPurpose

const (
	// PurposeUnspecified means the caller did not state a purpose. Embedders
	// should use their symmetric default.
	PurposeUnspecified = agenttypes.PurposeUnspecified
	// PurposeDocument marks content that is being indexed for retrieval.
	PurposeDocument = agenttypes.PurposeDocument
	// PurposeQuery marks a search query that is matched against indexed content.
	PurposeQuery = agenttypes.PurposeQuery
)

// WithEmbedPurpose returns a context that carries p to every VariantEmbedder
// called with it. The pipeline sets PurposeDocument during ingest and the
// vector retriever sets PurposeQuery during search, so embedders can select a
// task type without a change to the VariantEmbedder interface.
func WithEmbedPurpose(ctx context.Context, p EmbedPurpose) context.Context {
	return agenttypes.WithEmbedPurpose(ctx, p)
}

// EmbedPurposeFrom returns the purpose carried by ctx, or PurposeUnspecified.
func EmbedPurposeFrom(ctx context.Context) EmbedPurpose {
	return agenttypes.EmbedPurposeFrom(ctx)
}

// ValidateEmbeddings checks that out has exactly one non-empty vector per
// input and that every vector has the same dimension. Call it on the output
// of a single embedder; a registry that dispatches to several embedders must
// check each group separately because their dimensions may differ.
func ValidateEmbeddings(inputs int, out [][]float32) error {
	if len(out) != inputs {
		return fmt.Errorf("%w: got %d vectors for %d inputs", ErrEmbeddingShape, len(out), inputs)
	}
	dim := -1
	for i, vec := range out {
		if len(vec) == 0 {
			return fmt.Errorf("%w: vector %d is empty", ErrEmbeddingShape, i)
		}
		if dim < 0 {
			dim = len(vec)
		} else if len(vec) != dim {
			return fmt.Errorf("%w: vector %d has dimension %d, want %d", ErrEmbeddingShape, i, len(vec), dim)
		}
	}
	return nil
}

// VariantEmbedder generates vector embeddings for content variants of a specific modality.
type VariantEmbedder interface {
	Embed(ctx context.Context, variants []ContentVariant) ([][]float32, error)
}

// EmbedderRegistry dispatches embedding requests to the appropriate VariantEmbedder.
type EmbedderRegistry interface {
	Register(contentType ContentType, embedder VariantEmbedder)
	Embed(ctx context.Context, variants []ContentVariant) ([][]float32, error)
}

// --- Indexer interface ---

// Indexer is an optional interface that retrievers can implement to participate in document ingest/delete.
type Indexer interface {
	Index(ctx context.Context, doc *Document) error
	Remove(ctx context.Context, documentUUID string) error
}

// IndexRebuilder is an optional interface for components that keep a derived
// index and can rebuild it from the store. The BM25 retriever implements it
// over a store that implements DocumentLister, and the pipeline implements it
// by rebuilding every retriever that does. An in-memory index starts empty, so
// call RebuildIndex after opening a persistent store in a new process.
type IndexRebuilder interface {
	RebuildIndex(ctx context.Context) error
}

// --- LLM interface ---

// LLM is a minimal interface for language model generation, decoupled from any specific LLM SDK.
type LLM interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// --- Search pipeline interfaces ---

// QueryTransformer expands or rewrites a query into multiple queries for recall.
//
// Transform may return usable queries together with a non-nil error when only
// part of the expansion failed. The pipeline keeps those queries, falls back
// to the original query when none are returned, and reports the error as
// ErrPartialSearch. Only a canceled or expired context fails the search.
type QueryTransformer interface {
	Transform(ctx context.Context, query string) ([]string, error)
}

// Retriever retrieves search hits for a query.
//
// A retriever that produces usable hits but fails part of its work (for
// example a parent-section lookup) may return those hits together with an
// error wrapping ErrPartialSearch. The pipeline keeps the hits and reports the
// error as a partial failure.
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts *SearchOptions) ([]SearchHit, error)
}

// Reranker reorders search hits using a more expensive model. The pipeline
// passes the fused candidate pool (see WithCandidatePool) and cuts the
// returned list to the search limit, so a reranker may return every input.
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []SearchHit) ([]SearchHit, error)
}

// ContextAssembler builds a prompt with inline citations from search hits.
type ContextAssembler interface {
	Assemble(ctx context.Context, query string, hits []SearchHit) (*AssembledContext, error)
}

// --- Pipeline interface ---

// DedupBehavior controls what happens when a duplicate document is detected.
type DedupBehavior int

// Dedup behaviors.
const (
	DedupSkip DedupBehavior = iota
	DedupReplace
)

// Pipeline orchestrates the full RAG workflow: ingest, search, lookup, update, delete.
//
// Ingest and Search may return a non-nil result together with a non-nil error
// wrapping ErrPartialIngest or ErrPartialSearch respectively; see those
// sentinels for the exact semantics.
type Pipeline interface {
	Ingest(ctx context.Context, raw *RawDocument) (*IngestResult, error)
	Search(ctx context.Context, query string, opts ...SearchOption) (*SearchPipelineResult, error)
	Lookup(ctx context.Context, variantUUID string) (*SearchHit, error)
	Update(ctx context.Context, documentUUID string, raw *RawDocument) (*IngestResult, error)
	Delete(ctx context.Context, documentUUID string) error
	Reconstruct(ctx context.Context, documentUUID string) (*Document, error)
	Close(ctx context.Context) error
}

// IngestResult is the result of ingesting a raw document.
type IngestResult struct {
	DocumentUUID string `json:"document_uuid"`
	Deduplicated bool   `json:"deduplicated"`
	Sections     int    `json:"sections"`
	Variants     int    `json:"variants"`
}
