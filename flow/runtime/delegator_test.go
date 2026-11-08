package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// scripted gives each agent its own canned model, keyed by the agent's name,
// so a turn can call a tool and then answer without a model being called.
func scripted(scripts map[string][][]saigetypes.Delta) native.Option {
	var mu sync.Mutex
	return native.WithProvider(native.ProviderOffline, func(spec native.Spec) (saigetypes.Provider, error) {
		mu.Lock()
		defer mu.Unlock()
		return &agenttest.ScriptedProvider{Responses: scripts[spec.Name]}, nil
	})
}

// delegating builds a real sandbox with a lead that may tag a helper. The
// lead's model tags the helper once and then repeats what came back.
func delegating(t *testing.T, opts ...runtime.SandboxOption) (*runtime.Sandbox, native.Revision) {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("helper", "Help.")
	history.revise("lead", "Lead.")
	lead := history.edit("lead", func(s *native.Spec) { s.Access.Spawn = []string{"helper"} })

	opts = append([]runtime.SandboxOption{runtime.WithAgentOptions(scripted(map[string][][]saigetypes.Delta{
		"lead": {
			agenttest.ToolCallResponse("call-1", runtime.DelegateToolName("helper"), map[string]any{"text": "Count the failing tests"}),
			agenttest.TextResponse("The helper finished."),
		},
		"helper": {agenttest.TextResponse("Three tests fail.")},
	}))}, opts...)
	s, err := runtime.NewSandbox(plane, tools, history, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s, lead
}

func runLead(t *testing.T, s *runtime.Sandbox, lead native.Revision) (runtime.Outcome, []event.Body, error) {
	t.Helper()
	var chunks []event.Body
	out, err := s.Execute(context.Background(), withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Attempt: 1, Text: "How are the tests?"}, lead),
		func(_ context.Context, c event.Body) error {
			chunks = append(chunks, c)
			return nil
		})
	return out, chunks, err
}

func toolResult(t *testing.T, chunks []event.Body) event.ToolDone {
	t.Helper()
	for _, c := range chunks {
		if done, ok := c.(event.ToolDone); ok {
			return done
		}
	}
	t.Fatalf("no tool result among %d chunks", len(chunks))
	return event.ToolDone{}
}

// With no Delegator a delegation is what it always was: a sub-task of the
// turn, whose answer is the tool's result. One node is enough: the sub-task
// runs on the node the waiting turn holds instead of queueing behind it.
func TestDelegationIsASubTaskByDefault(t *testing.T) {
	s, lead := delegating(t, runtime.WithReplicas(1))
	type result struct {
		out    runtime.Outcome
		chunks []event.Body
		err    error
	}
	done := make(chan result, 1)
	go func() {
		out, chunks, err := runLead(t, s, lead)
		done <- result{out, chunks, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a delegation on one node never finished: the sub-task is waiting behind the turn that waits for it")
	}
	out, chunks, err := r.out, r.chunks, r.err
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "The helper finished." {
		t.Fatalf("answer = %q", out.Text)
	}
	if done := toolResult(t, chunks); done.Result != "Three tests fail." || done.Error != "" {
		t.Fatalf("tool result = %+v", done)
	}
}

// A Delegator takes the delegation over for a turn that is a run: it is told
// who asked, of whom and what, its answer is what the model reads, and what it
// reports lands on the parent's stream.
func TestDelegatorCarriesOutADelegation(t *testing.T) {
	var got struct {
		sync.Mutex
		calls        int
		run, tenant  string
		target, text string
	}
	s, lead := delegating(t, runtime.WithDelegator(func(_ context.Context, parent runtime.Request, target, text string, report func(event.Action) error) (string, error) {
		got.Lock()
		defer got.Unlock()
		got.calls++
		got.run, got.tenant, got.target, got.text = parent.RunID, parent.Tenant, target, text
		if err := report(event.Action{Name: runtime.DelegateToolName(target), Address: "run:child-1"}); err != nil {
			return "", err
		}
		return "Answered by a run of its own.", nil
	}))
	out, chunks, err := runLead(t, s, lead)
	if err != nil {
		t.Fatal(err)
	}
	if got.calls != 1 || got.run != "run-1" || got.tenant != "acme" || got.target != "helper" || got.text != "Count the failing tests" {
		t.Fatalf("delegator was told %+v", &got)
	}
	if done := toolResult(t, chunks); done.Result != "Answered by a run of its own." {
		t.Fatalf("the model read %q, not the delegator's answer", done.Result)
	}
	if out.Text != "The helper finished." {
		t.Fatalf("answer = %q", out.Text)
	}
	reported := false
	for _, c := range chunks {
		if a, ok := c.(event.Action); ok && strings.Contains(a.Summary+a.Target+a.Address+string(a.Data), "run:child-1") {
			reported = true
		}
	}
	if !reported {
		t.Fatal("what the delegator reported did not reach the parent's stream")
	}
}

// A refusal from the Delegator is the tool failing, which the model can read
// and answer around. It does not end the turn.
func TestDelegatorRefusalIsAToolError(t *testing.T) {
	s, lead := delegating(t, runtime.WithDelegator(func(context.Context, runtime.Request, string, string, func(event.Action) error) (string, error) {
		return "", errors.New("the tree is at its depth limit")
	}))
	out, chunks, err := runLead(t, s, lead)
	if err != nil {
		t.Fatal(err)
	}
	if done := toolResult(t, chunks); !strings.Contains(done.Error+done.Result, "depth limit") {
		t.Fatalf("the refusal did not reach the model: %+v", done)
	}
	if out.Text != "The helper finished." {
		t.Fatalf("answer = %q", out.Text)
	}
}
