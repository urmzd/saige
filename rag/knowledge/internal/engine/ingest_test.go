package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/knowledge/types"
)

var (
	_ types.DocumentEpisodeDeleter = (*GraphEngine)(nil)
)

// countingEmbedder records every Embed call.
type countingEmbedder struct {
	mu    sync.Mutex
	calls [][]string
	err   error
	short bool // return one vector fewer than requested
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.mu.Lock()
	c.calls = append(c.calls, slices.Clone(texts))
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	n := len(texts)
	if c.short {
		n--
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = []float32{float32(i)}
	}
	return out, nil
}

// ontologyExtractor implements types.OntologyExtractor and records the
// ontology it was given.
type ontologyExtractor struct {
	mu        sync.Mutex
	got       []*types.Ontology
	plain     int
	entities  []types.ExtractedEntity
	relations []types.ExtractedRelation
}

func (o *ontologyExtractor) Extract(context.Context, string) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	o.mu.Lock()
	o.plain++
	o.mu.Unlock()
	return slices.Clone(o.entities), slices.Clone(o.relations), nil
}

func (o *ontologyExtractor) ExtractWithOntology(_ context.Context, _ string, ont *types.Ontology) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	o.mu.Lock()
	o.got = append(o.got, ont)
	o.mu.Unlock()
	return slices.Clone(o.entities), slices.Clone(o.relations), nil
}

// linkingMockStore extends mockStore with types.EpisodeLinker and records
// the order of store calls.
type linkingMockStore struct {
	*mockStore
	mu            sync.Mutex
	calls         []string
	entityLinks   map[string][]string // episode -> entities
	relationLinks map[string][]string // relation -> episodes
	linkErr       error
	episodeInputs []*types.EpisodeInput
	episodeUUIDs  [][]string
}

func newLinkingMockStore() *linkingMockStore {
	return &linkingMockStore{
		mockStore:     newMockStore(),
		entityLinks:   make(map[string][]string),
		relationLinks: make(map[string][]string),
	}
}

func (m *linkingMockStore) record(call string) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
}

func (m *linkingMockStore) CreateEpisode(ctx context.Context, input *types.EpisodeInput, uuids []string) (string, error) {
	m.record("CreateEpisode")
	m.episodeInputs = append(m.episodeInputs, input)
	m.episodeUUIDs = append(m.episodeUUIDs, uuids)
	return m.mockStore.CreateEpisode(ctx, input, uuids)
}

func (m *linkingMockStore) UpsertEntity(ctx context.Context, entity *types.ExtractedEntity, emb []float32) (string, error) {
	m.record("UpsertEntity")
	return m.mockStore.UpsertEntity(ctx, entity, emb)
}

func (m *linkingMockStore) LinkEpisodeEntities(_ context.Context, episodeUUID string, entityUUIDs []string) error {
	m.record("LinkEpisodeEntities")
	if m.linkErr != nil {
		return m.linkErr
	}
	m.entityLinks[episodeUUID] = append(m.entityLinks[episodeUUID], entityUUIDs...)
	return nil
}

func (m *linkingMockStore) LinkRelationEpisode(_ context.Context, relationUUID, episodeUUID string) error {
	m.record("LinkRelationEpisode")
	if m.linkErr != nil {
		return m.linkErr
	}
	m.relationLinks[relationUUID] = append(m.relationLinks[relationUUID], episodeUUID)
	return nil
}

func aliceAcme() *mockExtractor {
	return &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person", Summary: "A person"},
			{Name: "Acme", Type: "Organization", Summary: "A company"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
		},
	}
}

func TestIngestEpisode_CreateEpisodeFailureIsHardError(t *testing.T) {
	tests := []struct {
		name  string
		store func() types.Store
	}{
		{"legacy store", func() types.Store {
			s := newMockStore()
			s.createEpisodeFn = func(context.Context, *types.EpisodeInput, []string) (string, error) {
				return "", errors.New("db down")
			}
			return s
		}},
		{"linking store", func() types.Store {
			s := newLinkingMockStore()
			s.createEpisodeFn = func(context.Context, *types.EpisodeInput, []string) (string, error) {
				return "", errors.New("db down")
			}
			return s
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := New(WithStore(tc.store()), WithExtractor(aliceAcme()))
			result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Name: "ep", Body: "x"})
			if err == nil {
				t.Fatal("expected error when the episode cannot be stored")
			}
			if errors.Is(err, types.ErrPartialEpisode) {
				t.Errorf("episode failure must not be reported as partial: %v", err)
			}
			if result != nil {
				t.Errorf("result = %+v, want nil", result)
			}
		})
	}
}

func TestIngestEpisode_PartialFailures(t *testing.T) {
	five := []types.ExtractedEntity{
		{Name: "A", Type: "T"}, {Name: "B", Type: "T"}, {Name: "C", Type: "T"},
		{Name: "D", Type: "T"}, {Name: "E", Type: "T"},
	}
	tests := []struct {
		name         string
		setup        func(*mockStore)
		embedder     *countingEmbedder
		relations    []types.ExtractedRelation
		wantEntities int
		wantPartial  bool
	}{
		{
			name: "one entity upsert fails",
			setup: func(s *mockStore) {
				s.upsertEntityFunc = func(_ context.Context, e *types.ExtractedEntity, _ []float32) (string, error) {
					if e.Name == "C" {
						return "", errors.New("constraint violation")
					}
					return "entity-" + e.Name, nil
				}
			},
			wantEntities: 4,
			wantPartial:  true,
		},
		{
			name:         "embedder fails",
			embedder:     &countingEmbedder{err: errors.New("embed down")},
			wantEntities: 5,
			wantPartial:  true,
		},
		{
			name:         "embedder returns wrong count",
			embedder:     &countingEmbedder{short: true},
			wantEntities: 5,
			wantPartial:  true,
		},
		{
			name: "relation create fails",
			setup: func(s *mockStore) {
				s.createRelationFn = func(context.Context, *types.RelationInput) (string, error) {
					return "", errors.New("fk violation")
				}
			},
			relations:    []types.ExtractedRelation{{Source: "A", Target: "B", Type: "r", Fact: "A r B"}},
			wantEntities: 5,
			wantPartial:  true,
		},
		{
			name: "relation lookup fails",
			setup: func(s *mockStore) {
				s.findRelsBetween = func(context.Context, string, string) ([]types.Relation, error) {
					return nil, errors.New("timeout")
				}
			},
			relations:    []types.ExtractedRelation{{Source: "A", Target: "B", Type: "r", Fact: "A r B"}},
			wantEntities: 5,
			wantPartial:  true,
		},
		{
			name:         "all succeed",
			embedder:     &countingEmbedder{},
			wantEntities: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			if tc.setup != nil {
				tc.setup(store)
			}
			opts := []Option{WithStore(store), WithExtractor(&mockExtractor{entities: five, relations: tc.relations})}
			if tc.embedder != nil {
				opts = append(opts, WithEmbedder(tc.embedder))
			}
			result, err := New(opts...).IngestEpisode(context.Background(), &types.EpisodeInput{Name: "ep", Body: "x"})
			if tc.wantPartial {
				if !errors.Is(err, types.ErrPartialEpisode) {
					t.Fatalf("err = %v, want ErrPartialEpisode", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("result must be returned with a partial error")
			}
			if len(result.EntityNodes) != tc.wantEntities {
				t.Errorf("entities = %d, want %d", len(result.EntityNodes), tc.wantEntities)
			}
			if result.UUID == "" {
				t.Error("episode UUID missing")
			}
		})
	}
}

func TestIngestEpisode_EmbedsEntitiesInOneCall(t *testing.T) {
	emb := &countingEmbedder{}
	var withVec int
	store := newMockStore()
	store.upsertEntityFunc = func(_ context.Context, e *types.ExtractedEntity, v []float32) (string, error) {
		if v != nil {
			withVec++
		}
		return "entity-" + e.Name, nil
	}
	ext := &mockExtractor{entities: []types.ExtractedEntity{
		{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D"}, {Name: "E"},
	}}
	if _, err := New(WithStore(store), WithExtractor(ext), WithEmbedder(emb)).
		IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emb.calls) != 1 || len(emb.calls[0]) != 5 {
		t.Fatalf("Embed calls = %v, want 1 call with 5 texts", emb.calls)
	}
	if withVec != 5 {
		t.Errorf("entities stored with a vector = %d, want 5", withVec)
	}
}

func TestIngestEpisode_LinkingStore(t *testing.T) {
	store := newLinkingMockStore()
	store.findRelsBetween = func(_ context.Context, src, tgt string) ([]types.Relation, error) {
		if src == "entity-Alice" && tgt == "entity-Bob" {
			return []types.Relation{{
				UUID: "existing", SourceUUID: src, TargetUUID: tgt,
				Type: "knows", Fact: "Alice knows Bob", ValidAt: time.Now().Add(-time.Hour),
			}}, nil
		}
		return nil, nil
	}
	ext := &mockExtractor{
		entities: []types.ExtractedEntity{
			{Name: "Alice", Type: "Person"}, {Name: "Acme", Type: "Organization"}, {Name: "Bob", Type: "Person"},
		},
		relations: []types.ExtractedRelation{
			{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"},
			{Source: "Alice", Target: "Bob", Type: "knows", Fact: "Alice knows Bob"}, // duplicate
		},
	}
	eng := New(WithStore(store), WithExtractor(ext))
	result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Name: "ep", Body: "x", DocumentID: "doc-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.calls[0] != "CreateEpisode" {
		t.Errorf("first store call = %s, want CreateEpisode", store.calls[0])
	}
	if n := countCalls(store.calls, "CreateEpisode"); n != 1 {
		t.Errorf("CreateEpisode calls = %d, want 1", n)
	}
	if len(store.episodeUUIDs[0]) != 0 {
		t.Errorf("CreateEpisode entity UUIDs = %v, want none (linked separately)", store.episodeUUIDs[0])
	}
	if store.episodeInputs[0].DocumentID != "doc-1" {
		t.Errorf("DocumentID = %q, want doc-1", store.episodeInputs[0].DocumentID)
	}
	if got := store.entityLinks[result.UUID]; len(got) != 3 {
		t.Errorf("entity links = %v, want 3 entities", got)
	}
	newRel := result.EpisodicEdges[0].UUID
	for _, rel := range []string{newRel, "existing"} {
		if got := store.relationLinks[rel]; !slices.Equal(got, []string{result.UUID}) {
			t.Errorf("relation %s links = %v, want [%s]", rel, got, result.UUID)
		}
	}
}

func TestIngestEpisode_LinkFailureIsPartial(t *testing.T) {
	store := newLinkingMockStore()
	store.linkErr = errors.New("link table missing")
	result, err := New(WithStore(store), WithExtractor(aliceAcme())).
		IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x"})
	if !errors.Is(err, types.ErrPartialEpisode) {
		t.Fatalf("err = %v, want ErrPartialEpisode", err)
	}
	if result == nil || len(result.EpisodicEdges) != 1 {
		t.Fatalf("result = %+v, want the relation still created", result)
	}
}

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

func TestIngestEpisode_Ontology(t *testing.T) {
	ont := &types.Ontology{
		EntityTypes:   []types.EntityTypeDef{{Name: "Person"}, {Name: "Organization"}},
		RelationTypes: []types.RelationTypeDef{{Name: "works_at"}},
	}
	extracted := []types.ExtractedEntity{
		{Name: "Alice", Type: "person"},
		{Name: "Acme", Type: "ORGANIZATION"},
		{Name: "Paris", Type: "City"},
	}
	rels := []types.ExtractedRelation{
		{Source: "Alice", Target: "Acme", Type: "Works At", Fact: "Alice works at Acme"},
		{Source: "Alice", Target: "Paris", Type: "lives_in", Fact: "Alice lives in Paris"},
	}
	tests := []struct {
		name      string
		strict    bool
		wantTypes map[string]string
		wantRels  []string
	}{
		{
			name:      "maps known types, keeps unknown",
			wantTypes: map[string]string{"Alice": "Person", "Acme": "Organization", "Paris": "City"},
			wantRels:  []string{"works_at", "lives_in"},
		},
		{
			name:      "strict drops unknown",
			strict:    true,
			wantTypes: map[string]string{"Alice": "Person", "Acme": "Organization"},
			wantRels:  []string{"works_at"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ext := &ontologyExtractor{entities: extracted, relations: rels}
			opts := []Option{WithStore(newMockStore()), WithExtractor(ext)}
			if tc.strict {
				opts = append(opts, WithStrictOntology())
			}
			eng := New(opts...)
			if err := eng.ApplyOntology(context.Background(), ont); err != nil {
				t.Fatal(err)
			}
			result, err := eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ext.got) != 1 || ext.got[0] == nil || len(ext.got[0].EntityTypes) != 2 {
				t.Fatalf("extractor ontology = %+v, want the applied ontology", ext.got)
			}
			if ext.plain != 0 {
				t.Errorf("plain Extract called %d times, want 0", ext.plain)
			}
			gotTypes := make(map[string]string)
			for _, e := range result.EntityNodes {
				gotTypes[e.Name] = e.Type
			}
			if fmt.Sprint(gotTypes) != fmt.Sprint(tc.wantTypes) {
				t.Errorf("entity types = %v, want %v", gotTypes, tc.wantTypes)
			}
			var gotRels []string
			for _, r := range result.EpisodicEdges {
				gotRels = append(gotRels, r.Type)
			}
			if !slices.Equal(gotRels, tc.wantRels) {
				t.Errorf("relation types = %v, want %v", gotRels, tc.wantRels)
			}
		})
	}
}

func TestIngestEpisode_NoOntologyUsesPlainExtract(t *testing.T) {
	ext := &ontologyExtractor{entities: []types.ExtractedEntity{{Name: "Alice", Type: "person"}}}
	result, err := New(WithStore(newMockStore()), WithExtractor(ext)).
		IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ext.plain != 1 || len(ext.got) != 0 {
		t.Errorf("plain=%d ontology=%d, want plain Extract only", ext.plain, len(ext.got))
	}
	if result.EntityNodes[0].Type != "person" {
		t.Errorf("type = %q, want unchanged %q", result.EntityNodes[0].Type, "person")
	}
}

// TestApplyOntologyConcurrentWithIngest runs under -race to prove the
// ontology is safe to replace while episodes are ingested.
func TestApplyOntologyConcurrentWithIngest(t *testing.T) {
	ext := &ontologyExtractor{entities: []types.ExtractedEntity{{Name: "Alice", Type: "person"}}}
	eng := New(WithStore(newSafeStore()), WithExtractor(ext))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = eng.ApplyOntology(context.Background(), &types.Ontology{
				EntityTypes: []types.EntityTypeDef{{Name: fmt.Sprintf("Person%d", i)}},
			})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = eng.IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x"})
		}()
	}
	wg.Wait()
}

// safeStore is a minimal concurrency-safe store for the race test.
type safeStore struct {
	*mockStore
	mu sync.Mutex
}

func newSafeStore() *safeStore { return &safeStore{mockStore: newMockStore()} }

func (s *safeStore) UpsertEntity(ctx context.Context, e *types.ExtractedEntity, v []float32) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mockStore.UpsertEntity(ctx, e, v)
}

func TestIngestEpisode_FuzzyMergeAcrossTypesReusesExistingEntity(t *testing.T) {
	store := newGroupScopedMockStore()
	existing := types.Entity{UUID: "acme-uuid", Name: "Acme Corporation", Type: "Company"}
	store.byGroup["g"] = map[string]types.Entity{"Acme Corporation|Company": existing}

	ext := &mockExtractor{entities: []types.ExtractedEntity{{Name: "Acme Corporaton", Type: "Organization", Summary: "newer"}}}
	result, err := New(WithStore(store), WithExtractor(ext)).
		IngestEpisode(context.Background(), &types.EpisodeInput{Body: "x", GroupID: "g"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result.EntityNodes[0].UUID; got != "acme-uuid" {
		t.Errorf("entity UUID = %q, want existing acme-uuid", got)
	}
	if n := len(store.byGroup["g"]); n != 1 {
		t.Errorf("entities in group = %d, want 1 (no duplicate row)", n)
	}
}

func TestIngestEpisode_Temporal(t *testing.T) {
	t2020 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	t2024 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name            string
		prior           types.Relation
		refTime         time.Time
		wantInvalidated []string
		wantNewInvalid  *time.Time
		wantValidAt     time.Time
	}{
		{
			name: "reversed edge is neither duplicate nor superseded",
			prior: types.Relation{
				UUID: "b-reports-a", SourceUUID: "entity-Bob", TargetUUID: "entity-Alice",
				Type: "reports_to", Fact: "Alice reports to Bob", ValidAt: t2020,
			},
			refTime:     t2024,
			wantValidAt: t2024,
		},
		{
			name: "older episode does not invalidate a newer fact",
			prior: types.Relation{
				UUID: "newer", SourceUUID: "entity-Alice", TargetUUID: "entity-Bob",
				Type: "reports_to", Fact: "Alice has reported to Bob since 2024", ValidAt: t2024,
			},
			refTime:        t2020,
			wantNewInvalid: &t2024,
			wantValidAt:    t2020,
		},
		{
			name: "newer episode invalidates an older fact at its reference time",
			prior: types.Relation{
				UUID: "older", SourceUUID: "entity-Alice", TargetUUID: "entity-Bob",
				Type: "reports_to", Fact: "Alice has reported to Bob since 2020", ValidAt: t2020,
			},
			refTime:         t2024,
			wantInvalidated: []string{"older"},
			wantValidAt:     t2024,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			store.findRelsBetween = func(context.Context, string, string) ([]types.Relation, error) {
				return []types.Relation{tc.prior}, nil
			}
			var created *types.RelationInput
			store.createRelationFn = func(_ context.Context, rel *types.RelationInput) (string, error) {
				created = rel
				return "new-rel", nil
			}
			ext := &mockExtractor{
				entities: []types.ExtractedEntity{{Name: "Alice", Type: "Person"}, {Name: "Bob", Type: "Person"}},
				relations: []types.ExtractedRelation{
					{Source: "Alice", Target: "Bob", Type: "reports_to", Fact: "Alice reports to Bob"},
				},
			}
			result, err := New(WithStore(store), WithExtractor(ext)).IngestEpisode(context.Background(),
				&types.EpisodeInput{Body: "x", ReferenceTime: tc.refTime})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if created == nil || len(result.EpisodicEdges) != 1 {
				t.Fatal("expected the relation to be created")
			}
			if !created.ValidAt.Equal(tc.wantValidAt) {
				t.Errorf("ValidAt = %v, want %v", created.ValidAt, tc.wantValidAt)
			}
			if (created.InvalidAt == nil) != (tc.wantNewInvalid == nil) ||
				(created.InvalidAt != nil && !created.InvalidAt.Equal(*tc.wantNewInvalid)) {
				t.Errorf("new InvalidAt = %v, want %v", created.InvalidAt, tc.wantNewInvalid)
			}
			var got []string
			for _, c := range store.invalidated {
				got = append(got, c.uuid)
				if !c.invalidAt.Equal(tc.refTime) {
					t.Errorf("invalidated %s at %v, want %v", c.uuid, c.invalidAt, tc.refTime)
				}
			}
			if !slices.Equal(got, tc.wantInvalidated) {
				t.Errorf("invalidated = %v, want %v", got, tc.wantInvalidated)
			}
		})
	}
}

func TestSearchFacts_PassesLimitToStore(t *testing.T) {
	tests := []struct {
		name string
		opts []types.SearchOption
		want int
	}{
		{"default", nil, DefaultSearchLimit},
		{"explicit", []types.SearchOption{types.WithLimit(30)}, 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			var got int
			store.searchTextFn = func(_ context.Context, _ string, o *types.SearchOptions) ([]types.ScoredFact, error) {
				got = o.Limit
				return nil, nil
			}
			if _, err := New(WithStore(store)).SearchFacts(context.Background(), "q", tc.opts...); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("store limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReciprocalRankFusion_TiesAreDeterministic(t *testing.T) {
	a := []types.ScoredFact{{Fact: types.Fact{UUID: "b"}}}
	b := []types.ScoredFact{{Fact: types.Fact{UUID: "a"}}}
	for i := 0; i < 20; i++ {
		got := reciprocalRankFusion(a, b, 2)
		if got[0].UUID != "a" || got[1].UUID != "b" {
			t.Fatalf("tie order = %s,%s, want a,b", got[0].UUID, got[1].UUID)
		}
	}
}

// docDeletingStore extends mockStore with types.DocumentEpisodeDeleter.
type docDeletingStore struct {
	*mockStore
	calls [][2]string
}

func (d *docDeletingStore) DeleteDocumentEpisodes(_ context.Context, groupID, documentID string) error {
	d.calls = append(d.calls, [2]string{groupID, documentID})
	return nil
}

func TestDeleteDocumentEpisodes(t *testing.T) {
	store := &docDeletingStore{mockStore: newMockStore()}
	if err := New(WithStore(store)).DeleteDocumentEpisodes(context.Background(), "ns", "doc"); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 1 || store.calls[0] != [2]string{"ns", "doc"} {
		t.Errorf("calls = %v, want [[ns doc]]", store.calls)
	}
	if err := New(WithStore(newMockStore())).DeleteDocumentEpisodes(context.Background(), "ns", "doc"); err == nil {
		t.Error("expected error for a store without document deletion")
	}
	if err := New().DeleteDocumentEpisodes(context.Background(), "ns", "doc"); !errors.Is(err, types.ErrStoreNotReady) {
		t.Errorf("err = %v, want ErrStoreNotReady", err)
	}
}
