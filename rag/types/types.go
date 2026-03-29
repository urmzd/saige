// Package types defines the core types and interfaces for rag.
package types

import (
	"context"
	"errors"
	"time"
)

// --- Errors ---

var (
	ErrDocumentNotFound    = errors.New("document not found")
	ErrVariantNotFound     = errors.New("variant not found")
	ErrDuplicateDocument   = errors.New("duplicate document")
	ErrNoExtractor         = errors.New("content extractor not configured")
	ErrNoStore             = errors.New("store not configured")
	ErrNoRetriever         = errors.New("no retriever configured")
	ErrUnsupportedMIMEType = errors.New("unsupported MIME type")
)

// --- Content types ---

// ContentType represents the modality of a content variant.
type ContentType string

const (
	ContentText  ContentType = "text"
	ContentImage ContentType = "image"
	ContentTable ContentType = "table"
	ContentAudio ContentType = "audio"
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
	UUID        string            `json:"uuid"`
	SourceURI   string            `json:"source_uri"`
	Fingerprint string            `json:"fingerprint"`
	Title       string            `json:"title,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Sections    []Section         `json:"sections"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// RawDocument represents unprocessed input from a source.
type RawDocument struct {
	SourceURI string            `json:"source_uri"`
	MIMEType  string            `json:"mime_type"`
	Data      []byte            `json:"data"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// --- Provenance and citation types ---

// Provenance tracks the origin of a search hit for citation purposes.
type Provenance struct {
	DocumentUUID string `json:"document_uuid"`
	DocumentTitle string `json:"document_title,omitempty"`
	SourceURI    string `json:"source_uri,omitempty"`
	SectionUUID  string `json:"section_uuid"`
	SectionHeading string `json:"section_heading,omitempty"`
	SectionIndex int    `json:"section_index"`
}

// SearchHit is a scored content variant with full provenance for citation tracking.
type SearchHit struct {
	Variant    ContentVariant `json:"variant"`
	Score      float64        `json:"score"`
	Provenance Provenance     `json:"provenance"`
}

// FilterOp defines metadata filter comparison operations.
type FilterOp string

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
	Context            *AssembledContext  `json:"context,omitempty"`
}

// --- Search options ---

// SearchOption configures a search query.
type SearchOption func(*SearchConfig)

// SearchConfig holds parsed search options for pipeline search.
type SearchConfig struct {
	ContentTypes    []ContentType
	Limit           int
	MetadataFilters []MetadataFilter
	MinScore        float64
	AssembleContext bool
	MaxTokens       int
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

// WithMinScore sets the minimum score threshold for results.
func WithMinScore(score float64) SearchOption {
	return func(c *SearchConfig) { c.MinScore = score }
}

// WithContextAssembly enables context assembly with inline citations.
func WithContextAssembly(maxTokens int) SearchOption {
	return func(c *SearchConfig) {
		c.AssembleContext = true
		c.MaxTokens = maxTokens
	}
}

// --- Search options for Store ---

// SearchOptions configures a vector search query at the store level.
type SearchOptions struct {
	ContentTypes    []ContentType
	Limit           int
	MetadataFilters []MetadataFilter
	MinScore        float64
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

// --- LLM interface ---

// LLM is a minimal interface for language model generation, decoupled from any specific LLM SDK.
type LLM interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// --- Search pipeline interfaces ---

// QueryTransformer expands or rewrites a query into multiple queries for recall.
type QueryTransformer interface {
	Transform(ctx context.Context, query string) ([]string, error)
}

// Retriever retrieves search hits for a query.
type Retriever interface {
	Retrieve(ctx context.Context, query string, opts *SearchOptions) ([]SearchHit, error)
}

// Reranker reorders search hits using a more expensive model.
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

const (
	DedupSkip    DedupBehavior = iota
	DedupReplace
)

// Pipeline orchestrates the full RAG workflow: ingest, search, lookup, update, delete.
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

