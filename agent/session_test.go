package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestSaveAndLoadSession(t *testing.T) {
	provider := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{
			agenttest.TextResponse("Hello back!"),
		},
	}

	a := NewAgent(AgentConfig{
		Name:         "test-agent",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
	})

	// Invoke to build some conversation history
	stream := a.Invoke(context.Background(), []types.Message{
		types.UserMsg(types.Text("Hello")),
	})
	for range stream.Deltas() {
	}

	// Save session
	session, err := a.SaveSession()
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	if session.ID == "" {
		t.Error("session ID should not be empty")
	}
	if session.TreeData == nil {
		t.Error("TreeData should not be nil")
	}

	// Create new agent and load session
	a2 := NewAgent(AgentConfig{
		Name:         "test-agent",
		SystemPrompt: "You are helpful.",
		Provider:     provider,
	})

	if err := a2.LoadSession(session); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	// Verify tree state matches
	branch := a2.Tree().Active()
	messages, err := a2.Tree().FlattenBranch(branch)
	if err != nil {
		t.Fatalf("FlattenBranch: %v", err)
	}

	// Should have: system, user("Hello"), assistant("Hello back!")
	if len(messages) < 3 {
		t.Errorf("expected at least 3 messages, got %d", len(messages))
	}
}

func TestSessionFileRoundTrip(t *testing.T) {
	a := NewAgent(AgentConfig{
		Name:         "test-agent",
		SystemPrompt: "You are helpful.",
		Provider: &agenttest.ScriptedProvider{
			Responses: [][]types.Delta{
				agenttest.TextResponse("Hi!"),
			},
		},
	})

	session, err := a.SaveSession()
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	if err := SaveSessionToFile(session, path); err != nil {
		t.Fatalf("SaveSessionToFile: %v", err)
	}

	loaded, err := LoadSessionFromFile(path)
	if err != nil {
		t.Fatalf("LoadSessionFromFile: %v", err)
	}

	if loaded.ID != session.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, session.ID)
	}
}

func TestLoadSessionFromFileNotFound(t *testing.T) {
	_, err := LoadSessionFromFile("/nonexistent/path.json")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoadSessionInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := LoadSessionFromFile(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}
