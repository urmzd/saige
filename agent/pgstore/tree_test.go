package pgstore

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
)

// TestMigrationBackfillsNodeConversation recreates unscoped node rows and
// checks that RunMigrations assigns them to the conversation of the branch
// that reaches them, including a side node no branch tip reaches.
func TestMigrationBackfillsNodeConversation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	stmts := []string{
		`INSERT INTO agent_node (uuid, parent_uuid, role, message, branch_id, depth, conversation_id) VALUES
			('bf-root', '', 'system', '{}'::jsonb, 'main', 0, ''),
			('bf-user', 'bf-root', 'user', '{}'::jsonb, 'main', 1, ''),
			('bf-asst', 'bf-user', 'assistant', '{}'::jsonb, 'main', 2, ''),
			('bf-side', 'bf-user', 'assistant', '{}'::jsonb, 'main', 2, '')`,
		`INSERT INTO agent_branch (conversation_id, branch_id, tip_uuid) VALUES ('conv-x', 'main', 'bf-asst')`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	store := NewStore(pool, "conv-x", nil)
	nodes, _, err := store.LoadTree(ctx, "bf-root")
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("backfilled tree = %d nodes, want 4", len(nodes))
	}
	if _, err := NewStore(pool, "", nil).LoadNode(ctx, "bf-root"); err == nil {
		t.Error("backfilled node is still visible in the legacy namespace")
	}
}

// TestTreeWriteThroughSurvivesCancelledContext checks that a tree built with
// WithStore records a mutation made under an already-cancelled context, such
// as a tool result written after the caller gave up, and that a reload
// restores the active branch.
func TestTreeWriteThroughSurvivesCancelledContext(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	conv := "conv-" + types.NewID()
	store := NewStore(pool, conv, nil)
	tr, err := tree.New(types.SystemMsg(types.Text("system")), tree.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	root := tr.Root()
	call, err := tr.AddChild(ctx, root.ID, types.AssistantMessage{Parts: []types.AssistantPart{
		types.ToolCallPart{ID: "call-1", Name: "lookup"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := tr.AddChild(cancelled, call.ID, types.ToolResults(types.ToolResultPart{CallID: "call-1", Parts: []types.ToolOutputPart{types.Text("done")}}))
	if err != nil {
		t.Fatalf("AddChild with cancelled context: %v", err)
	}
	side, _, err := tr.Branch(ctx, root.ID, "side", types.UserMsg(types.Text("other")))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.SetActiveContext(cancelled, side); err != nil {
		t.Fatal(err)
	}

	reloaded, err := tree.LoadFromStore(ctx, NewStore(pool, conv, nil), root.ID, "")
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	tip, err := reloaded.Tip("main")
	if err != nil || tip.ID != result.ID {
		t.Fatalf("reloaded main tip = %v, %v; want the tool result %s", tip, err, result.ID)
	}
	if reloaded.Active() != side {
		t.Errorf("reloaded active = %s, want %s", reloaded.Active(), side)
	}
}
