// Package types defines the core types and interfaces for the knowledge package.
package types

import (
	"context"
	"errors"
	"time"
)

// --- Errors ---

var (
	ErrNodeNotFound  = errors.New("node not found")
	ErrStoreNotReady = errors.New("store not ready")
	ErrNoEmbedder    = errors.New("embedder not configured")
	ErrNoExtractor   = errors.New("extractor not configured")
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

// --- Search options ---

// SearchOptions holds parsed search options.
type SearchOptions struct {
	GroupID string
	Limit   int
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
	UUID      string    `json:"uuid"`
	Name      string    `json:"name"`
	Body      string    `json:"body"`
	Source    string    `json:"source"`
	GroupID   string    `json:"group_id"`
	CreatedAt time.Time `json:"created_at"`
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
	Target   string     `json:"target"`
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
}

// EpisodeInput is input for ingesting an episode.
type EpisodeInput struct {
	Name     string            `json:"name"`
	Body     string            `json:"episode_body"`
	Source   string            `json:"source_description"`
	GroupID  string            `json:"group_id"`
	Metadata map[string]string `json:"metadata,omitempty"`
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
