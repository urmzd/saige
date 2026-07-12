package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/types"
)

// messageText projects any message into a comparable text string, concatenating
// every text-bearing content block. It is good enough to assert that the
// flattened conversation round-trips through a Store.
func messageText(msg types.Message) string {
	var sb strings.Builder
	sb.WriteString(string(msg.Role()))
	sb.WriteString(":")
	switch m := msg.(type) {
	case types.SystemMessage:
		for _, c := range m.Content {
			if tc, ok := c.(types.TextContent); ok {
				sb.WriteString(tc.Text)
			}
			if trc, ok := c.(types.ToolResultContent); ok {
				sb.WriteString("tool_result(")
				sb.WriteString(trc.Text)
				sb.WriteString(")")
			}
		}
	case types.UserMessage:
		for _, c := range m.Content {
			if tc, ok := c.(types.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
	case types.AssistantMessage:
		for _, c := range m.Content {
			if tc, ok := c.(types.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
	}
	return sb.String()
}

func flattenedText(t *testing.T, msgs []types.Message) []string {
	t.Helper()
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = messageText(m)
	}
	return out
}

// TestStoreMultiTurnRoundTrip builds an agent backed by a memstore, runs two
// Invoke turns, then reconstructs the tree purely from the Store (LoadTree +
// FromStore) and asserts the full message history round-trips byte-for-byte.
func TestStoreMultiTurnRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	provider := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{
			agenttest.TextResponse("hello there"),
			agenttest.TextResponse("second answer"),
		},
	}

	ag := NewAgent(AgentConfig{
		Name:         "store-agent",
		SystemPrompt: "you are helpful",
		Provider:     provider,
	}, WithStore(store))

	rootID := ag.Tree().Root().ID

	// Turn 1.
	stream := ag.Invoke(ctx, []types.Message{types.NewUserMessage("first question")})
	if err := stream.Wait(); err != nil {
		t.Fatalf("turn 1 invoke: %v", err)
	}
	// Turn 2.
	stream = ag.Invoke(ctx, []types.Message{types.NewUserMessage("second question")})
	if err := stream.Wait(); err != nil {
		t.Fatalf("turn 2 invoke: %v", err)
	}

	branch := ag.Tree().Active()
	liveMsgs, err := ag.Tree().FlattenBranch(branch)
	if err != nil {
		t.Fatalf("flatten live branch: %v", err)
	}
	if len(liveMsgs) < 5 {
		t.Fatalf("expected at least system+2*(user+assistant) messages, got %d", len(liveMsgs))
	}

	// Reconstruct the tree from the Store only.
	restored, err := LoadTreeFromStore(ctx, store, rootID, branch)
	if err != nil {
		t.Fatalf("LoadTreeFromStore: %v", err)
	}

	restoredMsgs, err := restored.FlattenBranch(branch)
	if err != nil {
		t.Fatalf("flatten restored branch: %v", err)
	}

	live := flattenedText(t, liveMsgs)
	got := flattenedText(t, restoredMsgs)
	if len(live) != len(got) {
		t.Fatalf("message count mismatch: live=%d restored=%d\nlive=%v\nrestored=%v",
			len(live), len(got), live, got)
	}
	for i := range live {
		if live[i] != got[i] {
			t.Fatalf("message[%d] mismatch:\n live=%q\n got =%q", i, live[i], got[i])
		}
	}

	// Spot-check the actual conversation made it through.
	joined := strings.Join(got, "|")
	for _, want := range []string{"first question", "hello there", "second question", "second answer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("restored history missing %q: %v", want, got)
		}
	}
}

// TestStoreNilIsBackwardCompatible verifies that with no Store configured the
// agent behaves exactly as before: Invoke works and the tree is populated.
func TestStoreNilIsBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	provider := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{agenttest.TextResponse("ok")},
	}
	ag := NewAgent(AgentConfig{
		Name:         "no-store",
		SystemPrompt: "sys",
		Provider:     provider,
	})
	if ag.cfg.Store != nil {
		t.Fatal("expected nil Store by default")
	}
	stream := ag.Invoke(ctx, []types.Message{types.NewUserMessage("hi")})
	if err := stream.Wait(); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	msgs, err := ag.Tree().FlattenBranch(ag.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("expected system+user+assistant, got %d", len(msgs))
	}
}

// TestStorePersistsRootOnConstruction verifies the root node + main branch are
// written to the Store as soon as the agent is constructed, so a reload anchor
// exists before the first Invoke.
func TestStorePersistsRootOnConstruction(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	ag := NewAgent(AgentConfig{
		Name:         "root-persist",
		SystemPrompt: "sys prompt",
		Provider:     &agenttest.ScriptedProvider{},
	}, WithStore(store))

	rootID := ag.Tree().Root().ID
	got, err := store.LoadNode(ctx, rootID)
	if err != nil {
		t.Fatalf("root not persisted on construction: %v", err)
	}
	if got.ID != rootID {
		t.Fatalf("persisted root mismatch: %s != %s", got.ID, rootID)
	}
	tip, err := store.LoadBranch(ctx, "main")
	if err != nil {
		t.Fatalf("main branch not persisted: %v", err)
	}
	if tip != rootID {
		t.Fatalf("main tip = %s, want root %s", tip, rootID)
	}
}
