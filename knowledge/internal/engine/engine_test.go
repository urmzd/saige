package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/knowledge/types"
)

// --- Mocks ---

type mockStore struct {
	entities          map[string]types.Entity
	relations         []types.Relation
	upsertEntityFunc  func(ctx context.Context, entity *types.ExtractedEntity, embedding []float32) (string, error)
	findByNameType    func(ctx context.Context, name, entityType string) ([]types.Entity, error)
	findByFuzzyName   func(ctx context.Context, name string, limit int) ([]types.Entity, error)
	createRelationFn  func(ctx context.Context, rel *types.RelationInput) (string, error)
	invalidateRelFn   func(ctx context.Context, uuid string, invalidAt time.Time) error
	invalidated       []invalidationCall
	findRelsBetween   func(ctx context.Context, src, tgt string) ([]types.Relation, error)
	createEpisodeFn   func(ctx context.Context, input *types.EpisodeInput, uuids []string) (string, error)
	searchEmbeddingFn func(ctx context.Context, emb []float32, opts *types.SearchOptions) ([]types.ScoredFact, error)
	searchTextFn      func(ctx context.Context, query string, opts *types.SearchOptions) ([]types.ScoredFact, error)
	getGraphFn        func(ctx context.Context, limit int64) (*types.GraphData, error)
	getNodeFn         func(ctx context.Context, id string, depth int) (*types.NodeDetail, error)
	getProvenanceFn   func(ctx context.Context, factUUID string) ([]types.Episode, error)
	closed            bool
}

// invalidationCall records an InvalidateRelation invocation for assertions.
type invalidationCall struct {
	uuid      string
	invalidAt time.Time
}

func newMockStore() *mockStore {
	return &mockStore{entities: make(map[string]types.Entity)}
}

func (m *mockStore) UpsertEntity(ctx context.Context, entity *types.ExtractedEntity, embedding []float32) (string, error) {
	if m.upsertEntityFunc != nil {
		return m.upsertEntityFunc(ctx, entity, embedding)
	}
	uuid := "entity-" + entity.Name
	m.entities[uuid] = types.Entity{UUID: uuid, Name: entity.Name, Type: entity.Type, Summary: entity.Summary}
	return uuid, nil
}

func (m *mockStore) GetEntity(_ context.Context, uuid string) (*types.Entity, error) {
	e, ok := m.entities[uuid]
	if !ok {
		return nil, types.ErrNodeNotFound
	}
	return &e, nil
}

func (m *mockStore) FindEntitiesByNameType(ctx context.Context, name, entityType string) ([]types.Entity, error) {
	if m.findByNameType != nil {
		return m.findByNameType(ctx, name, entityType)
	}
	return nil, nil
}

func (m *mockStore) FindEntitiesByFuzzyName(ctx context.Context, name string, limit int) ([]types.Entity, error) {
	if m.findByFuzzyName != nil {
		return m.findByFuzzyName(ctx, name, limit)
	}
	return nil, nil
}

func (m *mockStore) CreateRelation(ctx context.Context, rel *types.RelationInput) (string, error) {
	if m.createRelationFn != nil {
		return m.createRelationFn(ctx, rel)
	}
	uuid := "rel-" + rel.SourceUUID + "-" + rel.TargetUUID
	m.relations = append(m.relations, types.Relation{
		UUID: uuid, SourceUUID: rel.SourceUUID, TargetUUID: rel.TargetUUID,
		Type: rel.Type, Fact: rel.Fact, ValidAt: rel.ValidAt,
	})
	return uuid, nil
}

func (m *mockStore) InvalidateRelation(ctx context.Context, uuid string, invalidAt time.Time) error {
	m.invalidated = append(m.invalidated, invalidationCall{uuid: uuid, invalidAt: invalidAt})
	if m.invalidateRelFn != nil {
		return m.invalidateRelFn(ctx, uuid, invalidAt)
	}
	return nil
}

func (m *mockStore) FindRelationsBetweenEntities(ctx context.Context, src, tgt string) ([]types.Relation, error) {
	if m.findRelsBetween != nil {
		return m.findRelsBetween(ctx, src, tgt)
	}
	return nil, nil
}

func (m *mockStore) CreateEpisode(ctx context.Context, input *types.EpisodeInput, uuids []string) (string, error) {
	if m.createEpisodeFn != nil {
		return m.createEpisodeFn(ctx, input, uuids)
	}
	return "episode-1", nil
}

func (m *mockStore) SearchByEmbedding(ctx context.Context, emb []float32, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if m.searchEmbeddingFn != nil {
		return m.searchEmbeddingFn(ctx, emb, opts)
	}
	return nil, nil
}

func (m *mockStore) SearchByText(ctx context.Context, query string, opts *types.SearchOptions) ([]types.ScoredFact, error) {
	if m.searchTextFn != nil {
		return m.searchTextFn(ctx, query, opts)
	}
	return nil, nil
}

func (m *mockStore) GetGraph(ctx context.Context, limit int64) (*types.GraphData, error) {
	if m.getGraphFn != nil {
		return m.getGraphFn(ctx, limit)
	}
	return &types.GraphData{}, nil
}

func (m *mockStore) GetNode(ctx context.Context, id string, depth int) (*types.NodeDetail, error) {
	if m.getNodeFn != nil {
		return m.getNodeFn(ctx, id, depth)
	}
	return nil, types.ErrNodeNotFound
}

func (m *mockStore) GetFactProvenance(ctx context.Context, factUUID string) ([]types.Episode, error) {
	if m.getProvenanceFn != nil {
		return m.getProvenanceFn(ctx, factUUID)
	}
	return nil, nil
}

func (m *mockStore) Close(_ context.Context) error {
	m.closed = true
	return nil
}

// groupScopedMockStore extends mockStore with types.GroupScopedStore, keeping
// per-group entity maps so cross-group dedup leakage is observable.
type groupScopedMockStore struct {
	*mockStore
	byGroup map[string]map[string]types.Entity // groupID -> name|type -> entity
	upserts []upsertCall
}

// upsertCall records the group and name an UpsertEntity call carried.
type upsertCall struct {
	groupID string
	name    string
}

func newGroupScopedMockStore() *groupScopedMockStore {
	return &groupScopedMockStore{
		mockStore: newMockStore(),
		byGroup:   make(map[string]map[string]types.Entity),
	}
}

func (m *groupScopedMockStore) UpsertEntity(_ context.Context, entity *types.ExtractedEntity, _ []float32) (string, error) {
	m.upserts = append(m.upserts, upsertCall{groupID: entity.GroupID, name: entity.Name})
	key := entity.Name + "|" + entity.Type
	group, ok := m.byGroup[entity.GroupID]
	if !ok {
		group = make(map[string]types.Entity)
		m.byGroup[entity.GroupID] = group
	}
	if existing, ok := group[key]; ok {
		return existing.UUID, nil
	}
	uuid := "entity-" + entity.GroupID + "-" + entity.Name
	group[key] = types.Entity{UUID: uuid, Name: entity.Name, Type: entity.Type, Summary: entity.Summary}
	return uuid, nil
}

func (m *groupScopedMockStore) FindEntitiesByNameTypeInGroup(_ context.Context, groupID, name, entityType string) ([]types.Entity, error) {
	if e, ok := m.byGroup[groupID][name+"|"+entityType]; ok {
		return []types.Entity{e}, nil
	}
	return nil, nil
}

func (m *groupScopedMockStore) FindEntitiesByFuzzyNameInGroup(_ context.Context, groupID, _ string, _ int) ([]types.Entity, error) {
	var out []types.Entity
	for _, e := range m.byGroup[groupID] {
		out = append(out, e)
	}
	return out, nil
}

type mockExtractor struct {
	entities  []types.ExtractedEntity
	relations []types.ExtractedRelation
	err       error
}

func (m *mockExtractor) Extract(_ context.Context, _ string) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	return m.entities, m.relations, m.err
}

type mockEmbedder struct {
	err error
}

func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if m.err != nil {
		return nil, m.err
	}
	result := make([][]float32, len(texts))
	for i := range texts {
		result[i] = []float32{0.1, 0.2, 0.3}
	}
	return result, nil
}

// --- Tests ---

func TestNew(t *testing.T) {
	store := newMockStore()
	eng := New(WithStore(store))

	if eng.store != store {
		t.Error("store not set")
	}
}

func TestApplyOntology(t *testing.T) {
	eng := New()
	ont := &types.Ontology{
		EntityTypes: []types.EntityTypeDef{{Name: "Person"}},
	}

	err := eng.ApplyOntology(context.Background(), ont)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eng.ontology != ont {
		t.Error("ontology not stored")
	}
}

func TestIngestEpisode_NoExtractor(t *testing.T) {
	eng := New(WithStore(newMockStore()))

	_, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})
	if !errors.Is(err, types.ErrNoExtractor) {
		t.Errorf("error = %v, want ErrNoExtractor", err)
	}
}

func TestIngestEpisode_NoStore(t *testing.T) {
	eng := New(WithExtractor(&mockExtractor{}))

	_, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})
	if !errors.Is(err, types.ErrStoreNotReady) {
		t.Errorf("error = %v, want ErrStoreNotReady", err)
	}
}

func TestIngestEpisode_Success(t *testing.T) {
	store := newMockStore()
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}
	emb := &mockEmbedder{}

	eng := New(WithStore(store), WithExtractor(ext), WithEmbedder(emb))

	result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{
		Name: "test-episode", Body: "Alice works at Acme",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.UUID != "episode-1" {
		t.Errorf("UUID = %q, want %q", result.UUID, "episode-1")
	}
	if len(result.EntityNodes) != 2 {
		t.Errorf("entities = %d, want 2", len(result.EntityNodes))
	}
	if len(result.EpisodicEdges) != 1 {
		t.Errorf("relations = %d, want 1", len(result.EpisodicEdges))
	}
}

func TestIngestEpisode_ExtractError(t *testing.T) {
	eng := New(
		WithStore(newMockStore()),
		WithExtractor(&mockExtractor{err: errors.New("LLM error")}),
	)

	_, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIngestEpisode_RelationSkippedWhenEntityMissing(t *testing.T) {
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
		},
		relations: []types.ExtractedRelation{
			// Bob was not extracted as an entity
			{Source: "Alice", Target: "Bob", Type: "knows", Fact: "Alice knows Bob"},
		},
	}

	eng := New(WithStore(newMockStore()), WithExtractor(ext))
	result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.EpisodicEdges) != 0 {
		t.Errorf("relations = %d, want 0 (target entity missing)", len(result.EpisodicEdges))
	}
}

func TestIngestEpisode_DuplicateRelationSkipped(t *testing.T) {
	store := newMockStore()
	store.findRelsBetween = func(_ context.Context, _, _ string) ([]types.Relation, error) {
		return []types.Relation{
			{UUID: "existing", Fact: "Alice works at Acme", ValidAt: time.Now()},
		}, nil
	}

	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}

	eng := New(WithStore(store), WithExtractor(ext))
	result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.EpisodicEdges) != 0 {
		t.Errorf("relations = %d, want 0 (duplicate skipped)", len(result.EpisodicEdges))
	}
}

func TestIngestEpisode_SupersededRelationInvalidated(t *testing.T) {
	store := newMockStore()
	prior := types.Relation{
		UUID:       "prior-rel",
		SourceUUID: "entity-Alice",
		TargetUUID: "entity-Acme",
		Type:       "works_at",
		Fact:       "Alice works at Acme as an intern",
		ValidAt:    time.Now().Add(-24 * time.Hour),
	}
	store.findRelsBetween = func(_ context.Context, _, _ string) ([]types.Relation, error) {
		return []types.Relation{prior}, nil
	}

	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			// Same source+target+type but a contradicting fact (not a duplicate).
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme as the CEO"},
		},
	}

	eng := New(WithStore(store), WithExtractor(ext))
	result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The new (contradicting) relation should have been created.
	if len(result.EpisodicEdges) != 1 {
		t.Fatalf("relations created = %d, want 1", len(result.EpisodicEdges))
	}
	newUUID := result.EpisodicEdges[0].UUID

	// The prior relation should have been invalidated exactly once.
	if len(store.invalidated) != 1 {
		t.Fatalf("invalidations = %d, want 1", len(store.invalidated))
	}
	if store.invalidated[0].uuid != "prior-rel" {
		t.Errorf("invalidated uuid = %q, want prior-rel", store.invalidated[0].uuid)
	}
	// The new relation must not have been invalidated.
	if store.invalidated[0].uuid == newUUID {
		t.Errorf("the newly created relation was invalidated")
	}
	// InvalidAt should equal the new relation's ValidAt.
	if !store.invalidated[0].invalidAt.Equal(result.EpisodicEdges[0].ValidAt) {
		t.Errorf("invalidAt = %v, want new relation ValidAt %v",
			store.invalidated[0].invalidAt, result.EpisodicEdges[0].ValidAt)
	}
}

func TestIngestEpisode_DifferentTypePriorNotInvalidated(t *testing.T) {
	store := newMockStore()
	store.findRelsBetween = func(_ context.Context, _, _ string) ([]types.Relation, error) {
		return []types.Relation{{
			UUID:       "other-type-rel",
			SourceUUID: "entity-Alice",
			TargetUUID: "entity-Acme",
			Type:       "founded", // different type
			Fact:       "Alice founded Acme",
			ValidAt:    time.Now().Add(-24 * time.Hour),
		}}, nil
	}

	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}

	eng := New(WithStore(store), WithExtractor(ext))
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.invalidated) != 0 {
		t.Errorf("invalidations = %d, want 0 (different type must not supersede)", len(store.invalidated))
	}
}

func TestIngestEpisode_AlreadyInvalidatedPriorSkipped(t *testing.T) {
	store := newMockStore()
	priorInvalidAt := time.Now().Add(-1 * time.Hour)
	store.findRelsBetween = func(_ context.Context, _, _ string) ([]types.Relation, error) {
		return []types.Relation{{
			UUID:       "already-dead",
			SourceUUID: "entity-Alice",
			TargetUUID: "entity-Acme",
			Type:       "works_at",
			Fact:       "Alice used to work at Acme long ago",
			ValidAt:    time.Now().Add(-48 * time.Hour),
			InvalidAt:  &priorInvalidAt, // already invalidated
		}}, nil
	}

	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme today"},
		},
	}

	eng := New(WithStore(store), WithExtractor(ext))
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "test"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(store.invalidated) != 0 {
		t.Errorf("invalidations = %d, want 0 (already-invalidated prior must be skipped)", len(store.invalidated))
	}
}

func TestIngestEpisode_GroupScopedDedup_SeparateEntitiesPerGroup(t *testing.T) {
	store := newGroupScopedMockStore()
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{{Name: "Alice", Type: "Person", Summary: "A person"}},
	}
	eng := New(WithStore(store), WithExtractor(ext))

	resA, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resB, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(resA.EntityNodes) != 1 || len(resB.EntityNodes) != 1 {
		t.Fatalf("entities = %d/%d, want 1/1", len(resA.EntityNodes), len(resB.EntityNodes))
	}
	if resA.EntityNodes[0].UUID == resB.EntityNodes[0].UUID {
		t.Errorf("same entity UUID %q across groups, want separate entities per group", resA.EntityNodes[0].UUID)
	}
	if len(store.upserts) != 2 {
		t.Fatalf("upserts = %d, want 2", len(store.upserts))
	}
	if store.upserts[0].groupID != "tenant-a" || store.upserts[1].groupID != "tenant-b" {
		t.Errorf("upsert groups = %q/%q, want tenant-a/tenant-b",
			store.upserts[0].groupID, store.upserts[1].groupID)
	}
}

func TestIngestEpisode_GroupScopedDedup_SameGroupMerges(t *testing.T) {
	store := newGroupScopedMockStore()
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{{Name: "Alice", Type: "Person", Summary: "A person"}},
	}
	eng := New(WithStore(store), WithExtractor(ext))

	res1, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	res2, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res1.EntityNodes[0].UUID != res2.EntityNodes[0].UUID {
		t.Errorf("UUIDs %q vs %q, want same entity within one group",
			res1.EntityNodes[0].UUID, res2.EntityNodes[0].UUID)
	}
}

func TestIngestEpisode_GroupScopedDedup_FuzzyNeverMatchesAcrossGroups(t *testing.T) {
	store := newGroupScopedMockStore()
	eng := New(WithStore(store), WithExtractor(&mockExtractor{
		entities: []types.ExtractedEntity{{Name: "Alice Smith", Type: "Person", Summary: "A person"}},
	}))
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "alice smith" would fuzzy-merge with "Alice Smith" inside tenant-a, but
	// tenant-b must get its own entity.
	eng2 := New(WithStore(store), WithExtractor(&mockExtractor{
		entities: []types.ExtractedEntity{{Name: "alice smith", Type: "Person", Summary: "A person"}},
	}))
	res, err := eng2.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := res.EntityNodes[0].UUID; got == "entity-tenant-a-Alice Smith" {
		t.Errorf("entity merged across groups into %q", got)
	}
	if len(store.byGroup["tenant-b"]) != 1 {
		t.Errorf("tenant-b entities = %d, want 1", len(store.byGroup["tenant-b"]))
	}
}

func TestIngestEpisode_LegacyStoreKeepsGlobalDedup(t *testing.T) {
	// A store without GroupScopedStore must keep the old global lookups.
	store := newMockStore()
	var lookedUp bool
	store.findByNameType = func(_ context.Context, name, entityType string) ([]types.Entity, error) {
		lookedUp = true
		return nil, nil
	}

	eng := New(WithStore(store), WithExtractor(&mockExtractor{
		entities: []types.ExtractedEntity{{Name: "Alice", Type: "Person"}},
	}))
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !lookedUp {
		t.Error("legacy FindEntitiesByNameType was not used as fallback")
	}
}

func TestIngestEpisode_RelationCarriesGroupID(t *testing.T) {
	store := newMockStore()
	var gotGroup string
	store.createRelationFn = func(_ context.Context, rel *types.RelationInput) (string, error) {
		gotGroup = rel.GroupID
		return "rel-1", nil
	}

	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person"},
			{Name: "Acme", Type: "Organization"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}

	eng := New(WithStore(store), WithExtractor(ext))
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "tenant-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotGroup != "tenant-a" {
		t.Errorf("relation GroupID = %q, want tenant-a", gotGroup)
	}
}

func TestIngestEpisode_MetadataReachesStore(t *testing.T) {
	store := newMockStore()
	var gotMeta map[string]string
	store.createEpisodeFn = func(_ context.Context, input *types.EpisodeInput, _ []string) (string, error) {
		gotMeta = input.Metadata
		return "episode-1", nil
	}

	eng := New(WithStore(store), WithExtractor(&mockExtractor{}))
	meta := map[string]string{"channel": "slack"}
	if _, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", Metadata: meta}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMeta["channel"] != "slack" {
		t.Errorf("metadata = %v, want channel=slack", gotMeta)
	}
}

func TestSearchFacts_BM25Only(t *testing.T) {
	store := newMockStore()
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1", FactText: "Alice works at Acme"}, Score: 1.0},
		}, nil
	}

	eng := New(WithStore(store))
	result, err := eng.SearchFacts(context.Background(), "Alice")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Facts) != 1 {
		t.Errorf("facts = %d, want 1", len(result.Facts))
	}
}

func TestSearchFacts_HybridRRF(t *testing.T) {
	store := newMockStore()
	store.searchEmbeddingFn = func(_ context.Context, _ []float32, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1", FactText: "vector result"}, Score: 0.9},
			{Fact: types.Fact{UUID: "f2", FactText: "vector only"}, Score: 0.8},
		}, nil
	}
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1", FactText: "vector result"}, Score: 1.0},
			{Fact: types.Fact{UUID: "f3", FactText: "text only"}, Score: 0.7},
		}, nil
	}

	eng := New(WithStore(store), WithEmbedder(&mockEmbedder{}))
	result, err := eng.SearchFacts(context.Background(), "test")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Facts) != 3 {
		t.Errorf("facts = %d, want 3", len(result.Facts))
	}
	// f1 should rank first (appears in both lists)
	if result.Facts[0].UUID != "f1" {
		t.Errorf("first fact = %q, want f1 (highest RRF score)", result.Facts[0].UUID)
	}
}

func TestSearchFacts_WithLimit(t *testing.T) {
	store := newMockStore()
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1"}, Score: 1.0},
			{Fact: types.Fact{UUID: "f2"}, Score: 0.9},
			{Fact: types.Fact{UUID: "f3"}, Score: 0.8},
		}, nil
	}

	eng := New(WithStore(store))
	result, err := eng.SearchFacts(context.Background(), "test", types.WithLimit(2))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Facts) != 2 {
		t.Errorf("facts = %d, want 2 (limited)", len(result.Facts))
	}
}

func TestSearchFacts_SingleBackendErrorPropagated(t *testing.T) {
	// No embedder: text search is the only backend, so its failure must
	// surface instead of an empty success.
	store := newMockStore()
	storeErr := errors.New("connection refused")
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return nil, storeErr
	}

	eng := New(WithStore(store))
	result, err := eng.SearchFacts(context.Background(), "test")

	if err == nil {
		t.Fatal("expected error, got empty success")
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("error = %v, want wrapped %v", err, storeErr)
	}
	if errors.Is(err, types.ErrPartialSearch) {
		t.Error("total failure must not be reported as partial")
	}
	if result != nil {
		t.Errorf("result = %v, want nil", result)
	}
}

func TestSearchFacts_AllBackendsFailReturnsError(t *testing.T) {
	store := newMockStore()
	store.searchEmbeddingFn = func(_ context.Context, _ []float32, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return nil, errors.New("vector down")
	}
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return nil, errors.New("text down")
	}

	eng := New(WithStore(store), WithEmbedder(&mockEmbedder{}))
	result, err := eng.SearchFacts(context.Background(), "test")

	if err == nil {
		t.Fatal("expected error, got empty success")
	}
	if errors.Is(err, types.ErrPartialSearch) {
		t.Error("total failure must not be reported as partial")
	}
	if result != nil {
		t.Errorf("result = %v, want nil", result)
	}
}

func TestSearchFacts_PartialFailureReturnsResultsAndSentinel(t *testing.T) {
	store := newMockStore()
	store.searchEmbeddingFn = func(_ context.Context, _ []float32, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return nil, errors.New("vector down")
	}
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1", FactText: "Alice works at Acme"}, Score: 1.0},
		}, nil
	}

	eng := New(WithStore(store), WithEmbedder(&mockEmbedder{}))
	result, err := eng.SearchFacts(context.Background(), "test")

	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("error = %v, want ErrPartialSearch", err)
	}
	if result == nil || len(result.Facts) != 1 {
		t.Fatalf("result = %v, want 1 fact alongside the partial error", result)
	}
}

func TestSearchFacts_EmbedFailureIsPartial(t *testing.T) {
	store := newMockStore()
	store.searchTextFn = func(_ context.Context, _ string, _ *types.SearchOptions) ([]types.ScoredFact, error) {
		return []types.ScoredFact{
			{Fact: types.Fact{UUID: "f1"}, Score: 1.0},
		}, nil
	}

	eng := New(WithStore(store), WithEmbedder(&mockEmbedder{err: errors.New("embedder down")}))
	result, err := eng.SearchFacts(context.Background(), "test")

	if !errors.Is(err, types.ErrPartialSearch) {
		t.Fatalf("error = %v, want ErrPartialSearch", err)
	}
	if result == nil || len(result.Facts) != 1 {
		t.Fatalf("result = %v, want 1 fact", result)
	}
}

func TestGetEntity(t *testing.T) {
	store := newMockStore()
	store.entities["abc"] = types.Entity{UUID: "abc", Name: "Alice"}

	eng := New(WithStore(store))
	ent, err := eng.GetEntity(context.Background(), "abc")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ent.Name != "Alice" {
		t.Errorf("name = %q, want Alice", ent.Name)
	}
}

func TestGetEntity_NotFound(t *testing.T) {
	eng := New(WithStore(newMockStore()))
	_, err := eng.GetEntity(context.Background(), "nonexistent")

	if !errors.Is(err, types.ErrNodeNotFound) {
		t.Errorf("error = %v, want ErrNodeNotFound", err)
	}
}

func TestClose(t *testing.T) {
	store := newMockStore()
	eng := New(WithStore(store))

	err := eng.Close(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.closed {
		t.Error("store was not closed")
	}
}

func TestClose_NilStore(t *testing.T) {
	eng := New()
	err := eng.Close(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReciprocalRankFusion(t *testing.T) {
	listA := []types.ScoredFact{
		{Fact: types.Fact{UUID: "a", FactText: "fact a"}, Score: 1.0},
		{Fact: types.Fact{UUID: "b", FactText: "fact b"}, Score: 0.5},
	}
	listB := []types.ScoredFact{
		{Fact: types.Fact{UUID: "b", FactText: "fact b"}, Score: 1.0},
		{Fact: types.Fact{UUID: "c", FactText: "fact c"}, Score: 0.5},
	}

	facts := reciprocalRankFusion(listA, listB, 10)

	if len(facts) != 3 {
		t.Fatalf("facts = %d, want 3", len(facts))
	}
	// "b" appears in both lists at rank 1 and 0, so it should have highest RRF score
	if facts[0].UUID != "b" {
		t.Errorf("first = %q, want b (appears in both lists)", facts[0].UUID)
	}
}

func TestReciprocalRankFusion_Empty(t *testing.T) {
	facts := reciprocalRankFusion(nil, nil, 10)
	if len(facts) != 0 {
		t.Errorf("facts = %d, want 0", len(facts))
	}
}

func TestReciprocalRankFusion_LimitApplied(t *testing.T) {
	list := []types.ScoredFact{
		{Fact: types.Fact{UUID: "a"}, Score: 1.0},
		{Fact: types.Fact{UUID: "b"}, Score: 0.9},
		{Fact: types.Fact{UUID: "c"}, Score: 0.8},
	}

	facts := reciprocalRankFusion(list, nil, 2)
	if len(facts) != 2 {
		t.Errorf("facts = %d, want 2", len(facts))
	}
}
