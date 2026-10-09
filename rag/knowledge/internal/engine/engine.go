// Package engine provides the GraphEngine that orchestrates extraction,
// embedding, deduplication, and storage via the Store interface.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/urmzd/saige/rag/knowledge/internal/fuzzy"
	"github.com/urmzd/saige/rag/knowledge/types"
)

const (
	// FuzzyMatchThreshold is the minimum similarity score for entity dedup.
	FuzzyMatchThreshold = 0.8

	// EdgeDedupTextSimilarityThreshold is the minimum text similarity between
	// two relation facts for them to be considered duplicates.
	EdgeDedupTextSimilarityThreshold = 0.92

	// RRFConstant is the k parameter for Reciprocal Rank Fusion.
	RRFConstant = 60

	// DefaultSearchLimit is the number of facts SearchFacts returns when the
	// caller sets no limit.
	DefaultSearchLimit = 20
)

// GraphEngine implements types.Graph by orchestrating Store + Extractor + Embedder.
type GraphEngine struct {
	store          types.Store
	extractor      types.Extractor
	embedder       types.Embedder
	logger         *slog.Logger
	strictOntology bool

	// mu guards ontology, which ApplyOntology may replace while episodes
	// are being ingested.
	mu       sync.RWMutex
	ontology *types.Ontology
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

// WithStrictOntology drops extracted entities and relations whose type is
// not in the applied ontology. Without it, unknown types are kept as the
// extractor returned them. Each list is enforced only when the ontology
// defines at least one type of that kind.
func WithStrictOntology() Option {
	return func(e *GraphEngine) { e.strictOntology = true }
}

// New creates a new GraphEngine.
func New(opts ...Option) *GraphEngine {
	e := &GraphEngine{logger: slog.Default()}
	for _, o := range opts {
		o(e)
	}
	return e
}

// ApplyOntology sets the ontology used by later IngestEpisode calls. The
// extractor receives it when it implements types.OntologyExtractor, and
// extracted type names that match an ontology type case-insensitively are
// rewritten to the ontology's spelling. A nil ontology clears it. The
// ontology is copied, so later changes by the caller have no effect.
func (e *GraphEngine) ApplyOntology(_ context.Context, ont *types.Ontology) error {
	var cp *types.Ontology
	if ont != nil {
		cp = &types.Ontology{
			EntityTypes:   slices.Clone(ont.EntityTypes),
			RelationTypes: slices.Clone(ont.RelationTypes),
		}
	}
	e.mu.Lock()
	e.ontology = cp
	e.mu.Unlock()
	return nil
}

// currentOntology returns the ontology snapshot for one ingest.
func (e *GraphEngine) currentOntology() *types.Ontology {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ontology
}

// IngestEpisode extracts entities/relations from text, deduplicates, and stores them.
//
// A failure to extract or to store the episode itself is returned as an
// error with a nil result. Failures of individual entities, relations,
// embeddings, or links do not stop the ingest: the stored parts are
// returned together with an error wrapping types.ErrPartialEpisode.
func (e *GraphEngine) IngestEpisode(ctx context.Context, input *types.EpisodeInput) (*types.IngestResult, error) {
	if e.extractor == nil {
		return nil, types.ErrNoExtractor
	}
	if e.store == nil {
		return nil, types.ErrStoreNotReady
	}

	// Step 1: Extract entities and relations from text
	ont := e.currentOntology()
	extractedEntities, extractedRelations, err := e.extract(ctx, input.Body, ont)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	extractedEntities, extractedRelations = e.normalizeToOntology(ont, extractedEntities, extractedRelations)

	var partial []error

	// Step 2: Create the episode first when the store can link it later, so
	// every relation can name the episode that asserted it.
	linker, canLink := e.store.(types.EpisodeLinker)
	var episodeUUID string
	if canLink {
		episodeUUID, err = e.store.CreateEpisode(ctx, input, nil)
		if err != nil {
			return nil, fmt.Errorf("create episode %s: %w", input.Name, err)
		}
	}

	// Step 3: Embed all entities in one call, then deduplicate and upsert.
	embeddings, err := e.embedEntities(ctx, extractedEntities)
	if err != nil {
		partial = append(partial, err)
	}

	// entityUUIDs maps extracted entity name → stored UUID
	entityUUIDs := make(map[string]string, len(extractedEntities))
	storedUUIDs := make([]string, 0, len(extractedEntities))
	stored := make(map[string]bool, len(extractedEntities))
	responseEntities := make([]types.Entity, 0, len(extractedEntities))

	for i := range extractedEntities {
		ent := extractedEntities[i]
		var embedding []float32
		if embeddings != nil {
			embedding = embeddings[i]
		}
		resolvedUUID, err := e.deduplicateAndUpsertEntity(ctx, input.GroupID, &ent, embedding)
		if err != nil {
			partial = append(partial, fmt.Errorf("upsert entity %s: %w", ent.Name, err))
			continue
		}
		if !stored[resolvedUUID] {
			stored[resolvedUUID] = true
			storedUUIDs = append(storedUUIDs, resolvedUUID)
		}
		entityUUIDs[ent.Name] = resolvedUUID
		responseEntities = append(responseEntities, types.Entity{
			UUID: resolvedUUID, Name: ent.Name, Type: ent.Type, Summary: ent.Summary,
		})
	}

	if canLink && len(storedUUIDs) > 0 {
		if err := linker.LinkEpisodeEntities(ctx, episodeUUID, storedUUIDs); err != nil {
			partial = append(partial, fmt.Errorf("link episode entities: %w", err))
		}
	}

	// Step 4: Deduplicate and create relations with temporal tracking.
	// Relations become valid at the episode's reference time, so a
	// backfilled document does not look newer than what it describes.
	now := time.Now()
	validAt := input.ReferenceTime
	if validAt.IsZero() {
		validAt = now
	}
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

		existing, err := e.store.FindRelationsBetweenEntities(ctx, srcUUID, tgtUUID)
		if err != nil {
			// Without the existing relations the edge cannot be deduplicated
			// or superseded; creating it anyway keeps the fact.
			partial = append(partial, fmt.Errorf("find relations %s->%s: %w", rel.Source, rel.Target, err))
			existing = nil
		}
		existing = sameDirection(existing, srcUUID, tgtUUID)

		if dupUUID := duplicateRelation(existing, rel.Fact); dupUUID != "" {
			e.logger.Debug("skipping duplicate relation", "source", rel.Source, "target", rel.Target, "type", rel.Type)
			if canLink {
				if err := linker.LinkRelationEpisode(ctx, dupUUID, episodeUUID); err != nil {
					partial = append(partial, fmt.Errorf("link relation %s: %w", dupUUID, err))
				}
			}
			continue
		}

		// Contradiction handling: an active prior relation of the same type
		// and direction is superseded when it became valid no later than the
		// new one. When a prior is newer, the new (backfilled) relation is
		// created already superseded at the prior's ValidAt.
		superseded, newInvalidAt := supersession(existing, rel.Type, validAt)

		relUUID, err := e.store.CreateRelation(ctx, &types.RelationInput{
			SourceUUID: srcUUID,
			TargetUUID: tgtUUID,
			Type:       rel.Type,
			Fact:       rel.Fact,
			ValidAt:    validAt,
			InvalidAt:  newInvalidAt,
			GroupID:    input.GroupID,
		})
		if err != nil {
			partial = append(partial, fmt.Errorf("create relation %s: %w", rel.Type, err))
			continue
		}

		if canLink {
			if err := linker.LinkRelationEpisode(ctx, relUUID, episodeUUID); err != nil {
				partial = append(partial, fmt.Errorf("link relation %s: %w", relUUID, err))
			}
		}

		for _, priorUUID := range superseded {
			if err := e.store.InvalidateRelation(ctx, priorUUID, validAt); err != nil {
				partial = append(partial, fmt.Errorf("invalidate superseded relation %s: %w", priorUUID, err))
				continue
			}
			e.logger.Info("invalidated superseded relation",
				"relation", priorUUID, "type", rel.Type, "superseded_by", relUUID)
		}

		responseRelations = append(responseRelations, types.Relation{
			UUID:       relUUID,
			SourceUUID: srcUUID,
			TargetUUID: tgtUUID,
			Type:       rel.Type,
			Fact:       rel.Fact,
			CreatedAt:  now,
			ValidAt:    validAt,
			InvalidAt:  newInvalidAt,
		})
	}

	// Step 5: A store without linking creates the episode last, with its
	// mentions in the same call.
	if !canLink {
		episodeUUID, err = e.store.CreateEpisode(ctx, input, storedUUIDs)
		if err != nil {
			return nil, fmt.Errorf("create episode %s: %w", input.Name, err)
		}
	}

	result := &types.IngestResult{
		UUID:          episodeUUID,
		Name:          input.Name,
		EntityNodes:   responseEntities,
		EpisodicEdges: responseRelations,
	}
	if len(partial) > 0 {
		return result, fmt.Errorf("%w: %w", types.ErrPartialEpisode, errors.Join(partial...))
	}
	return result, nil
}

// extract runs the extractor, passing the ontology when the extractor
// accepts one.
func (e *GraphEngine) extract(ctx context.Context, text string, ont *types.Ontology) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	if oe, ok := e.extractor.(types.OntologyExtractor); ok && ont != nil {
		return oe.ExtractWithOntology(ctx, text, ont)
	}
	return e.extractor.Extract(ctx, text)
}

// normalizeToOntology rewrites extracted type names to the ontology's
// spelling when they match ignoring case and punctuation ("works at" and
// "WORKS_AT" both match "works_at"). With strict ontology enabled, entities
// and relations whose type matches no ontology type are dropped.
func (e *GraphEngine) normalizeToOntology(ont *types.Ontology, ents []types.ExtractedEntity, rels []types.ExtractedRelation) ([]types.ExtractedEntity, []types.ExtractedRelation) {
	if ont == nil {
		return ents, rels
	}
	entityTypes := make(map[string]string, len(ont.EntityTypes))
	for _, et := range ont.EntityTypes {
		entityTypes[fuzzy.Normalize(et.Name)] = et.Name
	}
	relationTypes := make(map[string]string, len(ont.RelationTypes))
	for _, rt := range ont.RelationTypes {
		relationTypes[fuzzy.Normalize(rt.Name)] = rt.Name
	}

	outEnts := ents[:0:0]
	for _, ent := range ents {
		if canonical, ok := entityTypes[fuzzy.Normalize(ent.Type)]; ok {
			ent.Type = canonical
		} else if e.strictOntology && len(entityTypes) > 0 {
			e.logger.Debug("dropping entity outside ontology", "entity", ent.Name, "type", ent.Type)
			continue
		}
		outEnts = append(outEnts, ent)
	}
	outRels := rels[:0:0]
	for _, rel := range rels {
		if canonical, ok := relationTypes[fuzzy.Normalize(rel.Type)]; ok {
			rel.Type = canonical
		} else if e.strictOntology && len(relationTypes) > 0 {
			e.logger.Debug("dropping relation outside ontology", "type", rel.Type)
			continue
		}
		outRels = append(outRels, rel)
	}
	return outEnts, outRels
}

// embedEntities embeds every entity in one Embed call. It returns nil
// vectors (and an error) when the embedder fails or returns the wrong count;
// the entities are then stored without embeddings.
func (e *GraphEngine) embedEntities(ctx context.Context, ents []types.ExtractedEntity) ([][]float32, error) {
	if e.embedder == nil || len(ents) == 0 {
		return nil, nil
	}
	texts := make([]string, len(ents))
	for i, ent := range ents {
		texts[i] = fmt.Sprintf("%s %s", ent.Name, ent.Summary)
	}
	vecs, err := e.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed entities: %w", err)
	}
	if len(vecs) != len(ents) {
		return nil, fmt.Errorf("embed entities: got %d embeddings for %d entities", len(vecs), len(ents))
	}
	return vecs, nil
}

// deduplicateAndUpsertEntity performs fuzzy entity deduplication then upserts.
// Dedup candidates are scoped to groupID when the store supports it, so
// entities never merge across tenant groups.
func (e *GraphEngine) deduplicateAndUpsertEntity(ctx context.Context, groupID string, ent *types.ExtractedEntity, embedding []float32) (string, error) {
	ent.GroupID = groupID

	// Try exact match first (handled by UpsertEntity's name+type check)
	existing, err := e.findEntitiesByNameType(ctx, groupID, ent.Name, ent.Type)
	if err == nil && len(existing) > 0 {
		// Exact match: upsert will update summary/embedding
		return e.store.UpsertEntity(ctx, ent, embedding)
	}

	// Try fuzzy match: find candidates with similar names
	candidates, err := e.findEntitiesByFuzzyName(ctx, groupID, ent.Name, 10)
	if err == nil {
		for _, candidate := range candidates {
			if fuzzy.IsFuzzyMatch(ent.Name, candidate.Name, FuzzyMatchThreshold) {
				// Fuzzy match found: update the existing entity with new data.
				// The candidate's name and type are kept so the upsert hits the
				// existing row instead of inserting a second node.
				e.logger.Info("fuzzy entity merge",
					"new", ent.Name, "existing", candidate.Name,
					"similarity", fuzzy.Similarity(ent.Name, candidate.Name))
				merged := &types.ExtractedEntity{
					Name:    candidate.Name,
					Type:    candidate.Type,
					Summary: ent.Summary, // use newer summary
					GroupID: groupID,
				}
				return e.store.UpsertEntity(ctx, merged, embedding)
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

// sameDirection keeps the relations that point from srcUUID to tgtUUID.
// Stores return relations in both directions; "B reports_to A" must neither
// duplicate nor supersede "A reports_to B".
func sameDirection(rels []types.Relation, srcUUID, tgtUUID string) []types.Relation {
	out := rels[:0:0]
	for _, r := range rels {
		if r.SourceUUID == srcUUID && r.TargetUUID == tgtUUID {
			out = append(out, r)
		}
	}
	return out
}

// duplicateRelation returns the UUID of an active relation whose fact text
// is similar enough to fact to count as the same assertion, or "".
func duplicateRelation(existing []types.Relation, fact string) string {
	for _, rel := range existing {
		if rel.InvalidAt != nil {
			continue // skip invalidated relations
		}
		if fuzzy.Similarity(rel.Fact, fact) >= EdgeDedupTextSimilarityThreshold {
			return rel.UUID
		}
	}
	return ""
}

// supersession applies the rule that a newer relation of the same
// (source, target, type) supersedes an older active one. It returns the
// active priors that became valid no later than validAt, which the new
// relation invalidates, and, when some active prior is newer than validAt,
// the earliest such ValidAt: the new relation is already superseded then.
// No LLM judge is involved.
func supersession(existing []types.Relation, relType string, validAt time.Time) (superseded []string, newInvalidAt *time.Time) {
	for _, prior := range existing {
		if prior.Type != relType || prior.InvalidAt != nil {
			continue
		}
		if !prior.ValidAt.After(validAt) {
			superseded = append(superseded, prior.UUID)
			continue
		}
		if newInvalidAt == nil || prior.ValidAt.Before(*newInvalidAt) {
			t := prior.ValidAt
			newInvalidAt = &t
		}
	}
	return superseded, newInvalidAt
}

// GetEntity retrieves an entity by UUID.
func (e *GraphEngine) GetEntity(ctx context.Context, id string) (*types.Entity, error) {
	return e.store.GetEntity(ctx, id)
}

// SupportsDocumentDeletion reports whether DeleteDocumentEpisodes can work:
// true only when the configured store implements types.DocumentEpisodeDeleter.
// The method set of GraphEngine always includes DeleteDocumentEpisodes, so
// callers that pick a storage layout by document deletion support must ask
// here instead of relying on a type assertion.
func (e *GraphEngine) SupportsDocumentDeletion() bool {
	_, ok := e.store.(types.DocumentEpisodeDeleter)
	return ok
}

// DeleteDocumentEpisodes implements types.DocumentEpisodeDeleter by
// delegating to the store. It errors when the configured store cannot delete
// by document (see SupportsDocumentDeletion).
func (e *GraphEngine) DeleteDocumentEpisodes(ctx context.Context, groupID, documentID string) error {
	if e.store == nil {
		return types.ErrStoreNotReady
	}
	dd, ok := e.store.(types.DocumentEpisodeDeleter)
	if !ok {
		return fmt.Errorf("store %T does not support document episode deletion", e.store)
	}
	return dd.DeleteDocumentEpisodes(ctx, groupID, documentID)
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

// SearchFacts combines vector and full-text search using Reciprocal Rank Fusion.
// If every attempted backend fails the error is returned. If only some fail,
// the surviving results are returned together with an error wrapping
// types.ErrPartialSearch so callers can detect degraded results.
func (e *GraphEngine) SearchFacts(ctx context.Context, query string, opts ...types.SearchOption) (*types.SearchFactsResult, error) {
	o := &types.SearchOptions{}
	for _, opt := range opts {
		opt(o)
	}

	if o.Limit <= 0 {
		o.Limit = DefaultSearchLimit
	}
	limit := o.Limit

	// Run vector search and full-text search
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

	// Full-text search
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
	// Ties break on UUID so equal scores rank the same way on every call.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].uuid < ranked[j].uuid
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

// GetFactProvenance returns the episodes that sourced a given fact, oldest
// first. Stores that implement types.EpisodeLinker return the episodes that
// asserted the relation; other stores may approximate it from entity
// mentions.
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
