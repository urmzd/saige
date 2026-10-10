package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urmzd/saige/agent/agenttest"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/cmd/internal/approvals"
)

// heldSession serves at with held approvals in a temporary store, to a
// client without elicitation.
func heldSession(t *testing.T, at agentTool, timeout time.Duration) (*mcp.ClientSession, *approvals.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	held := newHeldApprovals(ctx, t.TempDir(), timeout, 200*time.Millisecond)
	held.poll = 10 * time.Millisecond
	t.Cleanup(func() { held.close(); cancel() })
	cs := agentSession(t, bridge{approval: approvalElicit, held: held}, at, nil)
	store, err := held.openStore()
	if err != nil {
		t.Fatal(err)
	}
	return cs, store
}

func callResume(t *testing.T, cs *mcp.ClientSession, token string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: defaultAgentTool + resumeSuffix, Arguments: map[string]any{"token": token}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func heldToken(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	obj, _ := res.StructuredContent.(map[string]any)
	token, _ := obj["token"].(string)
	if res.IsError || obj["status"] != "approval_required" || approvals.CheckToken(token) != nil {
		t.Fatalf("not an approval-required result: %q %v", resultText(res), res.StructuredContent)
	}
	text := resultText(res)
	for _, want := range []string{"saige approvals approve " + token, "saige approvals deny " + token, defaultAgentTool + resumeSuffix, "store"} {
		if !strings.Contains(text, want) {
			t.Fatalf("result text lacks %q:\n%s", want, text)
		}
	}
	return token
}

func TestHeldApprovalRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		decision approvals.Decision
		wantRun  bool
		wantMsg  string
	}{
		{"approved", approvals.Decision{Approved: true}, true, "stored"},
		{"denied", approvals.Decision{Approved: false, Message: "not today"}, false, "not today"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runs atomic.Int64
			p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{
				agenttest.ToolCallResponse("c1", "store", map[string]any{"text": "x"}),
				agenttest.TextResponse("done"),
			}}
			cs, store := heldSession(t, scriptedAgent(p, storeTool(&runs)), time.Minute)
			token := heldToken(t, callAgent(t, cs, "store x"))
			if runs.Load() != 0 {
				t.Fatal("the tool ran before a decision")
			}
			entries, err := store.List()
			if err != nil || len(entries) != 1 || entries[0].Tool != "store" || entries[0].Arguments["text"] != "x" {
				t.Fatalf("store = %+v %v", entries, err)
			}

			// Resuming before a decision reports it still pending.
			res := callResume(t, cs, token)
			if obj, _ := res.StructuredContent.(map[string]any); obj["status"] != "approval_pending" {
				t.Fatalf("early resume = %q", resultText(res))
			}

			if err := store.Decide(token, tt.decision); err != nil {
				t.Fatal(err)
			}
			res = callResume(t, cs, token)
			if res.IsError || resultText(res) != "done" {
				t.Fatalf("resume = %q", resultText(res))
			}
			if ran := runs.Load() == 1; ran != tt.wantRun {
				t.Fatalf("ran = %v, want %v", ran, tt.wantRun)
			}
			calls := p.Requests()
			if !strings.Contains(toolResultText(calls[len(calls)-1].Messages), tt.wantMsg) {
				t.Fatalf("model saw %q, want %q", toolResultText(calls[len(calls)-1].Messages), tt.wantMsg)
			}
			// The record is gone, and the token resumes nothing.
			if _, err := store.Get(token); !errors.Is(err, approvals.ErrNotFound) {
				t.Fatalf("record kept: %v", err)
			}
			if res := callResume(t, cs, token); !res.IsError {
				t.Fatalf("second resume = %q", resultText(res))
			}
		})
	}
}

func TestHeldApprovalGrantCoversLaterCalls(t *testing.T) {
	var runs atomic.Int64
	p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{
		agenttest.ToolCallResponse("c1", "store", map[string]any{"text": "a"}),
		agenttest.ToolCallResponse("c2", "store", map[string]any{"text": "b"}),
		agenttest.TextResponse("done"),
	}}
	cs, store := heldSession(t, scriptedAgent(p, storeTool(&runs)), time.Minute)
	token := heldToken(t, callAgent(t, cs, "store twice"))
	if err := store.Decide(token, approvals.Decision{Approved: true, Grant: &agenttypes.GrantRequest{Scope: agenttypes.GrantTool}}); err != nil {
		t.Fatal(err)
	}
	if res := callResume(t, cs, token); res.IsError || resultText(res) != "done" {
		t.Fatalf("resume = %q", resultText(res))
	}
	if runs.Load() != 2 {
		t.Fatalf("runs = %d, want 2: the grant should cover the second call", runs.Load())
	}
}

func TestHeldApprovalGrantCap(t *testing.T) {
	var runs atomic.Int64
	at := scriptedAgent(&agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{
		agenttest.ToolCallResponse("c1", "store", map[string]any{}),
		agenttest.TextResponse("done"),
	}}, storeTool(&runs))
	inner := at.newAgent
	at.newAgent = nil
	at.newSession = func(context.Context) (agenthost.Agent, error) {
		return agenthost.Agent{Agent: inner(), MaxGrant: agenttypes.GrantOnce}, nil
	}
	cs, store := heldSession(t, at, time.Minute)
	token := heldToken(t, callAgent(t, cs, "store"))
	err := store.Decide(token, approvals.Decision{Approved: true, Grant: &agenttypes.GrantRequest{Scope: agenttypes.GrantSession}})
	if !errors.Is(err, agenttypes.ErrInvalidGrant) {
		t.Fatalf("grant beyond the cap: %v", err)
	}
}

func TestHeldApprovalExpires(t *testing.T) {
	var runs atomic.Int64
	at := scriptedAgent(&agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{
		agenttest.ToolCallResponse("c1", "store", map[string]any{}),
		agenttest.TextResponse("done"),
	}}, storeTool(&runs))
	cs, store := heldSession(t, at, 50*time.Millisecond)
	token := heldToken(t, callAgent(t, cs, "store"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := store.Get(token); errors.Is(err, approvals.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the expired record was kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res := callResume(t, cs, token); !res.IsError || !strings.Contains(resultText(res), "expired") {
		t.Fatalf("resume after expiry = %q", resultText(res))
	}
	if runs.Load() != 0 {
		t.Fatal("an expired approval ran the tool")
	}
}

func TestHeldApprovalsWriteNothingUntilUsed(t *testing.T) {
	dir := t.TempDir() + "/approvals"
	held := newHeldApprovals(context.Background(), dir, time.Minute, time.Second)
	p := &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{agenttest.TextResponse("Paris")}}
	cs := agentSession(t, bridge{approval: approvalElicit, held: held}, scriptedAgent(p), nil)
	if res := callAgent(t, cs, "capital of France"); resultText(res) != "Paris" {
		t.Fatalf("result = %q", resultText(res))
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store created without a held approval: %v", err)
	}
	// An agent that needs no approval publishes no resume tool.
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) != 1 {
		t.Fatalf("tools = %v %v", list, err)
	}
}
