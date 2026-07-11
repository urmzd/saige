package pgstore

import (
	"context"
	"maps"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/postgres"
)

// testPool connects to the database named by SAIGE_TEST_POSTGRES_DSN, runs
// migrations, and truncates the kg tables so each test starts clean. Tests
// are skipped when the env var is unset (e.g. plain `go test ./...`).
// The database needs the pgvector extension available; a disposable instance:
//
//	docker run --rm -e POSTGRES_PASSWORD=test -p 5433:5432 pgvector/pgvector:pg17
//	SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./knowledge/pgstore/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}

	ctx := context.Background()

	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist before it can connect; bootstrap it with a plain
	// connection first. pg_trgm (used by the fuzzy finders) is created by
	// RunMigrations itself.
	boot, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("bootstrap connect: %v", err)
	}
	_, err = boot.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	boot.Close()
	if err != nil {
		t.Fatalf("create vector extension: %v", err)
	}

	pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE kg_mention, kg_relation, kg_episode, kg_entity RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// testVec returns a 768-dim embedding (the migration default) whose first
// component varies so different seeds are distinguishable under cosine
// distance.
func testVec(seed float32) []float32 {
	v := make([]float32, 768)
	v[0] = seed
	v[1] = 1
	return v
}

func mustUpsert(t *testing.T, s *Store, group, name, typ, summary string, emb []float32) string {
	t.Helper()
	id, err := s.UpsertEntity(context.Background(), &types.ExtractedEntity{
		Name: name, Type: typ, Summary: summary, GroupID: group,
	}, emb)
	if err != nil {
		t.Fatalf("upsert entity %s/%s: %v", group, name, err)
	}
	return id
}

func mustRelate(t *testing.T, s *Store, group, srcUUID, tgtUUID, fact string) string {
	t.Helper()
	id, err := s.CreateRelation(context.Background(), &types.RelationInput{
		SourceUUID: srcUUID, TargetUUID: tgtUUID,
		Type: "KNOWS", Fact: fact, GroupID: group,
	})
	if err != nil {
		t.Fatalf("create relation in %s: %v", group, err)
	}
	return id
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// TestEntityGroupIsolation is the acceptance criterion for entity tenancy:
// the same (name, type) in two groups yields two rows, the group-scoped
// finders see only their group, the legacy global finders see both, and a
// same-group upsert merges into one row.
func TestEntityGroupIsolation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	idA := mustUpsert(t, store, "a", "Ada Lovelace", "person", "from a", testVec(1))
	idB := mustUpsert(t, store, "b", "Ada Lovelace", "person", "from b", testVec(2))
	if idA == idB {
		t.Fatalf("same UUID %s across groups; expected distinct rows", idA)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE name = 'Ada Lovelace'`); n != 2 {
		t.Errorf("kg_entity rows = %d, want 2 (one per group)", n)
	}

	// Group-scoped exact finder sees only its group.
	for _, tc := range []struct{ group, want string }{{"a", idA}, {"b", idB}} {
		got, err := store.FindEntitiesByNameTypeInGroup(ctx, tc.group, "Ada Lovelace", "person")
		if err != nil {
			t.Fatalf("find in group %s: %v", tc.group, err)
		}
		if len(got) != 1 || got[0].UUID != tc.want {
			t.Errorf("group %s exact find = %+v, want single entity %s", tc.group, got, tc.want)
		}
	}

	// Group-scoped fuzzy finder (trigram) sees only its group.
	fuzzyA, err := store.FindEntitiesByFuzzyNameInGroup(ctx, "a", "Ada Lovelase", 10)
	if err != nil {
		t.Fatalf("fuzzy find in group a: %v", err)
	}
	if len(fuzzyA) != 1 || fuzzyA[0].UUID != idA {
		t.Errorf("group a fuzzy find = %+v, want single entity %s", fuzzyA, idA)
	}

	// Legacy global finders still see both groups (documented single-tenant
	// behavior).
	global, err := store.FindEntitiesByNameType(ctx, "Ada Lovelace", "person")
	if err != nil {
		t.Fatalf("global find: %v", err)
	}
	if len(global) != 2 {
		t.Errorf("global exact find = %d entities, want 2", len(global))
	}
	globalFuzzy, err := store.FindEntitiesByFuzzyName(ctx, "Ada Lovelase", 10)
	if err != nil {
		t.Fatalf("global fuzzy find: %v", err)
	}
	if len(globalFuzzy) != 2 {
		t.Errorf("global fuzzy find = %d entities, want 2", len(globalFuzzy))
	}

	// Same-group upsert conflicts merge: same UUID back, summary updated,
	// nil embedding preserves the stored one, still 2 rows total.
	idA2 := mustUpsert(t, store, "a", "Ada Lovelace", "person", "updated summary", nil)
	if idA2 != idA {
		t.Errorf("same-group upsert returned %s, want existing %s", idA2, idA)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE name = 'Ada Lovelace'`); n != 2 {
		t.Errorf("kg_entity rows after merge = %d, want 2", n)
	}
	ent, err := store.GetEntity(ctx, idA)
	if err != nil {
		t.Fatalf("get entity: %v", err)
	}
	if ent.Summary != "updated summary" {
		t.Errorf("merged summary = %q, want %q", ent.Summary, "updated summary")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE uuid = $1 AND embedding IS NOT NULL`, idA); n != 1 {
		t.Error("nil-embedding upsert wiped the stored embedding; COALESCE should preserve it")
	}
}

// seedGroup populates one tenant group with two entities, a relation, and an
// episode mentioning both entities. Entity names/summaries are identical
// across groups so global searches match everything and only the group
// filter separates tenants.
func seedGroup(t *testing.T, store *Store, group string, seed float32, meta map[string]string) (relUUID, epUUID string) {
	t.Helper()
	e1 := mustUpsert(t, store, group, "Alpha", "person", "expert in quantum computing", testVec(seed))
	e2 := mustUpsert(t, store, group, "Beta", "person", "studies quantum computing", testVec(seed+0.1))
	relUUID = mustRelate(t, store, group, e1, e2, "Alpha mentors Beta in "+group)
	ep, err := store.CreateEpisode(context.Background(), &types.EpisodeInput{
		Name: "ep-" + group, Body: "body " + group, Source: "test", GroupID: group, Metadata: meta,
	}, []string{e1, e2})
	if err != nil {
		t.Fatalf("create episode in %s: %v", group, err)
	}
	return relUUID, ep
}

func factUUIDs(facts []types.ScoredFact) map[string]bool {
	set := make(map[string]bool, len(facts))
	for _, f := range facts {
		set[f.Fact.UUID] = true
	}
	return set
}

// TestSearchGroupScoping proves relations carry group_id: with
// SearchOptions.GroupID set, embedding and text search return only that
// group's facts; without it, the legacy global queries return both groups.
func TestSearchGroupScoping(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	relA, _ := seedGroup(t, store, "a", 1, nil)
	relB, _ := seedGroup(t, store, "b", 2, nil)

	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE group_id = 'a'`); n != 1 {
		t.Fatalf("relations in group a = %d, want 1", n)
	}

	for _, tc := range []struct {
		group    string
		wantRel  string
		otherRel string
	}{
		{"a", relA, relB},
		{"b", relB, relA},
	} {
		opts := &types.SearchOptions{GroupID: tc.group}

		byEmb, err := store.SearchByEmbedding(ctx, testVec(1), opts)
		if err != nil {
			t.Fatalf("embedding search group %s: %v", tc.group, err)
		}
		got := factUUIDs(byEmb)
		if !got[tc.wantRel] {
			t.Errorf("embedding search group %s missing own fact %s", tc.group, tc.wantRel)
		}
		if got[tc.otherRel] {
			t.Errorf("embedding search group %s leaked fact %s from other group", tc.group, tc.otherRel)
		}

		byText, err := store.SearchByText(ctx, "quantum computing", opts)
		if err != nil {
			t.Fatalf("text search group %s: %v", tc.group, err)
		}
		got = factUUIDs(byText)
		if !got[tc.wantRel] {
			t.Errorf("text search group %s missing own fact %s", tc.group, tc.wantRel)
		}
		if got[tc.otherRel] {
			t.Errorf("text search group %s leaked fact %s from other group", tc.group, tc.otherRel)
		}
	}

	// Legacy global search (no GroupID) sees both groups.
	globalEmb, err := store.SearchByEmbedding(ctx, testVec(1), nil)
	if err != nil {
		t.Fatalf("global embedding search: %v", err)
	}
	if got := factUUIDs(globalEmb); !got[relA] || !got[relB] {
		t.Errorf("global embedding search = %v, want both %s and %s", got, relA, relB)
	}
	globalText, err := store.SearchByText(ctx, "quantum computing", nil)
	if err != nil {
		t.Fatalf("global text search: %v", err)
	}
	if got := factUUIDs(globalText); !got[relA] || !got[relB] {
		t.Errorf("global text search = %v, want both %s and %s", got, relA, relB)
	}
}

// TestEpisodeMetadataProvenanceRoundTrip verifies CreateEpisode metadata
// survives the JSONB round trip through GetFactProvenance, and that an
// episode created without metadata comes back with Metadata == nil.
func TestEpisodeMetadataProvenanceRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	meta := map[string]string{"channel": "slack", "author": "alice"}
	relUUID, epWithMeta := seedGroup(t, store, "g", 1, meta)

	// A second episode mentioning the same entities, without metadata.
	e1s, err := store.FindEntitiesByNameTypeInGroup(ctx, "g", "Alpha", "person")
	if err != nil || len(e1s) != 1 {
		t.Fatalf("find Alpha: %v (%d entities)", err, len(e1s))
	}
	epNoMeta, err := store.CreateEpisode(ctx, &types.EpisodeInput{
		Name: "ep-no-meta", Body: "no metadata", Source: "test", GroupID: "g",
	}, []string{e1s[0].UUID})
	if err != nil {
		t.Fatalf("create episode without metadata: %v", err)
	}

	episodes, err := store.GetFactProvenance(ctx, relUUID)
	if err != nil {
		t.Fatalf("get fact provenance: %v", err)
	}
	byUUID := make(map[string]types.Episode, len(episodes))
	for _, ep := range episodes {
		byUUID[ep.UUID] = ep
	}

	got, ok := byUUID[epWithMeta]
	if !ok {
		t.Fatalf("provenance missing episode %s; got %v", epWithMeta, episodes)
	}
	if !maps.Equal(got.Metadata, meta) {
		t.Errorf("metadata round trip = %v, want %v", got.Metadata, meta)
	}
	if got.GroupID != "g" {
		t.Errorf("episode group = %q, want %q", got.GroupID, "g")
	}

	bare, ok := byUUID[epNoMeta]
	if !ok {
		t.Fatalf("provenance missing metadata-less episode %s; got %v", epNoMeta, episodes)
	}
	if bare.Metadata != nil {
		t.Errorf("empty metadata = %v, want nil", bare.Metadata)
	}
}

// TestDeleteEpisodesGroup verifies DeleteEpisodes removes every trace of one
// group — episodes, relations, entities, and (via FK cascade) mentions —
// while leaving the other group fully intact, and rejects the empty group.
func TestDeleteEpisodesGroup(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewStore(pool, nil)

	seedGroup(t, store, "a", 1, nil)
	relB, epB := seedGroup(t, store, "b", 2, nil)

	if n := countRows(t, pool, `SELECT count(*) FROM kg_mention`); n != 4 {
		t.Fatalf("seeded mentions = %d, want 4 (2 per group)", n)
	}

	if err := store.DeleteEpisodes(ctx, "a"); err != nil {
		t.Fatalf("delete episodes group a: %v", err)
	}

	// Group a is gone from every table.
	for _, q := range []string{
		`SELECT count(*) FROM kg_episode WHERE group_id = 'a'`,
		`SELECT count(*) FROM kg_relation WHERE group_id = 'a'`,
		`SELECT count(*) FROM kg_entity WHERE group_id = 'a'`,
	} {
		if n := countRows(t, pool, q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	// Mentions cascaded: only group b's 2 remain.
	if n := countRows(t, pool, `SELECT count(*) FROM kg_mention`); n != 2 {
		t.Errorf("mentions after delete = %d, want 2 (group b only)", n)
	}

	// Group b is fully intact.
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE group_id = 'b'`); n != 2 {
		t.Errorf("group b entities = %d, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_relation WHERE uuid = $1`, relB); n != 1 {
		t.Errorf("group b relation missing after deleting group a")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_episode WHERE uuid = $1`, epB); n != 1 {
		t.Errorf("group b episode missing after deleting group a")
	}
	if episodes, err := store.GetFactProvenance(ctx, relB); err != nil || len(episodes) != 1 {
		t.Errorf("group b provenance after delete = %v (%v), want 1 episode", episodes, err)
	}

	// The default group holds legacy data and must be rejected.
	if err := store.DeleteEpisodes(ctx, ""); err == nil {
		t.Error("DeleteEpisodes(\"\") succeeded, want error")
	}
}

// TestMigrationUpgradeFromUnscopedKGSchema recreates the pre-group_id kg
// schema (kg_entity with a global UNIQUE(name, type) and no group_id,
// kg_relation without group_id, kg_episode without metadata) and verifies
// RunMigrations upgrades it in place: columns added, the global unique
// constraint replaced by the (group_id, name, type) index, legacy rows land
// in group "" and stay findable, and the same (name, type) can then exist in
// another group.
func TestMigrationUpgradeFromUnscopedKGSchema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	stmts := []string{
		`DROP TABLE IF EXISTS kg_mention`,
		`DROP TABLE IF EXISTS kg_relation`,
		`DROP TABLE IF EXISTS kg_episode`,
		`DROP TABLE IF EXISTS kg_entity`,
		`CREATE TABLE kg_entity (
			id         BIGSERIAL PRIMARY KEY,
			uuid       TEXT NOT NULL UNIQUE,
			name       TEXT NOT NULL,
			type       TEXT NOT NULL,
			summary    TEXT NOT NULL DEFAULT '',
			embedding  vector(768),
			search_vec tsvector GENERATED ALWAYS AS (
				to_tsvector('english', coalesce(name,'') || ' ' || coalesce(summary,''))
			) STORED,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (name, type)
		)`,
		`CREATE TABLE kg_relation (
			id         BIGSERIAL PRIMARY KEY,
			uuid       TEXT NOT NULL UNIQUE,
			source_id  BIGINT NOT NULL REFERENCES kg_entity(id) ON DELETE CASCADE,
			target_id  BIGINT NOT NULL REFERENCES kg_entity(id) ON DELETE CASCADE,
			type       TEXT NOT NULL,
			fact       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			valid_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			invalid_at TIMESTAMPTZ
		)`,
		`CREATE TABLE kg_episode (
			id         BIGSERIAL PRIMARY KEY,
			uuid       TEXT NOT NULL UNIQUE,
			name       TEXT NOT NULL,
			body       TEXT NOT NULL,
			source     TEXT NOT NULL DEFAULT '',
			group_id   TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE kg_mention (
			episode_id BIGINT NOT NULL REFERENCES kg_episode(id) ON DELETE CASCADE,
			entity_id  BIGINT NOT NULL REFERENCES kg_entity(id) ON DELETE CASCADE,
			PRIMARY KEY (episode_id, entity_id)
		)`,
		`INSERT INTO kg_entity (uuid, name, type, summary) VALUES
			('legacy-src', 'Ada Lovelace', 'person', 'legacy pioneer'),
			('legacy-tgt', 'Charles Babbage', 'person', 'legacy engineer')`,
		`INSERT INTO kg_relation (uuid, source_id, target_id, type, fact)
			SELECT 'legacy-rel', s.id, t.id, 'KNOWS', 'legacy fact'
			FROM kg_entity s, kg_entity t
			WHERE s.uuid = 'legacy-src' AND t.uuid = 'legacy-tgt'`,
		`INSERT INTO kg_episode (uuid, name, body) VALUES ('legacy-ep', 'legacy episode', 'legacy body')`,
		// Group-scoped legacy data: before group_id existed on entities and
		// relations, tenancy lived only on episodes. g1-alpha and g1-beta are
		// mentioned solely from group g1 episodes; 'ambig' is mentioned from
		// both g1 and g2 (the old cross-tenant merge bug) and cannot be
		// cleanly assigned.
		`INSERT INTO kg_entity (uuid, name, type, summary) VALUES
			('g1-alpha', 'Grace Hopper', 'person', 'expert in quantum computing'),
			('g1-beta', 'Alan Turing', 'person', 'studies quantum computing'),
			('ambig', 'Shared Entity', 'person', 'mentioned by two groups')`,
		`INSERT INTO kg_relation (uuid, source_id, target_id, type, fact)
			SELECT 'g1-rel', s.id, t.id, 'MENTORS', 'Grace mentors Alan in quantum computing'
			FROM kg_entity s, kg_entity t
			WHERE s.uuid = 'g1-alpha' AND t.uuid = 'g1-beta'`,
		`INSERT INTO kg_relation (uuid, source_id, target_id, type, fact)
			SELECT 'ambig-rel', s.id, t.id, 'KNOWS', 'cross-group fact'
			FROM kg_entity s, kg_entity t
			WHERE s.uuid = 'g1-alpha' AND t.uuid = 'ambig'`,
		`INSERT INTO kg_episode (uuid, name, body, group_id) VALUES
			('ep-g1', 'episode g1', 'body g1', 'g1'),
			('ep-g2', 'episode g2', 'body g2', 'g2')`,
		`INSERT INTO kg_mention (episode_id, entity_id)
			SELECT ep.id, e.id
			FROM kg_episode ep, kg_entity e
			WHERE (ep.uuid, e.uuid) IN
				(('ep-g1', 'g1-alpha'), ('ep-g1', 'g1-beta'),
				 ('ep-g1', 'ambig'), ('ep-g2', 'ambig'))`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("recreate legacy schema: %v", err)
		}
	}

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations on legacy schema: %v", err)
	}

	// New columns exist.
	for _, col := range []struct{ table, column string }{
		{"kg_entity", "group_id"},
		{"kg_relation", "group_id"},
		{"kg_episode", "metadata"},
	} {
		n := countRows(t, pool,
			`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
			col.table, col.column)
		if n != 1 {
			t.Errorf("column %s.%s missing after upgrade", col.table, col.column)
		}
	}

	// The global unique constraint is gone; the group-scoped index exists.
	if n := countRows(t, pool,
		`SELECT count(*) FROM pg_constraint WHERE conname = 'kg_entity_name_type_key'`); n != 0 {
		t.Error("legacy constraint kg_entity_name_type_key still present after upgrade")
	}
	if n := countRows(t, pool,
		`SELECT count(*) FROM pg_indexes WHERE tablename = 'kg_entity' AND indexname = 'idx_kg_entity_group_name_type'`); n != 1 {
		t.Error("idx_kg_entity_group_name_type missing after upgrade")
	}

	// Legacy rows landed in group "".
	for _, q := range []string{
		`SELECT count(*) FROM kg_entity WHERE group_id = ''`,
		`SELECT count(*) FROM kg_relation WHERE uuid = 'legacy-rel' AND group_id = ''`,
	} {
		if n := countRows(t, pool, q); n == 0 {
			t.Errorf("%s = 0, want legacy rows in group ''", q)
		}
	}

	// Backfill: entities mentioned from exactly one non-empty group got that
	// group; the entity mentioned from two groups stays in "" (it cannot be
	// cleanly assigned to either tenant). Relations follow their endpoints
	// when both agree on a non-empty group.
	groupOf := func(table, uuid string) string {
		t.Helper()
		var g string
		if err := pool.QueryRow(ctx,
			`SELECT group_id FROM `+table+` WHERE uuid = $1`, uuid).Scan(&g); err != nil {
			t.Fatalf("group of %s %s: %v", table, uuid, err)
		}
		return g
	}
	for _, tc := range []struct{ table, uuid, want string }{
		{"kg_entity", "g1-alpha", "g1"},
		{"kg_entity", "g1-beta", "g1"},
		{"kg_entity", "ambig", ""},
		{"kg_entity", "legacy-src", ""},
		{"kg_entity", "legacy-tgt", ""},
		{"kg_relation", "g1-rel", "g1"},
		{"kg_relation", "ambig-rel", ""},
		{"kg_relation", "legacy-rel", ""},
	} {
		if got := groupOf(tc.table, tc.uuid); got != tc.want {
			t.Errorf("%s %s group_id = %q, want %q", tc.table, tc.uuid, got, tc.want)
		}
	}

	store := NewStore(pool, nil)

	// Backfilled rows are visible to group-scoped search. Text search matches
	// the g1 entities' summaries; embedding search needs a vector, which the
	// legacy rows lack, so give g1-alpha one first (the migration never
	// touches embeddings).
	byText, err := store.SearchByText(ctx, "quantum computing", &types.SearchOptions{GroupID: "g1"})
	if err != nil {
		t.Fatalf("group-scoped text search after upgrade: %v", err)
	}
	if got := factUUIDs(byText); !got["g1-rel"] || got["ambig-rel"] || got["legacy-rel"] {
		t.Errorf("g1 text search = %v, want g1-rel only (no ambig-rel/legacy-rel)", got)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE kg_entity SET embedding = $1 WHERE uuid = 'g1-alpha'`,
		pgvector.NewVector(testVec(1))); err != nil {
		t.Fatalf("set embedding on backfilled entity: %v", err)
	}
	byEmb, err := store.SearchByEmbedding(ctx, testVec(1), &types.SearchOptions{GroupID: "g1"})
	if err != nil {
		t.Fatalf("group-scoped embedding search after upgrade: %v", err)
	}
	if got := factUUIDs(byEmb); !got["g1-rel"] || got["ambig-rel"] {
		t.Errorf("g1 embedding search = %v, want g1-rel only (no ambig-rel)", got)
	}

	// Legacy entity is findable both globally and via the "" group.
	global, err := store.FindEntitiesByNameType(ctx, "Ada Lovelace", "person")
	if err != nil || len(global) != 1 {
		t.Fatalf("global find after upgrade = %v (%v), want 1 entity", global, err)
	}
	inDefault, err := store.FindEntitiesByNameTypeInGroup(ctx, "", "Ada Lovelace", "person")
	if err != nil || len(inDefault) != 1 || inDefault[0].UUID != "legacy-src" {
		t.Fatalf("default-group find after upgrade = %v (%v), want legacy-src", inDefault, err)
	}

	// Upserting the same (name, type) in group "" merges with the legacy row
	// via the new (group_id, name, type) conflict target...
	merged := mustUpsert(t, store, "", "Ada Lovelace", "person", "merged", nil)
	if merged != "legacy-src" {
		t.Errorf("default-group upsert = %s, want merge into legacy-src", merged)
	}
	// ...while the same (name, type) in a new group creates a separate row.
	scoped := mustUpsert(t, store, "tenant-1", "Ada Lovelace", "person", "scoped", nil)
	if scoped == "legacy-src" {
		t.Error("tenant-scoped upsert merged into the legacy global row")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM kg_entity WHERE name = 'Ada Lovelace'`); n != 2 {
		t.Errorf("Ada Lovelace rows after scoped upsert = %d, want 2", n)
	}

	// Re-running migrations is a no-op: the backfill must not move any row a
	// second time (guarded by group_id = ''), including the deliberately
	// unassigned ambiguous rows.
	snapshot := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT 'e:' || uuid, group_id FROM kg_entity
			UNION ALL
			SELECT 'r:' || uuid, group_id FROM kg_relation`)
		if err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		defer rows.Close()
		snap := make(map[string]string)
		for rows.Next() {
			var key, group string
			if err := rows.Scan(&key, &group); err != nil {
				t.Fatalf("snapshot scan: %v", err)
			}
			snap[key] = group
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows: %v", err)
		}
		return snap
	}
	before := snapshot()
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	if after := snapshot(); !maps.Equal(before, after) {
		t.Errorf("re-running migrations changed rows:\nbefore %v\nafter  %v", before, after)
	}
}
