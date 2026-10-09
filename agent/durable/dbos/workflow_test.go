package dbos

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// testEngine starts a DBOS engine on a fresh database of the server named by
// SAIGE_TEST_POSTGRES_DSN. register runs before Launch. Tests are skipped
// when the variable is unset.
func testEngine(t *testing.T, register func(*Engine)) *Engine {
	t.Helper()
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN not set; skipping DBOS workflow test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "saige_dbos_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	engine, err := NewEngine(ctx, "saige-dbos-test", nil, u.String())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	register(engine)
	if err := engine.Launch(); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { engine.Shutdown(10 * time.Second) })
	return engine
}

// echoProvider answers with the text of the last user message.
type echoProvider struct{}

func (echoProvider) ChatStream(_ context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	var last string
	for _, m := range msgs {
		if u, ok := m.(types.UserMessage); ok {
			for _, c := range u.Content {
				if tc, ok := c.(types.TextContent); ok {
					last = tc.Text
				}
			}
		}
	}
	ch := make(chan types.Delta, 8)
	for _, d := range agenttest.TextResponse("echo: " + last) {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func finalText(out RunOutput) string {
	if out.Final == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range out.Final.Content {
		if tc, ok := c.(types.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestFactoryIsolatesConcurrentWorkflows runs two workflows at once. Each
// gets its own agent, tree and budget: with a shared agent the outputs would
// mix and both runs would spend from one budget.
func TestFactoryIsolatesConcurrentWorkflows(t *testing.T) {
	var mu sync.Mutex
	built := map[string]*agent.Agent{}
	var wf dbos.Workflow[RunInput, RunOutput]
	engine := testEngine(t, func(e *Engine) {
		wf = e.RegisterAgentFactory(func(workflowID string) *agent.Agent {
			a := agent.NewAgent(agent.AgentConfig{
				Provider:     echoProvider{},
				SystemPrompt: "echo",
				Budget:       types.NewBudget(types.BudgetPolicy{MaxRequests: 2}),
			})
			mu.Lock()
			built[workflowID] = a
			mu.Unlock()
			return a
		}, "saige.test.isolated")
	})

	inputs := map[string]string{"wf-a-" + rand.Text()[:8]: "alpha", "wf-b-" + rand.Text()[:8]: "beta"}
	handles := map[string]dbos.WorkflowHandle[RunOutput]{}
	for id, text := range inputs {
		h, err := engine.Run(wf, RunInput{Messages: []types.Message{types.NewUserMessage(text)}}, id)
		if err != nil {
			t.Fatal(err)
		}
		handles[id] = h
	}
	for id, h := range handles {
		out, err := h.GetResult()
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got, want := finalText(out), "echo: "+inputs[id]; got != want {
			t.Errorf("%s final = %q, want %q", id, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(built) != 2 {
		t.Fatalf("factory built %d agents, want one per workflow", len(built))
	}
	for id, a := range built {
		if msgs, _ := a.Tree().FlattenBranch(a.Tree().Active()); len(msgs) != 3 {
			t.Errorf("%s tree has %d messages, want system, user and reply only", id, len(msgs))
		}
		if got := a.Budget().Usage().Requests; got != 1 {
			t.Errorf("%s budget counted %d requests, want 1", id, got)
		}
	}
}

// blockingProvider blocks until its context is cancelled and records that it
// observed the cancellation.
type blockingProvider struct {
	started   chan struct{}
	cancelled atomic.Bool
}

func (p *blockingProvider) ChatStream(ctx context.Context, _ []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	close(p.started)
	<-ctx.Done()
	p.cancelled.Store(true)
	return nil, ctx.Err()
}

// TestWorkflowTimeoutReachesProvider checks that the workflow context is the
// run context: a workflow timeout cancels the in-flight provider call.
func TestWorkflowTimeoutReachesProvider(t *testing.T) {
	p := &blockingProvider{started: make(chan struct{})}
	var wf dbos.Workflow[RunInput, RunOutput]
	engine := testEngine(t, func(e *Engine) {
		wf = e.RegisterAgentFactory(func(string) *agent.Agent {
			return agent.NewAgent(agent.AgentConfig{Provider: p, SystemPrompt: "s"})
		}, "saige.test.cancel")
	})
	timed, cancel := dbos.WithTimeout(engine.Context(), 2*time.Second)
	defer cancel()
	in := RunInput{Messages: []types.Message{types.NewUserMessage("hi")}}
	if _, err := dbos.RunWorkflow(timed, wf, in, dbos.WithWorkflowID("wf-timeout-"+rand.Text()[:8])); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(20 * time.Second):
		t.Fatal("provider never called")
	}
	deadline := time.Now().Add(20 * time.Second)
	for !p.cancelled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("provider context was not cancelled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPendingApprovalWithoutRequest checks that a run that never asks for
// approval reads as not waiting, with no error, so hosts can poll.
func TestPendingApprovalWithoutRequest(t *testing.T) {
	var wf dbos.Workflow[RunInput, RunOutput]
	engine := testEngine(t, func(e *Engine) {
		wf = e.RegisterAgentFactory(func(string) *agent.Agent {
			return agent.NewAgent(agent.AgentConfig{
				Provider:     &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}},
				SystemPrompt: "s",
			})
		}, "saige.test.noapproval")
	})
	id := "wf-noapproval-" + rand.Text()[:8]
	h, err := engine.Run(wf, RunInput{Messages: []types.Message{types.NewUserMessage("hi")}}, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetResult(); err != nil {
		t.Fatal(err)
	}
	req, ok, err := engine.PendingApproval(id, 100*time.Millisecond)
	if err != nil || ok {
		t.Fatalf("PendingApproval = %+v, %v, %v; want not waiting with no error", req, ok, err)
	}
}

// TestApprovalRoundTrip suspends a marked tool until a decision arrives from
// another goroutine, then completes with the approved arguments.
func TestApprovalRoundTrip(t *testing.T) {
	var gotArgs atomic.Value
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(_ context.Context, args map[string]any) (string, error) {
		gotArgs.Store(fmt.Sprint(args["value"]))
		return "written", nil
	}}
	var wf dbos.Workflow[RunInput, RunOutput]
	engine := testEngine(t, func(e *Engine) {
		e.ApprovalTimeout = 30 * time.Second
		wf = e.RegisterAgentFactory(func(string) *agent.Agent {
			return agent.NewAgent(agent.AgentConfig{
				Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
					agenttest.ToolCallResponse("call-1", "write", map[string]any{"value": "original"}),
					agenttest.TextResponse("done"),
				}},
				SystemPrompt: "s",
				Tools:        types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"})),
			})
		}, "saige.test.approval")
	})
	id := "wf-approval-" + rand.Text()[:8]
	h, err := engine.Run(wf, RunInput{Messages: []types.Message{types.NewUserMessage("go")}}, id)
	if err != nil {
		t.Fatal(err)
	}

	var req types.ApprovalRequest
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, ok, err := engine.PendingApproval(id, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			req = r
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never published an approval request")
		}
	}
	if req.ToolCall.Name != "write" {
		t.Fatalf("pending request = %+v", req)
	}
	go func() {
		_ = engine.Decide(id, req.ID, types.ApprovalDecision{Approved: true, ModifiedArgs: map[string]any{"value": "approved"}})
	}()

	out, err := h.GetResult()
	if err != nil {
		t.Fatal(err)
	}
	if finalText(out) != "done" {
		t.Errorf("final = %q, want done", finalText(out))
	}
	if gotArgs.Load() != "approved" {
		t.Errorf("tool ran with %v, want the approved arguments", gotArgs.Load())
	}
	if _, ok, _ := engine.PendingApproval(id, 100*time.Millisecond); ok {
		t.Error("pending approval still published after the decision")
	}
}

// TestApprovalTimeoutFailsClosed checks that a run with no decision fails
// instead of running the tool.
func TestApprovalTimeoutFailsClosed(t *testing.T) {
	var ran atomic.Bool
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) {
		ran.Store(true)
		return "written", nil
	}}
	var wf dbos.Workflow[RunInput, RunOutput]
	engine := testEngine(t, func(e *Engine) {
		e.ApprovalTimeout = 500 * time.Millisecond
		wf = e.RegisterAgentFactory(func(string) *agent.Agent {
			return agent.NewAgent(agent.AgentConfig{
				Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
					agenttest.ToolCallResponse("call-1", "write", nil),
					agenttest.TextResponse("done"),
				}},
				SystemPrompt: "s",
				Tools:        types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"})),
			})
		}, "saige.test.approval-timeout")
	})
	h, err := engine.Run(wf, RunInput{Messages: []types.Message{types.NewUserMessage("go")}}, "wf-timeout-"+rand.Text()[:8])
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.GetResult()
	if err == nil || !strings.Contains(err.Error(), ErrApprovalExpired.Error()) {
		t.Fatalf("result error = %v, want %v", err, ErrApprovalExpired)
	}
	if ran.Load() {
		t.Error("tool ran without approval")
	}
}
