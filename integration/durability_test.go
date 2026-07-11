package integration

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	agentpgstore "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// TestAgentDurabilityRoundTrip proves the durability layers compose end to
// end: an agent writes its conversation through a file WAL (tree side) and a
// PostgreSQL store (persistNode side); a simulated fresh process then heals
// the store from the WAL and rehydrates the tree via RecoverAndLoadTree.
//
// The checkpoint is the critical cross-layer artifact: tree.Checkpoint only
// reaches the WAL, so it can only appear in the recovered tree if WAL
// recovery replayed it into the pg store — exactly the crash window
// RecoverAndLoadTree exists to heal.
func TestAgentDurabilityRoundTrip(t *testing.T) {
	pool := requirePostgres(t)
	truncate(t, pool, "agent_node", "agent_branch", "agent_checkpoint")
	ctx := testContext(t, 2*time.Minute)

	walPath := filepath.Join(t.TempDir(), "tree.wal")

	// ── Process 1: run a conversation, checkpoint it ──────────────────
	wal1, err := filewal.New(walPath)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	tr, err := tree.New(types.NewSystemMessage("You are a durable test agent."), tree.WithWAL(wal1))
	if err != nil {
		t.Fatalf("new tree: %v", err)
	}
	rootID := tr.Root().ID
	convID := string(rootID)
	store1 := agentpgstore.NewStore(pool, convID, nil)

	tool, calls := addTool()
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("call-1", "add", map[string]any{"a": float64(2), "b": float64(3)}),
		agenttest.TextResponse("first-turn answer: 5"),
	}}
	ag := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:     "durable",
		Provider: provider,
		Tools:    types.NewToolRegistry(tool),
		Tree:     tr,
		Store:    store1,
	})

	text, _, err := drainStream(ag.Invoke(ctx, []types.Message{types.NewUserMessage("What is 2 + 3?")}))
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("add tool executed %d times, want 1", *calls)
	}
	if !strings.Contains(text, "first-turn answer") {
		t.Fatalf("first turn text = %q, want scripted answer", text)
	}

	cpID, err := tr.Checkpoint("main", "turn-1")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := wal1.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}

	// ── Process 2 (simulated fresh process): recover + rehydrate ──────
	wal2, err := filewal.New(walPath)
	if err != nil {
		t.Fatalf("reopen wal: %v", err)
	}
	t.Cleanup(func() { _ = wal2.Close() })
	store2 := agentpgstore.NewStore(pool, convID, nil)

	recovered, err := agentsdk.RecoverAndLoadTree(ctx, wal2, store2, rootID, "")
	if err != nil {
		t.Fatalf("recover and load tree: %v", err)
	}

	// The recovered tree contains the full conversation:
	// system, user, assistant(tool call), tool result, assistant(text).
	msgs, err := recovered.FlattenBranch("main")
	if err != nil {
		t.Fatalf("flatten recovered main: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("recovered main has %d messages, want 5: %#v", len(msgs), msgs)
	}
	last, ok := msgs[len(msgs)-1].(types.AssistantMessage)
	if !ok {
		t.Fatalf("last recovered message is %T, want AssistantMessage", msgs[len(msgs)-1])
	}
	if got := assistantText(&last); !strings.Contains(got, "first-turn answer") {
		t.Errorf("recovered final assistant text = %q, want first-turn answer", got)
	}

	// Branches survived.
	branches := recovered.Branches()
	if _, ok := branches["main"]; !ok {
		t.Fatalf("recovered branches %v missing main", branches)
	}

	// The checkpoint survived — it only reaches the store via WAL recovery.
	cps := recovered.Checkpoints()
	cp, ok := cps[cpID]
	if !ok {
		t.Fatalf("recovered checkpoints %v missing %s (WAL recovery did not replay it)", cps, cpID)
	}
	if cp.Name != "turn-1" || cp.Branch != "main" {
		t.Errorf("recovered checkpoint = %+v, want name turn-1 on main", cp)
	}
	if cp.NodeID != branches["main"] {
		t.Errorf("checkpoint node %s != main tip %s", cp.NodeID, branches["main"])
	}

	// Rewind from the recovered checkpoint works: the rewound branch points at
	// the checkpoint node and flattens to the full first-turn history.
	rewound, err := recovered.Rewind(cpID)
	if err != nil {
		t.Fatalf("rewind from recovered checkpoint: %v", err)
	}
	tip, err := recovered.Tip(rewound)
	if err != nil {
		t.Fatalf("tip of rewound branch: %v", err)
	}
	if tip.ID != cp.NodeID {
		t.Errorf("rewound tip = %s, want checkpoint node %s", tip.ID, cp.NodeID)
	}
	rewoundMsgs, err := recovered.FlattenBranch(rewound)
	if err != nil {
		t.Fatalf("flatten rewound branch: %v", err)
	}
	if len(rewoundMsgs) != 5 {
		t.Fatalf("rewound branch has %d messages, want 5", len(rewoundMsgs))
	}

	// Invoking the agent again on the recovered tree (active branch "main")
	// continues the conversation with the scripted second response.
	provider2 := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("second-turn answer: continuation-ok"),
	}}
	ag2 := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:     "durable",
		Provider: provider2,
		Tree:     recovered,
		Store:    store2,
	})
	text2, _, err := drainStream(ag2.Invoke(ctx, []types.Message{types.NewUserMessage("And what did you just compute?")}))
	if err != nil {
		t.Fatalf("second invoke on recovered tree: %v", err)
	}
	if !strings.Contains(text2, "continuation-ok") {
		t.Errorf("second turn text = %q, want scripted continuation", text2)
	}

	msgs2, err := recovered.FlattenBranch("main")
	if err != nil {
		t.Fatalf("flatten main after continuation: %v", err)
	}
	if len(msgs2) != 7 {
		t.Fatalf("main has %d messages after continuation, want 7", len(msgs2))
	}

	// The continuation was persisted too: a third hydration from the store
	// alone sees both turns.
	tree3, err := agentsdk.LoadTreeFromStore(ctx, store2, rootID, "main")
	if err != nil {
		t.Fatalf("third load from store: %v", err)
	}
	msgs3, err := tree3.FlattenBranch("main")
	if err != nil {
		t.Fatalf("flatten main from third load: %v", err)
	}
	if len(msgs3) != 7 {
		t.Fatalf("store-only reload of main has %d messages, want 7", len(msgs3))
	}
	var sawFirst, sawSecond bool
	for _, m := range msgs3 {
		if am, ok := m.(types.AssistantMessage); ok {
			txt := assistantText(&am)
			sawFirst = sawFirst || strings.Contains(txt, "first-turn answer")
			sawSecond = sawSecond || strings.Contains(txt, "continuation-ok")
		}
	}
	if !sawFirst || !sawSecond {
		t.Errorf("reloaded branch missing turns: first=%v second=%v", sawFirst, sawSecond)
	}
}
