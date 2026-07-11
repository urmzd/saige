package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
)

// testPool connects to the database named by SAIGE_TEST_POSTGRES_DSN, runs
// migrations, and truncates the agent tables so each test starts clean. Tests
// are skipped when the env var is unset (e.g. plain `go test ./...`).
// The database needs the pgvector extension available; a disposable instance:
//
//	docker run --rm -e POSTGRES_PASSWORD=test -p 5433:5432 pgvector/pgvector:pg17
//	SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./agent/pgstore/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping PostgreSQL integration test")
	}

	ctx := context.Background()

	// postgres.NewPool registers pgvector types at connect time, so the
	// extension must exist before it can connect; bootstrap it with a plain
	// connection first.
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
	if _, err := pool.Exec(ctx, `TRUNCATE agent_node, agent_branch, agent_checkpoint`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func testNode(id, parent types.NodeID, branch types.BranchID, depth int) *types.Node {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &types.Node{
		ID:        id,
		ParentID:  parent,
		Message:   types.NewUserMessage("msg " + string(id)),
		Version:   1,
		Depth:     depth,
		BranchID:  branch,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// saveConversation persists a two-node tree the way Agent.persistNode does:
// node + branch tip committed in one transaction.
func saveConversation(t *testing.T, store *Store, root, tip types.NodeID) {
	t.Helper()
	ctx := context.Background()
	for _, n := range []*types.Node{
		testNode(root, "", "main", 0),
		testNode(tip, root, "main", 1),
	} {
		node := n
		err := store.Tx(ctx, func(tx types.StoreTx) error {
			if err := tx.SaveNode(ctx, node); err != nil {
				return err
			}
			return tx.SaveBranch(ctx, node.BranchID, node.ID)
		})
		if err != nil {
			t.Fatalf("persist node %s: %v", node.ID, err)
		}
	}
}

// TestConversationIsolation is the acceptance criterion for branch
// namespacing: two conversations must both be able to use "main" without
// touching each other's nodes, branches, or checkpoints.
func TestConversationIsolation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	storeA := NewStore(pool, "conv-a", nil)
	storeB := NewStore(pool, "conv-b", nil)

	saveConversation(t, storeA, "root-a", "tip-a")
	saveConversation(t, storeB, "root-b", "tip-b")

	// Each conversation's "main" points at its own tip: B's writes must not
	// have overwritten A's branch row.
	tipA, err := storeA.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatalf("load branch A: %v", err)
	}
	if tipA != "tip-a" {
		t.Errorf("conversation A main tip = %s, want tip-a", tipA)
	}
	tipB, err := storeB.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatalf("load branch B: %v", err)
	}
	if tipB != "tip-b" {
		t.Errorf("conversation B main tip = %s, want tip-b", tipB)
	}

	// A branch created only in A must not leak into B's branch map.
	if err := storeA.SaveBranch(ctx, "experiment", "tip-a"); err != nil {
		t.Fatalf("save branch: %v", err)
	}
	branchesB, err := storeB.ListBranches(ctx)
	if err != nil {
		t.Fatalf("list branches B: %v", err)
	}
	if _, leaked := branchesB["experiment"]; leaked {
		t.Error("branch 'experiment' from conversation A leaked into conversation B")
	}
	if len(branchesB) != 1 || branchesB["main"] != "tip-b" {
		t.Errorf("conversation B branches = %v, want map[main:tip-b]", branchesB)
	}

	// LoadTree must return only the conversation's own nodes and branches.
	nodesB, treeBranchesB, err := storeB.LoadTree(ctx, "root-b")
	if err != nil {
		t.Fatalf("load tree B: %v", err)
	}
	for _, n := range nodesB {
		if n.ID == "root-a" || n.ID == "tip-a" {
			t.Errorf("conversation A node %s leaked into conversation B's tree", n.ID)
		}
	}
	if len(nodesB) != 2 {
		t.Errorf("conversation B tree has %d nodes, want 2", len(nodesB))
	}
	if _, leaked := treeBranchesB["experiment"]; leaked {
		t.Error("LoadTree imported conversation A's branch map into conversation B")
	}

	// Checkpoints are scoped too: A's checkpoint is invisible to B.
	cp := types.Checkpoint{
		ID:        "cp-1",
		Branch:    "main",
		NodeID:    "tip-a",
		Name:      "before-refactor",
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := storeA.SaveCheckpoint(ctx, cp); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	got, err := storeA.LoadCheckpoint(ctx, "cp-1")
	if err != nil {
		t.Fatalf("load checkpoint A: %v", err)
	}
	if got.NodeID != "tip-a" || got.Branch != "main" {
		t.Errorf("checkpoint A = %+v, want node tip-a on main", got)
	}
	if _, err := storeB.LoadCheckpoint(ctx, "cp-1"); err == nil {
		t.Error("conversation B loaded conversation A's checkpoint")
	}
}

// TestBranchUpsertSameConversation verifies the tip still advances via upsert
// within one conversation (the ON CONFLICT path).
func TestBranchUpsertSameConversation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	store := NewStore(pool, "conv-a", nil)
	if err := store.SaveBranch(ctx, "main", "n1"); err != nil {
		t.Fatalf("save branch: %v", err)
	}
	if err := store.SaveBranch(ctx, "main", "n2"); err != nil {
		t.Fatalf("advance branch: %v", err)
	}
	tip, err := store.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatalf("load branch: %v", err)
	}
	if tip != "n2" {
		t.Errorf("tip = %s, want n2", tip)
	}
}

// TestMigrationUpgradeFromUnscopedSchema recreates the pre-namespacing schema
// (branch_id globally UNIQUE, no conversation_id) and verifies RunMigrations
// upgrades it in place: the column is added, the global unique constraint is
// dropped, and two conversations can then both hold a "main" branch. Legacy
// rows land in the "" namespace and stay readable.
func TestMigrationUpgradeFromUnscopedSchema(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	stmts := []string{
		`DROP TABLE IF EXISTS agent_branch`,
		`CREATE TABLE agent_branch (
			id        BIGSERIAL PRIMARY KEY,
			branch_id TEXT NOT NULL UNIQUE,
			tip_uuid  TEXT NOT NULL
		)`,
		`INSERT INTO agent_branch (branch_id, tip_uuid) VALUES ('main', 'legacy-tip')`,
		`DROP TABLE IF EXISTS agent_checkpoint`,
		`CREATE TABLE agent_checkpoint (
			id         BIGSERIAL PRIMARY KEY,
			uuid       TEXT NOT NULL UNIQUE,
			branch_id  TEXT NOT NULL,
			node_uuid  TEXT NOT NULL,
			name       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("recreate legacy schema: %v", err)
		}
	}

	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations on legacy schema: %v", err)
	}

	// The legacy row is preserved in the "" namespace.
	legacy := NewStore(pool, "", nil)
	tip, err := legacy.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatalf("load legacy branch: %v", err)
	}
	if tip != "legacy-tip" {
		t.Errorf("legacy tip = %s, want legacy-tip", tip)
	}

	// Post-upgrade, two conversations can both use "main".
	for _, conv := range []struct{ id, tip string }{{"conv-a", "tip-a"}, {"conv-b", "tip-b"}} {
		store := NewStore(pool, conv.id, nil)
		if err := store.SaveBranch(ctx, "main", types.NodeID(conv.tip)); err != nil {
			t.Fatalf("save branch for %s: %v", conv.id, err)
		}
	}
	for _, conv := range []struct{ id, tip string }{{"conv-a", "tip-a"}, {"conv-b", "tip-b"}} {
		store := NewStore(pool, conv.id, nil)
		tip, err := store.LoadBranch(ctx, "main")
		if err != nil {
			t.Fatalf("load branch for %s: %v", conv.id, err)
		}
		if string(tip) != conv.tip {
			t.Errorf("%s main tip = %s, want %s", conv.id, tip, conv.tip)
		}
	}
}
