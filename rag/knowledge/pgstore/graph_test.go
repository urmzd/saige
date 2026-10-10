package pgstore

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/rag/knowledge/internal/engine"
	"github.com/urmzd/saige/rag/knowledge/types"
)

// scriptedExtractor returns a fixed extraction per episode body.
type scriptedExtractor map[string]struct {
	ents []types.ExtractedEntity
	rels []types.ExtractedRelation
}

func (s scriptedExtractor) Extract(_ context.Context, text string) ([]types.ExtractedEntity, []types.ExtractedRelation, error) {
	out, ok := s[text]
	if !ok {
		return nil, nil, fmt.Errorf("no script for %q", text)
	}
	return out.ents, out.rels, nil
}

var provenanceScript = scriptedExtractor{
	"Alice works at Acme": {
		ents: []types.ExtractedEntity{{Name: "Alice", Type: "Person"}, {Name: "Acme", Type: "Organization"}},
		rels: []types.ExtractedRelation{{Source: "Alice", Target: "Acme", Type: "works_at", Fact: "Alice works at Acme"}},
	},
	"Alice likes tea": {
		ents: []types.ExtractedEntity{{Name: "Alice", Type: "Person"}, {Name: "Tea", Type: "Drink"}, {Name: "Acme", Type: "Organization"}},
		rels: []types.ExtractedRelation{{Source: "Alice", Target: "Tea", Type: "likes", Fact: "Alice likes tea"}},
	},
	"Bob works at Acme": {
		ents: []types.ExtractedEntity{{Name: "Bob", Type: "Person"}, {Name: "Acme", Type: "Organization"}, {Name: "Alice", Type: "Person"}},
		rels: []types.ExtractedRelation{
			{Source: "Bob", Target: "Acme", Type: "works_at", Fact: "Bob works at Acme"},
			{Source: "Bob", Target: "Alice", Type: "knows", Fact: "Bob knows Alice"},
		},
	},
}

func ingest(t *testing.T, eng *engine.GraphEngine, in *types.EpisodeInput) *types.IngestResult {
	t.Helper()
	res, err := eng.IngestEpisode(context.Background(), in)
	if err != nil {
		t.Fatalf("ingest %q: %v", in.Body, err)
	}
	return res
}

func relationUUID(t *testing.T, res *types.IngestResult, relType string) string {
	t.Helper()
	for _, r := range res.EpisodicEdges {
		if r.Type == relType {
			return r.UUID
		}
	}
	t.Fatalf("relation %s not created; got %+v", relType, res.EpisodicEdges)
	return ""
}

// TestFactProvenanceFromAssertingEpisodes: provenance lists only the episodes
// that asserted the relation, in creation order, including a later episode
// whose repeat of the fact was deduplicated.
func TestFactProvenanceFromAssertingEpisodes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	eng := engine.New(engine.WithStore(must.Get(New(Config{Pool: pool}))), engine.WithExtractor(provenanceScript))

	e1 := ingest(t, eng, &types.EpisodeInput{Name: "e1", Body: "Alice works at Acme", GroupID: "g"})
	worksAt := relationUUID(t, e1, "works_at")
	ingest(t, eng, &types.EpisodeInput{Name: "e2", Body: "Alice likes tea", GroupID: "g"})

	eps, err := eng.GetFactProvenance(ctx, worksAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].UUID != e1.UUID {
		t.Fatalf("provenance = %v, want only e1", episodeNames(eps))
	}

	e3 := ingest(t, eng, &types.EpisodeInput{Name: "e3", Body: "Alice works at Acme", GroupID: "g"})
	if len(e3.EpisodicEdges) != 0 {
		t.Fatalf("repeated fact created %d relations, want 0 (deduplicated)", len(e3.EpisodicEdges))
	}
	eps, err = eng.GetFactProvenance(ctx, worksAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := episodeNames(eps); !slices.Equal(got, []string{"e1", "e3"}) {
		t.Errorf("provenance = %v, want [e1 e3]", got)
	}
}

func episodeNames(eps []types.Episode) []string {
	out := make([]string, len(eps))
	for i, ep := range eps {
		out[i] = ep.Name
	}
	return out
}

// TestNamespaceSharesEntitiesAcrossDocuments: two documents in one
// namespace share one Alice node; deleting one document removes only the
// relations it alone asserted and keeps shared entities.
func TestNamespaceSharesEntitiesAcrossDocuments(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))
	eng := engine.New(engine.WithStore(store), engine.WithExtractor(provenanceScript))

	a := ingest(t, eng, &types.EpisodeInput{Name: "a", Body: "Alice works at Acme", GroupID: "ns", DocumentID: "docA"})
	b := ingest(t, eng, &types.EpisodeInput{Name: "b", Body: "Bob works at Acme", GroupID: "ns", DocumentID: "docB"})

	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE name = 'Alice'`); n != 1 {
		t.Fatalf("Alice rows = %d, want 1", n)
	}
	alice := a.EntityNodes[0].UUID
	node, err := store.GetNode(ctx, alice, 1)
	if err != nil {
		t.Fatal(err)
	}
	edgeIDs := make(map[string]bool)
	for _, e := range node.Edges {
		edgeIDs[e.ID] = true
	}
	aliceWorks, bobKnows := relationUUID(t, a, "works_at"), relationUUID(t, b, "knows")
	if !edgeIDs[aliceWorks] || !edgeIDs[bobKnows] {
		t.Errorf("Alice edges = %v, want edges from both documents", edgeIDs)
	}

	eps, err := store.GetFactProvenance(ctx, bobKnows)
	if err != nil || len(eps) != 1 || eps[0].DocumentID != "docB" {
		t.Errorf("provenance = %+v (%v), want one episode of docB", eps, err)
	}

	if err := store.DeleteDocumentEpisodes(ctx, "ns", "docA"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, aliceWorks); n != 0 {
		t.Error("docA's relation survived the delete")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, bobKnows); n != 1 {
		t.Error("docB's relation was deleted with docA")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE uuid = $1`, alice); n != 1 {
		t.Error("shared entity Alice was deleted with docA")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_episode WHERE document_id = 'docA'`); n != 0 {
		t.Error("docA episodes survived the delete")
	}

	// A relation asserted by both documents survives deleting one of them.
	c := ingest(t, eng, &types.EpisodeInput{Name: "c", Body: "Alice likes tea", GroupID: "ns", DocumentID: "docC"})
	ingest(t, eng, &types.EpisodeInput{Name: "d", Body: "Alice likes tea", GroupID: "ns", DocumentID: "docD"})
	likes := relationUUID(t, c, "likes")
	if err := store.DeleteDocumentEpisodes(ctx, "ns", "docC"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, likes); n != 1 {
		t.Error("relation still asserted by docD was deleted")
	}
	// Deleting the last document removes the relation and the orphaned entity.
	if err := store.DeleteDocumentEpisodes(ctx, "ns", "docD"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, likes); n != 0 {
		t.Error("relation survived deleting its last document")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE name = 'Tea'`); n != 0 {
		t.Error("orphaned entity Tea survived")
	}

	if err := store.DeleteDocumentEpisodes(ctx, "ns", ""); err == nil {
		t.Error("empty document id accepted")
	}
}

// TestSearchMatchesFactTextAndBoundsRows covers fact-text search, the SQL
// limit with a hub entity, deterministic order, and as-of queries.
func TestSearchMatchesFactTextAndBoundsRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))

	carol := mustUpsert(t, store, "g", "Carol", "person", "engineer", testVec(1))
	dave := mustUpsert(t, store, "g", "Dave", "person", "manager", testVec(2))
	reports, err := store.CreateRelation(ctx, &types.RelationInput{
		SourceUUID: carol, TargetUUID: dave, Type: "reports_to", Fact: "Carol reports to Dave", GroupID: "g",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.SearchByText(ctx, "reports", &types.SearchOptions{GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if !factUUIDs(got)[reports] {
		t.Fatalf("fact-text search = %v, want %s", factUUIDs(got), reports)
	}
	if got[0].Fact.SourceNode.UUID != carol || got[0].Fact.TargetNode.UUID != dave {
		t.Errorf("fact endpoints = %s -> %s, want Carol -> Dave", got[0].Fact.SourceNode.Name, got[0].Fact.TargetNode.Name)
	}

	// A hub entity with 500 edges returns at most Limit facts.
	hub := mustUpsert(t, store, "h", "Hub", "thing", "central hub", testVec(3))
	if _, err := pool.Exec(ctx, `
		INSERT INTO kg_entity (uuid, name, type, group_id)
		SELECT 'leaf-' || i, 'Leaf ' || i, 'thing', 'h' FROM generate_series(1, 500) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO kg_relation (uuid, source_id, target_id, type, fact, group_id, valid_at)
		SELECT 'edge-' || i, h.id, l.id, 'links', 'hub links leaf', 'h', '2024-01-01'::timestamptz
		FROM generate_series(1, 500) i
		JOIN kg_entity l ON l.uuid = 'leaf-' || i
		CROSS JOIN (SELECT id FROM kg_entity WHERE uuid = $1) h`, hub); err != nil {
		t.Fatal(err)
	}
	opts := &types.SearchOptions{GroupID: "h", Limit: 5}
	first, err := store.SearchByText(ctx, "hub", opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 5 {
		t.Fatalf("hub text search rows = %d, want 5", len(first))
	}
	second, err := store.SearchByText(ctx, "hub", opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].Fact.UUID != second[i].Fact.UUID {
			t.Fatalf("order differs between runs at %d: %s vs %s", i, first[i].Fact.UUID, second[i].Fact.UUID)
		}
	}
	byEmb, err := store.SearchByEmbedding(ctx, testVec(3), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(byEmb) != 5 {
		t.Errorf("hub embedding search rows = %d, want 5", len(byEmb))
	}

	// As-of: a superseded fact is returned for a time inside its validity.
	t2020 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	t2022 := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	old, err := store.CreateRelation(ctx, &types.RelationInput{
		SourceUUID: carol, TargetUUID: dave, Type: "mentored_by", Fact: "Carol mentored by Dave",
		ValidAt: t2020, InvalidAt: &t2022, GroupID: "g",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.SearchByText(ctx, "mentored", &types.SearchOptions{GroupID: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if factUUIDs(current)[old] {
		t.Error("superseded fact returned without an as-of time")
	}
	asOf := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	past, err := store.SearchByText(ctx, "mentored", &types.SearchOptions{GroupID: "g", ValidAt: &asOf})
	if err != nil {
		t.Fatal(err)
	}
	if !factUUIDs(past)[old] {
		t.Error("as-of 2021 search missed the fact valid in 2021")
	}
	before := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	tooEarly, err := store.SearchByText(ctx, "mentored", &types.SearchOptions{GroupID: "g", ValidAt: &before})
	if err != nil {
		t.Fatal(err)
	}
	if factUUIDs(tooEarly)[old] {
		t.Error("as-of 2019 search returned a fact that became valid in 2020")
	}
}

// queryCounter counts executed statements whose SQL contains a marker.
type queryCounter struct {
	marker string
	n      atomic.Int64
}

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, q.marker) {
		q.n.Add(1)
	}
	return ctx
}

func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestGetNodeBatchesHopsAndCapsSize: a star with 1000 leaves at depth 2
// runs one neighbor query per hop, returns no duplicate edges, and reports
// truncation when the node cap is reached.
func TestGetNodeBatchesHopsAndCapsSize(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(os.Getenv("SAIGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	counter := &queryCounter{marker: "WITH frontier AS"}
	cfg.ConnConfig.Tracer = counter
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { return pgxvector.RegisterTypes(ctx, conn) }
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)

	seed := must.Get(New(Config{Pool: pool}))
	center := mustUpsert(t, seed, "s", "Center", "thing", "", nil)
	if _, err := pool.Exec(ctx, `
		INSERT INTO kg_entity (uuid, name, type, group_id)
		SELECT 'star-' || i, 'Star ' || i, 'thing', 's' FROM generate_series(1, 1000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO kg_relation (uuid, source_id, target_id, type, fact, group_id)
		SELECT 'ray-' || i, c.id, l.id, 'ray', 'center to star', 's'
		FROM generate_series(1, 1000) i
		JOIN kg_entity l ON l.uuid = 'star-' || i
		CROSS JOIN (SELECT id FROM kg_entity WHERE uuid = $1) c`, center); err != nil {
		t.Fatal(err)
	}
	// Two leaves linked to each other: the edge is reachable from both.
	if _, err := pool.Exec(ctx, `
		INSERT INTO kg_relation (uuid, source_id, target_id, type, fact, group_id)
		SELECT 'chord', a.id, b.id, 'chord', 'star to star', 's'
		FROM kg_entity a, kg_entity b WHERE a.uuid = 'star-1' AND b.uuid = 'star-2'`); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		opts          []Option
		wantNeighbors int
		wantEdges     int
		wantTruncated bool
	}{
		{"full traversal", nil, 1000, 1001, false},
		{"node cap", []Option{WithTraversalLimits(100, 0)}, 100, 100, true},
		{"edge cap", []Option{WithTraversalLimits(0, 10)}, 10, 10, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counter.n.Store(0)
			store := must.Get(New(Config{Pool: traced}, tc.opts...))
			node, err := store.GetNode(ctx, center, 2)
			if err != nil {
				t.Fatal(err)
			}
			if n := counter.n.Load(); n > 3 {
				t.Errorf("neighbor queries = %d, want at most depth+1 = 3", n)
			}
			seen := make(map[string]bool)
			for _, e := range node.Edges {
				if seen[e.ID] {
					t.Fatalf("duplicate edge %s", e.ID)
				}
				seen[e.ID] = true
			}
			if len(node.Neighbors) != tc.wantNeighbors || len(node.Edges) != tc.wantEdges {
				t.Errorf("neighbors=%d edges=%d, want %d and %d",
					len(node.Neighbors), len(node.Edges), tc.wantNeighbors, tc.wantEdges)
			}
			if node.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", node.Truncated, tc.wantTruncated)
			}
		})
	}
}

// constEmbedder gives every text the same embedding, so embedding search
// anchors on every entity and filtering is left to the validity predicate.
type constEmbedder struct{}

func (constEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = testVec(1)
	}
	return out, nil
}

// reportsToScript holds contradicting facts about one Alice -> Bob
// reports_to edge. The fact texts differ enough not to be deduplicated.
var reportsToScript = func() scriptedExtractor {
	ents := []types.ExtractedEntity{{Name: "Alice", Type: "Person"}, {Name: "Bob", Type: "Person"}}
	s := scriptedExtractor{}
	for _, fact := range []string{
		"Alice reports to Bob as an intern",
		"Alice reports to Bob as a contractor",
		"Alice reports to Bob as a staff engineer",
		"Alice reports to Bob on loan from research",
	} {
		s[fact] = struct {
			ents []types.ExtractedEntity
			rels []types.ExtractedRelation
		}{ents: ents, rels: []types.ExtractedRelation{{Source: "Alice", Target: "Bob", Type: "reports_to", Fact: fact}}}
	}
	return s
}()

func year(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }

// relationEnd returns a relation's invalid_at, or nil when it is current.
func relationEnd(t *testing.T, pool *pgxpool.Pool, relUUID string) *time.Time {
	t.Helper()
	var end *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT invalid_at FROM kg_relation WHERE uuid = $1`, relUUID).Scan(&end); err != nil {
		t.Fatalf("read relation %s: %v", relUUID, err)
	}
	return end
}

// TestDeleteDocumentRestoresSupersededRelations: in a shared namespace a
// relation from one document can end another document's relation. Deleting
// the ending document recomputes the end from the relations that remain.
func TestDeleteDocumentRestoresSupersededRelations(t *testing.T) {
	type doc struct {
		id   string
		body string
		year int
	}
	tests := []struct {
		name   string
		docs   []doc
		delete string
		// wantEnd maps a remaining document to the year its relation ends,
		// or 0 when it must be current.
		wantEnd map[string]int
	}{
		{
			name: "newer contradiction removed",
			docs: []doc{
				{"docA", "Alice reports to Bob as an intern", 2020},
				{"docB", "Alice reports to Bob as a contractor", 2024},
			},
			delete:  "docB",
			wantEnd: map[string]int{"docA": 0},
		},
		{
			name: "newer fact removed after backfill",
			docs: []doc{
				{"docA", "Alice reports to Bob as an intern", 2024},
				{"docB", "Alice reports to Bob as a contractor", 2020},
			},
			delete:  "docA",
			wantEnd: map[string]int{"docB": 0},
		},
		{
			name: "middle fact removed",
			docs: []doc{
				{"docA", "Alice reports to Bob as an intern", 2020},
				{"docB", "Alice reports to Bob as a contractor", 2022},
				{"docC", "Alice reports to Bob as a staff engineer", 2024},
			},
			delete:  "docB",
			wantEnd: map[string]int{"docA": 2024, "docC": 0},
		},
		{
			name: "unrelated document removed",
			docs: []doc{
				{"docA", "Alice reports to Bob as an intern", 2020},
				{"docB", "Alice reports to Bob as a contractor", 2024},
				{"docC", "Alice reports to Bob on loan from research", 2018},
			},
			delete:  "docC",
			wantEnd: map[string]int{"docA": 2024, "docB": 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			store := must.Get(New(Config{Pool: pool}))
			eng := engine.New(engine.WithStore(store), engine.WithExtractor(reportsToScript))

			rels := make(map[string]string)
			for _, d := range tt.docs {
				res := ingest(t, eng, &types.EpisodeInput{
					Name: d.id, Body: d.body, GroupID: "ns", DocumentID: d.id, ReferenceTime: year(d.year),
				})
				rels[d.id] = relationUUID(t, res, "reports_to")
			}
			if err := store.DeleteDocumentEpisodes(ctx, "ns", tt.delete); err != nil {
				t.Fatal(err)
			}
			if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, rels[tt.delete]); n != 0 {
				t.Fatalf("relation of deleted %s survived", tt.delete)
			}

			current, err := eng.SearchFacts(ctx, "reports", types.WithGroupID("ns"))
			if err != nil {
				t.Fatal(err)
			}
			currentIDs := make(map[string]bool)
			for _, f := range current.Facts {
				currentIDs[f.UUID] = true
			}
			for id, wantYear := range tt.wantEnd {
				end := relationEnd(t, pool, rels[id])
				switch {
				case wantYear == 0 && end != nil:
					t.Errorf("%s ends at %v, want current", id, end)
				case wantYear != 0 && (end == nil || !end.Equal(year(wantYear))):
					t.Errorf("%s ends at %v, want %d", id, end, wantYear)
				}
				if currentIDs[rels[id]] != (wantYear == 0) {
					t.Errorf("%s in current search = %v, want %v", id, currentIDs[rels[id]], wantYear == 0)
				}
			}
		})
	}
}

// TestSearchFactsAsOf: a superseded fact is returned only for an as-of time
// inside its validity, on the text, embedding, and fused search paths. A
// backfilled fact is created already ended and never looks current.
func TestSearchFactsAsOf(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := must.Get(New(Config{Pool: pool}))
	eng := engine.New(engine.WithStore(store), engine.WithExtractor(reportsToScript), engine.WithEmbedder(constEmbedder{}))

	ingestAt := func(body string, y int) string {
		res := ingest(t, eng, &types.EpisodeInput{Name: body, Body: body, GroupID: "g", ReferenceTime: year(y)})
		return relationUUID(t, res, "reports_to")
	}
	old := ingestAt("Alice reports to Bob as an intern", 2020)
	cur := ingestAt("Alice reports to Bob as a contractor", 2024)
	backfill := ingestAt("Alice reports to Bob on loan from research", 2018)

	if end := relationEnd(t, pool, old); end == nil || !end.Equal(year(2024)) {
		t.Errorf("2020 fact ends at %v, want 2024", end)
	}
	if end := relationEnd(t, pool, backfill); end == nil || !end.Equal(year(2024)) {
		t.Errorf("backfilled fact ends at %v, want 2024 (the newer active fact)", end)
	}

	searches := map[string]func(*types.SearchOptions) (map[string]bool, error){
		"text": func(o *types.SearchOptions) (map[string]bool, error) {
			got, err := store.SearchByText(ctx, "reports", o)
			return factUUIDs(got), err
		},
		"embedding": func(o *types.SearchOptions) (map[string]bool, error) {
			got, err := store.SearchByEmbedding(ctx, testVec(1), o)
			return factUUIDs(got), err
		},
		"fused": func(o *types.SearchOptions) (map[string]bool, error) {
			opts := []types.SearchOption{types.WithGroupID(o.GroupID), types.WithLimit(o.Limit)}
			if o.ValidAt != nil {
				opts = append(opts, types.WithValidAt(*o.ValidAt))
			}
			res, err := eng.SearchFacts(ctx, "reports", opts...)
			if err != nil {
				return nil, err
			}
			ids := make(map[string]bool)
			for _, f := range res.Facts {
				ids[f.UUID] = true
			}
			return ids, nil
		},
	}
	asOf := []struct {
		name string
		at   *time.Time
		want map[string]bool
	}{
		{"current", nil, map[string]bool{cur: true}},
		{"2019", ptr(year(2019)), map[string]bool{backfill: true}},
		{"2021", ptr(year(2021)), map[string]bool{old: true, backfill: true}},
		{"2025", ptr(year(2025)), map[string]bool{cur: true}},
		{"2017", ptr(year(2017)), map[string]bool{}},
	}
	for path, search := range searches {
		for _, tc := range asOf {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				got, err := search(&types.SearchOptions{GroupID: "g", Limit: 20, ValidAt: tc.at})
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{old, cur, backfill} {
					if got[id] != tc.want[id] {
						t.Errorf("fact %s returned = %v, want %v", id, got[id], tc.want[id])
					}
				}
			})
		}
	}
}

func ptr[T any](v T) *T { return &v }
