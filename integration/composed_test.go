package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/fallback"
	"github.com/urmzd/saige/agent/types"
)

// These tests need no external services: they prove that three agent-core
// hardening fixes compose correctly at the parent/sub-agent/fallback level:
//
//  1. a mid-stream provider ErrorDelta fails the turn (it is not swallowed
//     into a "successful" empty response);
//  2. a failed sub-agent fails the delegation — the parent tool result
//     records the error instead of presenting partial output as success;
//  3. fallback.Provider retries the next provider on a mid-stream error that
//     arrives before any content was forwarded.

// errorProvider returns a scripted provider whose only output is a mid-stream
// ErrorDelta (emitted before any content, so fallback is allowed to recover).
func errorProvider(msg string) *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{types.ErrorDelta{Error: errors.New(msg)}},
	}}
}

// delegatingParent builds a parent agent whose scripted provider first
// delegates to the "helper" sub-agent (backed by subProvider) and then closes
// with a final text turn.
func delegatingParent(subProvider types.Provider) *agentsdk.Agent {
	parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("call-1", "delegate_to_helper", map[string]any{"task": "do the thing"}),
		agenttest.TextResponse("parent-final: done"),
	}}
	return agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "parent",
		SystemPrompt: "You are the parent agent.",
		Provider:     parentProvider,
	}, agentsdk.WithSubAgents(agentsdk.SubAgentDef{
		Name:        "helper",
		Description: "Test helper sub-agent.",
		Provider:    subProvider,
	}))
}

// toolResults extracts every ToolResultContent persisted on the branch.
func toolResults(t *testing.T, ag *agentsdk.Agent) []types.ToolResultContent {
	t.Helper()
	msgs, err := ag.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatalf("flatten main: %v", err)
	}
	var out []types.ToolResultContent
	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Content {
				if trc, ok := c.(types.ToolResultContent); ok {
					out = append(out, trc)
				}
			}
		case types.UserMessage:
			for _, c := range v.Content {
				if trc, ok := c.(types.ToolResultContent); ok {
					out = append(out, trc)
				}
			}
		}
	}
	return out
}

func composedContext(t *testing.T) context.Context {
	return testContext(t, 30*time.Second)
}

// TestSubAgentFallbackRecoversMidStreamError: the sub-agent's first provider
// dies mid-stream, the fallback's second provider succeeds, and the parent
// run completes cleanly — Wait() is nil and the delegation result carries the
// recovered text.
func TestSubAgentFallbackRecoversMidStreamError(t *testing.T) {
	ctx := composedContext(t)

	subProvider := fallback.New(
		errorProvider("provider boom: simulated mid-stream 529"),
		&agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.TextResponse("recovered: sub-agent result"),
		}},
	)
	ag := delegatingParent(subProvider)

	stream := ag.Invoke(ctx, []types.Message{types.NewUserMessage("delegate please")})
	var text strings.Builder
	for d := range stream.Deltas() {
		if tc, ok := d.(types.TextContentDelta); ok {
			text.WriteString(tc.Content)
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil (fallback should have recovered the sub-agent)", err)
	}
	if !strings.Contains(text.String(), "parent-final") {
		t.Errorf("parent text = %q, want final turn", text.String())
	}

	results := toolResults(t, ag)
	if len(results) != 1 {
		t.Fatalf("persisted tool results = %d, want 1", len(results))
	}
	if results[0].IsError {
		t.Errorf("delegation recorded as error: %q — fallback recovery did not compose", results[0].Text)
	}
	if !strings.Contains(results[0].Text, "recovered: sub-agent result") {
		t.Errorf("delegation result = %q, want recovered sub-agent text", results[0].Text)
	}
}

// TestSubAgentMidStreamErrorFailsDelegation: with no second provider to fall
// back to, the sub-agent's mid-stream error fails its run (Wait() non-nil on
// the failing agent) and the parent's persisted tool result records the error
// instead of a fake success.
func TestSubAgentMidStreamErrorFailsDelegation(t *testing.T) {
	ctx := composedContext(t)

	// Direct invocation first: a mid-stream ErrorDelta must surface through
	// Wait() even when wrapped in a fallback with no remaining providers.
	failing := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "doomed",
		SystemPrompt: "You will fail.",
		Provider:     fallback.New(errorProvider("provider boom: simulated mid-stream 529")),
	})
	stream := failing.Invoke(ctx, []types.Message{types.NewUserMessage("hi")})
	sawErrorDelta := false
	for d := range stream.Deltas() {
		if _, ok := d.(types.ErrorDelta); ok {
			sawErrorDelta = true
		}
	}
	err := stream.Wait()
	if err == nil {
		t.Fatal("Wait() = nil, want the mid-stream provider error to fail the run")
	}
	var fbErr *types.FallbackError
	if !errors.As(err, &fbErr) {
		t.Fatalf("Wait() error %v does not unwrap to *types.FallbackError", err)
	}
	if len(fbErr.Errors) != 1 || !strings.Contains(fbErr.Errors[0].Error(), "boom") {
		t.Errorf("FallbackError.Errors = %v, want the original mid-stream provider error", fbErr.Errors)
	}
	if !sawErrorDelta {
		t.Error("no ErrorDelta observed on the failing stream")
	}

	// Composed: the same failing provider behind a sub-agent. The parent run
	// itself completes (a failed delegation is a tool error, not a crash), but
	// the persisted tool result must record the failure.
	ag := delegatingParent(fallback.New(errorProvider("provider boom: simulated mid-stream 529")))
	pstream := ag.Invoke(ctx, []types.Message{types.NewUserMessage("delegate please")})
	var delegationErr string
	for d := range pstream.Deltas() {
		if te, ok := d.(types.ToolExecEndDelta); ok && te.ToolCallID == "call-1" {
			delegationErr = te.Error
		}
	}
	if err := pstream.Wait(); err != nil {
		t.Fatalf("parent Wait() = %v, want nil (delegation failure is recorded, not fatal)", err)
	}
	if delegationErr == "" || !strings.Contains(delegationErr, "providers failed") {
		t.Errorf("ToolExecEndDelta error = %q, want the sub-agent's fallback failure", delegationErr)
	}

	results := toolResults(t, ag)
	if len(results) != 1 {
		t.Fatalf("persisted tool results = %d, want 1", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("delegation tool result = %+v, want IsError=true", results[0])
	}
	if !strings.Contains(results[0].Text, "providers failed") {
		t.Errorf("delegation tool result text = %q, want the sub-agent's fallback failure", results[0].Text)
	}
}
