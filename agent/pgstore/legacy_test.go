package pgstore

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// TestLegacyRowsMigrate loads node rows the release before typed parts
// wrote, reads them through the store, migrates them, and checks that the
// tree reads the same before and after.
func TestLegacyRowsMigrate(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	raw, err := os.ReadFile("../internal/testdata/legacy/pgstore/v1_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		RootID     types.NodeID    `json:"root_id"`
		Nodes      json.RawMessage `json:"agent_node"`
		Branches   json.RawMessage `json:"agent_branch"`
		Checkpoint json.RawMessage `json:"agent_checkpoint"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}
	const conv = "conv-golden-v1"
	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM agent_node WHERE conversation_id = $1`,
			`DELETE FROM agent_branch WHERE conversation_id = $1`,
			`DELETE FROM agent_checkpoint WHERE conversation_id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, conv); err != nil {
				t.Fatal(err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, q := range []struct {
		sql  string
		rows json.RawMessage
	}{
		{`INSERT INTO agent_node (uuid, parent_uuid, role, message, state, version, depth, branch_id, child_index, summary_of,
			created_at, updated_at, archived_at, archived_by, conversation_id)
		  SELECT uuid, parent_uuid, role, message, state, version, depth, branch_id, child_index, summary_of,
			created_at, updated_at, archived_at, archived_by, conversation_id
		  FROM jsonb_populate_recordset(NULL::agent_node, $1)`, dump.Nodes},
		{`INSERT INTO agent_branch (conversation_id, branch_id, tip_uuid)
		  SELECT conversation_id, branch_id, tip_uuid FROM jsonb_populate_recordset(NULL::agent_branch, $1)`, dump.Branches},
		{`INSERT INTO agent_checkpoint (conversation_id, uuid, branch_id, node_uuid, name, created_at)
		  SELECT conversation_id, uuid, branch_id, node_uuid, name, created_at FROM jsonb_populate_recordset(NULL::agent_checkpoint, $1)`, dump.Checkpoint},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.rows); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	store := NewStore(pool, conv, nil)
	load := func() *tree.Tree {
		t.Helper()
		tr, err := tree.LoadFromStore(ctx, store, dump.RootID, "")
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	before := load()
	nodes, err := before.FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 5 {
		t.Fatalf("main branch = %d nodes, want 5", len(nodes))
	}
	asst, ok := nodes[2].(types.AssistantMessage)
	if !ok {
		t.Fatalf("third message = %T", nodes[2])
	}
	if _, ok := asst.Parts[2].(types.ServerToolCallPart); !ok {
		t.Fatalf("server tool block = %T, want a call then its result", asst.Parts[2])
	}
	if _, ok := asst.Parts[3].(types.ServerToolResultPart); !ok {
		t.Fatalf("server tool block = %T, want a call then its result", asst.Parts[3])
	}

	dry, err := store.MigrateMessages(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Outdated != 7 || dry.Rewritten != 0 {
		t.Fatalf("dry run = %+v", dry)
	}
	res, err := store.MigrateMessages(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outdated != 7 || res.Rewritten != 7 {
		t.Fatalf("migration = %+v", res)
	}
	var v1Left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_node WHERE conversation_id = $1 AND message->>'v' IS DISTINCT FROM '2'`,
		conv).Scan(&v1Left); err != nil {
		t.Fatal(err)
	}
	if v1Left != 0 {
		t.Fatalf("%d rows still in the old format", v1Left)
	}
	again, err := store.MigrateMessages(ctx, false)
	if err != nil || again.Outdated != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}

	after := load()
	walk := func(tr *tree.Tree) map[types.NodeID]any {
		out := map[types.NodeID]any{}
		var visit func(id types.NodeID)
		visit = func(id types.NodeID) {
			n, err := tr.Node(id)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := tree.MarshalNode(n)
			if err != nil {
				t.Fatal(err)
			}
			var v any
			_ = json.Unmarshal(raw, &v)
			out[id] = v
			kids, err := tr.Children(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range kids {
				visit(k.ID)
			}
		}
		visit(dump.RootID)
		return out
	}
	if b, a := walk(before), walk(after); len(b) != 7 || !reflect.DeepEqual(a, b) {
		t.Fatalf("tree changed across the migration:\n%v\n%v", b, a)
	}
}
