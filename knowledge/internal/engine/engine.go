// Package engine provides the GraphEngine that orchestrates extraction,
// embedding, deduplication, and storage via the Store interface.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/urmzd/saige/knowledge/internal/fuzzy"
	"github.com/urmzd/saige/knowledge/types"
)

const (
	// FuzzyMatchThreshold is the minimum similarity score for entity dedup.
	FuzzyMatchThreshold = 0.8

	// EdgeDedupEmbeddingSimilarityThreshold is the minimum embedding similarity
	// for two relations to be considered duplicates.
	EdgeDedupEmbeddingSimilarityThreshold = 0.92

	// RRFConstant is the k parameter for Reciprocal Rank Fusion.
	RRFConstant = 60
)

// GraphEngine implements types.Graph by orchestrating Store + Extractor + Embedder.
type GraphEngine struct {
	store     types.Store
	extractor types.Extractor
	embedder  types.Embedder
	ontology  *types.Ontology
	logger    *slog.Logger
}

// Option configures a GraphEngine.
type Option func(*GraphEngine)

// WithStore sets the storage backend.
func WithStore(s types.Store) Option {
	return func(e *GraphEngine) { e.store = s }
}

// WithExtractor sets the entity/relation extractor.
func WithExtractor(ext types.Extractor) Option {
	return func(e *GraphEngine) { e.extractor = ext }
}

// WithEmbedder sets the vector embedder.
func WithEmbedder(emb types.Embedder) Option {
	return func(e *GraphEngine) { e.embedder = emb }
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) Option {
	return func(e *GraphEngine) { e.logger = logger }
}

// New creates a new GraphEngine.
func New(opts ...Option) *GraphEngine {
	e := &GraphEngine{logger: slog.Default()}
	for _, o := range opts {
		o(e)
	}
	return e
}

// ApplyOntology stores the ontology for use during extraction.
func (e *GraphEngine) ApplyOntology(_ context.Context, ont *types.Ontology) error {
	e.ontology = ont
	return nil
}

// IngestEpisode extracts entities/relations from text, deduplicates, and stores them.
func (e *GraphEngine) IngestEpisode(ctx context.Context, input *types.EpisodeInput) (*types.IngestResult, error) {
	if e.extractor == nil {
		return nil, types.ErrNoExtractor
	}
	if e.store == nil {
		return nil, types.ErrStoreNotReady
	}

	// Step 1: Extract entities and relations from text
	extractedEntities, extractedRelations, err := e.extractor.Extract(ctx, input.Body)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}

	// Step 2: Deduplicate and upsert entities
	// entityUUIDs maps extracted entity name → stored UUID
	entityUUIDs := make(map[string]string, len(extractedEntities))
	responseEntities := make([]types.Entity, 0, len(extractedEntities))

	for _, ent := range extractedEntities {
		resolvedUUID, err := e.deduplicateAndUpsertEntity(ctx, &ent)
		if err != nil {
			e.logger.Warn("upsert entity failed", "entity", ent.Name, "error", err)
			continue
		}
		entityUUIDs[ent.Name] = resolvedUUID
		responseEntities = append(responseEntities, types.Entity{
			UUID: resolvedUUID, Name: ent.Name, Type: ent.Type, Summary: ent.Summary,
		})
	}

	// Step 3: Deduplicate and create relations with temporal tracking
	now := time.Now()
	responseRelations := make([]types.Relation, 0, len(extractedRelations))

	for _, rel := range extractedRelations {
		srcUUID, ok := entityUUIDs[rel.Source]
		if !ok {
			continue
		}
		tgtUUID, ok := entityUUIDs[rel.Target]
		if !ok {
			continue
		}

		// Edge dedup: check for existing similar relations
		isDuplicate, err := e.isRelationDuplicate(ctx, srcUUID, tgtUUID, rel.Fact)
		if err != nil {
			e.logger.Warn("edge dedup check failed", "error", err)
		}
		if isDuplicate {
			e.logger.Debug("skipping duplicate relation", "source", rel.Source, "target", rel.Target, "type", rel.Type)
			continue
		}

		relUUID, err := e.store.CreateRelation(ctx, &types.RelationInput{
			SourceUUID: srcUUID,
			TargetUUID: tgtUUID,
			Type:       rel.Type,
			Fact:       rel.Fact,
			ValidAt:    now,
		})
		if err != nil {
			e.logger.Warn("create relation failed", "type", rel.Type, "error", err)
			continue
		}

		responseRelations = append(responseRelations, types.Relation{
			UUID:       relUUID,
			SourceUUID: srcUUID,
			TargetUUID: tgtUUID,
			Type:       rel.Type,
			Fact:       rel.Fact,
			CreatedAt:  now,
			ValidAt:    now,
		})
	}

	// Step 4: Create episode and link to entities
	uuids := make([]string, 0, len(entityUUIDs))
	for _, uuid := range entityUUIDs {
		uuids = append(uuids, uuid)
	}

	episodeUUID, err := e.store.CreateEpisode(ctx, input, uuids)
	if err != nil {
		e.logger.Warn("create episode failed", "name", input.Name, "error", err)
	}

	return &types.IngestResult{
		UUID:          episodeUUID,
		Name:          input.Name,
		EntityNodes:   responseEntities,
		EpisodicEdges: responseRelations,
	}, nil
}

// deduplicateAndUpsertEntity performs fuzzy entity deduplication then upserts.
func (e *GraphEngine) deduplicateAndUpsertEntity(ctx context.Context, ent *types.ExtractedEntity) (string, error) {
	// Generate embedding
	var embedding []float32
	if e.embedder != nil {
		embeddings, err := e.embedder.Embed(ctx, []string{fmt.Sprintf("%s %s", ent.Name, ent.Summary)})
		if err == nil && len(embeddings) > 0 {
			embedding = embeddings[0]
		} else if err != nil {
			e.logger.Warn("embedding failed", "entity", ent.Name, "error", err)
		}
	}

	// Try exact match first (handled by UpsertEntity's name+type check)
	existing, err := e.store.FindEntitiesByNameType(ctx, ent.Name, ent.Type)
	if err == nil && len(existing) > 0 {
		// Exact match — upsert will update summary/embedding
		uuid, err := e.store.UpsertEntity(ctx, ent, embedding)
		return uuid, err
	}

	// Try fuzzy match: find candidates with similar names
	candidates, err := e.store.FindEntitiesByFuzzyName(ctx, ent.Name, 10)
	if err == nil {
		for _, candidate := range candidates {
			if fuzzy.IsFuzzyMatch(ent.Name, candidate.Name, FuzzyMatchThreshold) {
				// Fuzzy match found — update the existing entity with new data
				e.logger.Info("fuzzy entity merge",
					"new", ent.Name, "existing", candidate.Name,
					"similarity", fuzzy.Similarity(ent.Name, candidate.Name))
				merged := &types.ExtractedEntity{
					Name:    candidate.Name, // keep the canonical name
					Type:    ent.Type,
					Summary: ent.Summary, // use newer summary
				}
				uuid, err := e.store.UpsertEntity(ctx, merged, embedding)
				return uuid, err
			}
		}
	}

	// No match — create new entity
	return e.store.UpsertEntity(ctx, ent, embedding)
}

// isRelationDuplicate checks if a similar relation already exists between entities.
func (e *GraphEngine) isRelationDuplicate(ctx context.Context, srcUUID, tgtUUID, fact string) (bool, error) {
	existing, err := e.store.FindRelationsBetweenEntities(ctx, srcUUID, tgtUUID)
	if err != nil {
		return false, err
	}

	for _, rel := range existing {
		if rel.InvalidAt != nil {
			continue // skip invalidated relations
		}
		// Check text similarity for edge dedup
		if fuzzy.Similarity(rel.Fact, fact) >= EdgeDedupEmbeddingSimilarityThreshold {
			return true, nil
		}
	}
	return false, nil
}

// GetEntity retrieves an entity by UUID.
func (e *GraphEngine) GetEntity(ctx context.Context, id string) (*types.Entity, error) {
	return e.store.GetEntity(ctx, id)
}

// SearchFacts combines vector and BM25 search using Reciprocal Rank Fusion.
func (e *GraphEngine) SearchFacts(ctx context.Context, query string, opts ...types.SearchOption) (*types.SearchFactsResult, error) {
	o := &types.SearchOptions{}
	for _, opt := range opts {
		opt(o)
	}

	limit := o.Limit
	if limit <= 0 {
		limit = 20
	}

	// Run vector search and BM25 search
	var vectorResults []types.ScoredFact
	var bm25Results []types.ScoredFact

	// Vector search (requires embedder)
	if e.embedder != nil {
		embeddings, err := e.embedder.Embed(ctx, []string{query})
		if err == nil && len(embeddings) > 0 {
			vectorResults, _ = e.store.SearchByEmbedding(ctx, embeddings[0], o)
		}
	}

	// BM25 text search
	bm25Results, _ = e.store.SearchByText(ctx, query, o)

	// Combine via RRF
	facts := reciprocalRankFusion(vectorResults, bm25Results, limit)

	return &types.SearchFactsResult{Facts: facts}, nil
}

// reciprocalRankFusion combines two ranked lists using RRF scoring.
func reciprocalRankFusion(listA, listB []types.ScoredFact, limit int) []types.Fact {
	scores := make(map[string]float64)
	factMap := make(map[string]types.Fact)

	for rank, sf := range listA {
		scores[sf.Fact.UUID] += 1.0 / float64(RRFConstant+rank+1)
		factMap[sf.Fact.UUID] = sf.Fact
	}

	for rank, sf := range listB {
		scores[sf.Fact.UUID] += 1.0 / float64(RRFConstant+rank+1)
		factMap[sf.Fact.UUID] = sf.Fact
	}

	type scored struct {
		uuid  string
		score float64
	}
	ranked := make([]scored, 0, len(scores))
	for uuid, s := range scores {
		ranked = append(ranked, scored{uuid, s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	if limit > len(ranked) {
		limit = len(ranked)
	}

	facts := make([]types.Fact, limit)
	for i := 0; i < limit; i++ {
		facts[i] = factMap[ranked[i].uuid]
	}
	return facts
}

// GetGraph returns the full graph data.
func (e *GraphEngine) GetGraph(ctx context.Context, limit int64) (*types.GraphData, error) {
	return e.store.GetGraph(ctx, limit)
}

// GetNode returns a node with its neighbors and edges at the requested depth.
func (e *GraphEngine) GetNode(ctx context.Context, id string, depth int) (*types.NodeDetail, error) {
	return e.store.GetNode(ctx, id, depth)
}

// GetFactProvenance returns the episodes that sourced a given fact.
func (e *GraphEngine) GetFactProvenance(ctx context.Context, factUUID string) ([]types.Episode, error) {
	return e.store.GetFactProvenance(ctx, factUUID)
}

// Close closes the underlying store.
func (e *GraphEngine) Close(ctx context.Context) error {
	if e.store != nil {
		return e.store.Close(ctx)
	}
	return nil
}
