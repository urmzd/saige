// Package engine provides the GraphEngine that orchestrates extraction,
// embedding, deduplication, and storage via the Store interface.
package engine

import (
	"context"
	"errors"
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
		resolvedUUID, err := e.deduplicateAndUpsertEntity(ctx, input.GroupID, &ent)
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
			GroupID:    input.GroupID,
		})
		if err != nil {
			e.logger.Warn("create relation failed", "type", rel.Type, "error", err)
			continue
		}

		// Contradiction invalidation: a new relation for an existing
		// (source, target, type) supersedes any active prior relation(s) of the
		// same type. Mark those priors invalid as of the new relation's ValidAt.
		e.invalidateSupersededRelations(ctx, srcUUID, tgtUUID, rel.Type, relUUID, now)

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
// Dedup candidates are scoped to groupID when the store supports it, so
// entities never merge across tenant groups.
func (e *GraphEngine) deduplicateAndUpsertEntity(ctx context.Context, groupID string, ent *types.ExtractedEntity) (string, error) {
	ent.GroupID = groupID

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
	existing, err := e.findEntitiesByNameType(ctx, groupID, ent.Name, ent.Type)
	if err == nil && len(existing) > 0 {
		// Exact match: upsert will update summary/embedding
		uuid, err := e.store.UpsertEntity(ctx, ent, embedding)
		return uuid, err
	}

	// Try fuzzy match: find candidates with similar names
	candidates, err := e.findEntitiesByFuzzyName(ctx, groupID, ent.Name, 10)
	if err == nil {
		for _, candidate := range candidates {
			if fuzzy.IsFuzzyMatch(ent.Name, candidate.Name, FuzzyMatchThreshold) {
				// Fuzzy match found: update the existing entity with new data
				e.logger.Info("fuzzy entity merge",
					"new", ent.Name, "existing", candidate.Name,
					"similarity", fuzzy.Similarity(ent.Name, candidate.Name))
				merged := &types.ExtractedEntity{
					Name:    candidate.Name, // keep the canonical name
					Type:    ent.Type,
					Summary: ent.Summary, // use newer summary
					GroupID: groupID,
				}
				uuid, err := e.store.UpsertEntity(ctx, merged, embedding)
				return uuid, err
			}
		}
	}

	// No match: create new entity
	return e.store.UpsertEntity(ctx, ent, embedding)
}

// findEntitiesByNameType uses group-scoped lookup when the store supports it,
// falling back to the legacy global lookup otherwise.
func (e *GraphEngine) findEntitiesByNameType(ctx context.Context, groupID, name, entityType string) ([]types.Entity, error) {
	if gs, ok := e.store.(types.GroupScopedStore); ok {
		return gs.FindEntitiesByNameTypeInGroup(ctx, groupID, name, entityType)
	}
	return e.store.FindEntitiesByNameType(ctx, name, entityType)
}

// findEntitiesByFuzzyName uses group-scoped lookup when the store supports it,
// falling back to the legacy global lookup otherwise.
func (e *GraphEngine) findEntitiesByFuzzyName(ctx context.Context, groupID, name string, limit int) ([]types.Entity, error) {
	if gs, ok := e.store.(types.GroupScopedStore); ok {
		return gs.FindEntitiesByFuzzyNameInGroup(ctx, groupID, name, limit)
	}
	return e.store.FindEntitiesByFuzzyName(ctx, name, limit)
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

// invalidateSupersededRelations marks any active prior relation between
// (srcUUID, tgtUUID) of the same relType as invalid as of invalidAt. The newly
// created relation (newRelUUID) and any already-invalidated relations are left
// untouched. This is a rule-based supersession: same source+target+type
// supersedes, no LLM judge involved.
func (e *GraphEngine) invalidateSupersededRelations(ctx context.Context, srcUUID, tgtUUID, relType, newRelUUID string, invalidAt time.Time) {
	existing, err := e.store.FindRelationsBetweenEntities(ctx, srcUUID, tgtUUID)
	if err != nil {
		e.logger.Warn("supersession lookup failed", "source", srcUUID, "target", tgtUUID, "error", err)
		return
	}
	for _, prior := range existing {
		if prior.UUID == newRelUUID {
			continue // never invalidate the relation we just created
		}
		if prior.Type != relType {
			continue // only same-type edges supersede each other
		}
		if prior.InvalidAt != nil {
			continue // already invalidated
		}
		if err := e.store.InvalidateRelation(ctx, prior.UUID, invalidAt); err != nil {
			e.logger.Warn("invalidate superseded relation failed", "relation", prior.UUID, "error", err)
			continue
		}
		e.logger.Info("invalidated superseded relation",
			"relation", prior.UUID, "type", relType, "superseded_by", newRelUUID)
	}
}

// GetEntity retrieves an entity by UUID.
func (e *GraphEngine) GetEntity(ctx context.Context, id string) (*types.Entity, error) {
	return e.store.GetEntity(ctx, id)
}

// DeleteEpisodes implements types.EpisodeDeleter by delegating to the store.
// It errors when the configured store cannot delete episodes, so callers
// never mistake a no-op for a completed cleanup.
func (e *GraphEngine) DeleteEpisodes(ctx context.Context, groupID string) error {
	if e.store == nil {
		return types.ErrStoreNotReady
	}
	ed, ok := e.store.(types.EpisodeDeleter)
	if !ok {
		return fmt.Errorf("store %T does not support episode deletion", e.store)
	}
	return ed.DeleteEpisodes(ctx, groupID)
}

// SearchFacts combines vector and BM25 search using Reciprocal Rank Fusion.
// If every attempted backend fails the error is returned. If only some fail,
// the surviving results are returned together with an error wrapping
// types.ErrPartialSearch so callers can detect degraded results.
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
	var searchErrs []error
	attempted := 0

	// Vector search (requires embedder)
	if e.embedder != nil {
		attempted++
		embeddings, err := e.embedder.Embed(ctx, []string{query})
		switch {
		case err != nil:
			searchErrs = append(searchErrs, fmt.Errorf("embed query: %w", err))
		case len(embeddings) == 0:
			searchErrs = append(searchErrs, fmt.Errorf("embed query: no embedding returned"))
		default:
			vectorResults, err = e.store.SearchByEmbedding(ctx, embeddings[0], o)
			if err != nil {
				searchErrs = append(searchErrs, fmt.Errorf("vector search: %w", err))
			}
		}
	}

	// BM25 text search
	attempted++
	var err error
	bm25Results, err = e.store.SearchByText(ctx, query, o)
	if err != nil {
		searchErrs = append(searchErrs, fmt.Errorf("text search: %w", err))
	}

	if len(searchErrs) == attempted {
		return nil, errors.Join(searchErrs...)
	}

	// Combine via RRF
	facts := reciprocalRankFusion(vectorResults, bm25Results, limit)

	result := &types.SearchFactsResult{Facts: facts}
	if len(searchErrs) > 0 {
		return result, fmt.Errorf("%w: %w", types.ErrPartialSearch, errors.Join(searchErrs...))
	}
	return result, nil
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
