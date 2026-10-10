// Package types defines the core types and interfaces for the knowledge package.
package types

import (
	"context"
	"errors"
	"time"

	ragtypes "github.com/urmzd/saige/rag/types"
)

// --- Errors ---

// Knowledge graph errors.
var (
	ErrNodeNotFound  = errors.New("node not found")
	ErrStoreNotReady = errors.New("store not ready")
	ErrNoEmbedder    = errors.New("embedder not configured")
	ErrNoExtractor   = errors.New("extractor not configured")

	// ErrPartialSearch indicates a search succeeded on at least one backend
	// but failed on another. The returned results are usable; callers can
	// detect degraded results with errors.Is(err, ErrPartialSearch). It is
	// the same sentinel as the RAG pipeline's, so one errors.Is check covers
	// a partial graph search and a partial pipeline search.
	ErrPartialSearch = ragtypes.ErrPartialSearch

	// ErrPartialEpisode indicates an episode was stored but some of its
	// entities, relations, embeddings, or links failed. IngestEpisode returns
	// it wrapped together with a usable IngestResult; callers can detect it
	// with errors.Is(err, ErrPartialEpisode).
	ErrPartialEpisode = errors.New("partial episode ingest")
)

// --- Graph interface (high-level, orchestrated) ---

// Graph is the top-level interface for knowledge graph operations.
// Implementations orchestrate extraction, embedding, deduplication, and storage.
type Graph interface {
	ApplyOntology(ctx context.Context, ont *Ontology) error
	IngestEpisode(ctx context.Context, input *EpisodeInput) (*IngestResult, error)
	GetEntity(ctx context.Context, id string) (*Entity, error)
	SearchFacts(ctx context.Context, query string, opts ...SearchOption) (*SearchFactsResult, error)
	GetGraph(ctx context.Context, limit int64) (*GraphData, error)
	GetNode(ctx context.Context, id string, depth int) (*NodeDetail, error)
	GetFactProvenance(ctx context.Context, factUUID string) ([]Episode, error)
	Close(ctx context.Context) error
}

// --- Store interface (low-level CRUD, backend-agnostic) ---

// Store is the low-level storage interface that backends implement.
// It handles CRUD operations without business logic like extraction or dedup.
type Store interface {
	// Entity operations
	UpsertEntity(ctx context.Context, entity *ExtractedEntity, embedding []float32) (string, error)
	GetEntity(ctx context.Context, uuid string) (*Entity, error)
	FindEntitiesByNameType(ctx context.Context, name, entityType string) ([]Entity, error)
	FindEntitiesByFuzzyName(ctx context.Context, name string, limit int) ([]Entity, error)

	// Relation operations
	CreateRelation(ctx context.Context, rel *RelationInput) (string, error)
	InvalidateRelation(ctx context.Context, uuid string, invalidAt time.Time) error
	FindRelationsBetweenEntities(ctx context.Context, srcUUID, tgtUUID string) ([]Relation, error)

	// Episode operations
	CreateEpisode(ctx context.Context, input *EpisodeInput, entityUUIDs []string) (string, error)

	// Search operations
	SearchByEmbedding(ctx context.Context, embedding []float32, opts *SearchOptions) ([]ScoredFact, error)
	SearchByText(ctx context.Context, query string, opts *SearchOptions) ([]ScoredFact, error)

	// Graph operations
	GetGraph(ctx context.Context, limit int64) (*GraphData, error)
	GetNode(ctx context.Context, id string, depth int) (*NodeDetail, error)

	// Provenance
	GetFactProvenance(ctx context.Context, factUUID string) ([]Episode, error)

	// Lifecycle
	Close(ctx context.Context) error
}

// EpisodeDeleter is an optional Graph/Store extension for removing all data
// derived from a group: its episodes, mentions, relations, and entities.
// Older rag pipelines ingested each document under GroupID = document UUID;
// DeleteEpisodes still removes data written that way. The groupID must be
// non-empty: the default group ("") holds all legacy single-tenant data and
// cannot be bulk-deleted.
type EpisodeDeleter interface {
	DeleteEpisodes(ctx context.Context, groupID string) error
}

// DocumentEpisodeDeleter is an optional Graph/Store extension for removing
// the episodes of one source document inside a group. Relations asserted
// only by those episodes are removed; relations that another episode also
// asserts are kept. Entities left with no mentions and no relations are
// removed. Entities shared with other documents survive, so a group can hold
// one canonical node per entity across many documents. A relation that a
// removed relation had superseded is valid again unless a remaining relation
// supersedes it.
type DocumentEpisodeDeleter interface {
	DeleteDocumentEpisodes(ctx context.Context, groupID, documentID string) error
}

// EpisodeLinker is an optional Store extension that records which episode
// asserted which relation. With it, the engine creates the episode before
// its entities and relations, links each relation (new or matched as a
// duplicate) to the episode, and GetFactProvenance returns the asserting
// episodes instead of every episode that mentions an endpoint.
type EpisodeLinker interface {
	// LinkEpisodeEntities records that the episode mentions the entities.
	LinkEpisodeEntities(ctx context.Context, episodeUUID string, entityUUIDs []string) error
	// LinkRelationEpisode records that the episode asserts the relation.
	LinkRelationEpisode(ctx context.Context, relationUUID, episodeUUID string) error
}

// GroupScopedStore is an optional Store extension for tenant isolation.
// Stores that implement it scope entity lookups to a group, so the engine
// deduplicates entities only within the episode's GroupID. Stores that do
// not implement it keep the legacy global (single-tenant) behavior.
type GroupScopedStore interface {
	FindEntitiesByNameTypeInGroup(ctx context.Context, groupID, name, entityType string) ([]Entity, error)
	FindEntitiesByFuzzyNameInGroup(ctx context.Context, groupID, name string, limit int) ([]Entity, error)
}

// --- Search options ---

// SearchOptions holds parsed search options.
type SearchOptions struct {
	GroupID string
	Limit   int
	// ValidAt, when set, returns the relations that were valid at that
	// instant (valid_at <= t and not yet invalidated at t) instead of the
	// currently valid ones.
	ValidAt *time.Time
}

// SearchOption configures a search query.
type SearchOption func(*SearchOptions)

// WithGroupID filters search to a specific group.
func WithGroupID(id string) SearchOption {
	return func(o *SearchOptions) { o.GroupID = id }
}

// WithLimit sets the max number of results.
func WithLimit(n int) SearchOption {
	return func(o *SearchOptions) { o.Limit = n }
}

// WithValidAt runs an as-of query: only relations valid at t are returned,
// including ones that a later fact has since superseded.
func WithValidAt(t time.Time) SearchOption {
	return func(o *SearchOptions) { o.ValidAt = &t }
}

// --- Core types ---

// Entity represents a node in the knowledge graph.
type Entity struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
}

// Relation represents an edge in the knowledge graph with temporal tracking.
type Relation struct {
	UUID       string     `json:"uuid"`
	SourceUUID string     `json:"source_uuid"`
	TargetUUID string     `json:"target_uuid"`
	Type       string     `json:"type"`
	Fact       string     `json:"fact"`
	CreatedAt  time.Time  `json:"created_at"`
	ValidAt    time.Time  `json:"valid_at"`
	InvalidAt  *time.Time `json:"invalid_at,omitempty"`
}

// RelationInput is input for creating a new relation.
type RelationInput struct {
	SourceUUID string
	TargetUUID string
	Type       string
	Fact       string
	ValidAt    time.Time
	// InvalidAt, when set, creates the relation already superseded. The
	// engine sets it when a backfilled fact is older than a stored fact of
	// the same kind.
	InvalidAt *time.Time
	// GroupID scopes the relation to a tenant group. Empty means the
	// default (single-tenant) group.
	GroupID string
}

// Fact is a relation with resolved source and target entities.
type Fact struct {
	UUID       string     `json:"uuid"`
	Name       string     `json:"name"`
	FactText   string     `json:"fact"`
	SourceNode Entity     `json:"source_node"`
	TargetNode Entity     `json:"target_node"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	ValidAt    time.Time  `json:"valid_at,omitempty"`
	InvalidAt  *time.Time `json:"invalid_at,omitempty"`
}

// ScoredFact is a fact with a relevance score from search.
type ScoredFact struct {
	Fact  Fact
	Score float64
}

// Episode represents an ingested text episode.
type Episode struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	Source  string `json:"source"`
	GroupID string `json:"group_id"`
	// DocumentID names the source document the episode came from, when the
	// caller supplied one.
	DocumentID string            `json:"document_id,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// GraphData holds nodes and edges for visualization.
type GraphData struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphNode is a node for graph visualization.
type GraphNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Summary string `json:"summary,omitempty"`
}

// GraphEdge is an edge for graph visualization.
type GraphEdge struct {
	ID        string     `json:"id"`
	Source    string     `json:"source"`
	Target    string     `json:"target"`
	Type      string     `json:"type"`
	Fact      string     `json:"fact,omitempty"`
	Weight    float64    `json:"weight"`
	CreatedAt time.Time  `json:"created_at,omitempty"`
	ValidAt   time.Time  `json:"valid_at,omitempty"`
	InvalidAt *time.Time `json:"invalid_at,omitempty"`
}

// NodeDetail holds a node with its neighbors and edges.
type NodeDetail struct {
	Node      GraphNode   `json:"node"`
	Neighbors []GraphNode `json:"neighbors"`
	Edges     []GraphEdge `json:"edges"`
	// Truncated reports that the traversal stopped at the store's node or
	// edge cap, so Neighbors and Edges are incomplete.
	Truncated bool `json:"truncated,omitempty"`
}

// EpisodeInput is input for ingesting an episode.
type EpisodeInput struct {
	Name   string `json:"name"`
	Body   string `json:"episode_body"`
	Source string `json:"source_description"`
	// GroupID is the tenant scope. Entities are deduplicated within a group,
	// so episodes from different documents that name the same entity share
	// one node.
	GroupID string `json:"group_id"`
	// DocumentID names the source document. It lets
	// DocumentEpisodeDeleter remove one document's episodes without
	// touching the rest of the group.
	DocumentID string `json:"document_id,omitempty"`
	// ReferenceTime is when the episode's facts became true, for example the
	// date of a backfilled document. It sets the ValidAt of the relations the
	// episode creates. The zero value means the ingest time.
	ReferenceTime time.Time         `json:"reference_time,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// IngestResult is the result of ingesting an episode.
type IngestResult struct {
	UUID          string     `json:"uuid"`
	Name          string     `json:"name"`
	EntityNodes   []Entity   `json:"entity_nodes"`
	EpisodicEdges []Relation `json:"episodic_edges"`
}

// SearchFactsResult holds search results.
type SearchFactsResult struct {
	Facts []Fact `json:"facts"`
}

// --- Ontology ---

// Ontology defines the schema for entities and relations.
type Ontology struct {
	EntityTypes   []EntityTypeDef
	RelationTypes []RelationTypeDef
}

// EntityTypeDef defines an entity type.
type EntityTypeDef struct {
	Name        string
	Description string
}

// RelationTypeDef defines a relation type.
type RelationTypeDef struct {
	Name        string
	Description string
	SourceType  string
	TargetType  string
}

// --- Embedder ---

// Embedder generates vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// --- Extractor ---

// ExtractedEntity is an entity extracted from text.
type ExtractedEntity struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Summary string `json:"summary"`
	// GroupID scopes the entity to a tenant group. It is set by the engine
	// from the episode's GroupID, not by extractors. Empty means the
	// default (single-tenant) group.
	GroupID string `json:"-"`
}

// ExtractedRelation is a relation extracted from text.
type ExtractedRelation struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
	Fact   string `json:"fact"`
}

// Extractor extracts entities and relations from text.
type Extractor interface {
	Extract(ctx context.Context, text string) ([]ExtractedEntity, []ExtractedRelation, error)
}

// OntologyExtractor is an optional Extractor extension that constrains
// extraction to an ontology. The engine calls it instead of Extract when an
// ontology has been applied.
type OntologyExtractor interface {
	ExtractWithOntology(ctx context.Context, text string, ont *Ontology) ([]ExtractedEntity, []ExtractedRelation, error)
}
