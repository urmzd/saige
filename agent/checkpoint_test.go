package agent

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/types"
)

// Agent.Checkpoint must persist through the Store so a store-only reload can
// rewind: no WAL required.
func TestAgentCheckpointRoundTripsThroughStore(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("first answer"),
	}}

	ag := NewAgent(AgentConfig{
		Name:         "cp-agent",
		SystemPrompt: "sys",
		Provider:     provider,
	}, WithStore(store))

	stream := ag.Invoke(ctx, []types.Message{types.NewUserMessage("hi")})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() = %v", err)
	}

	cpID, err := ag.Checkpoint(ctx, "", "after-turn-1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	rootID := ag.Tree().Root().ID
	loaded, err := LoadTreeFromStore(ctx, store, rootID, "")
	if err != nil {
		t.Fatalf("LoadTreeFromStore: %v", err)
	}

	cps := loaded.Checkpoints()
	cp, ok := cps[cpID]
	if !ok {
		t.Fatalf("checkpoint %s not restored; got %d checkpoints", cpID, len(cps))
	}
	if cp.Name != "after-turn-1" {
		t.Errorf("checkpoint name = %q, want after-turn-1", cp.Name)
	}

	branch, err := loaded.Rewind(cpID)
	if err != nil {
		t.Fatalf("Rewind on restored tree: %v", err)
	}
	if _, err := loaded.Tip(branch); err != nil {
		t.Fatalf("rewound branch tip: %v", err)
	}
}

// A store failure must not lose the in-tree checkpoint, and must be reported.
func TestAgentCheckpointStoreFailureReported(t *testing.T) {
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("ok"),
	}}
	ag := NewAgent(AgentConfig{
		Name:         "cp-agent",
		SystemPrompt: "sys",
		Provider:     provider,
	}, WithStore(failingCheckpointStore{Store: memstore.New()}))

	cpID, err := ag.Checkpoint(context.Background(), "", "save")
	if err == nil {
		t.Fatal("Checkpoint with failing store = nil error, want error")
	}
	if cpID == "" {
		t.Fatal("checkpoint should still exist in the tree when only the store write fails")
	}
	if _, ok := ag.Tree().Checkpoints()[cpID]; !ok {
		t.Error("checkpoint missing from tree after store failure")
	}
}

type failingCheckpointStore struct {
	types.Store
}

func (failingCheckpointStore) SaveCheckpoint(context.Context, types.Checkpoint) error {
	return context.DeadlineExceeded
}
