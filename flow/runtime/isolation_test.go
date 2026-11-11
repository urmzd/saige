package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/isolation"
	"github.com/urmzd/saige/flow/isolation/isolationtest"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// leaseTool takes the turn's instance, then does what then says.
type leaseTool struct {
	then func(ctx context.Context) error
}

func (leaseTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "use_sandbox", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (l leaseTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	run, ok := isolation.RunFrom(ctx)
	if !ok {
		return "", errors.New("no run on the tool's context")
	}
	inst, err := run.Instance(ctx)
	if err != nil {
		return "", err
	}
	if l.then != nil {
		if err := l.then(ctx); err != nil {
			return "", err
		}
	}
	return inst.ID(), nil
}

type toolCatalog struct{ t saigetypes.Tool }

func (c toolCatalog) Lookup(name string) (saigetypes.Tool, error) {
	if name == c.t.Definition().Name {
		return c.t, nil
	}
	return nil, fmt.Errorf("%w: %q", native.ErrNoSuchTool, name)
}

func isolatedSandbox(t *testing.T, leaser isolation.Leaser, use leaseTool, script [][]saigetypes.Delta) (*runtime.Sandbox, native.Revision) {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("lead", "Lead.")
	lead := history.edit("lead", func(s *native.Spec) { s.Tools = []string{"use_sandbox"} })
	s, err := runtime.NewSandbox(plane, tools, history,
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) { return toolCatalog{use}, nil }),
		runtime.WithRunInstances(func(_ context.Context, req runtime.Request) *isolation.Run {
			return isolation.NewRun(leaser, isolation.Request{Tenant: req.Tenant, Run: req.RunID}, nil)
		}),
		runtime.WithAgentOptions(scripted(map[string][][]saigetypes.Delta{"lead": script})))
	if err != nil {
		t.Fatal(err)
	}
	return s, lead
}

// A turn's instance is released however the turn ends: with an answer, with
// a failure, or cancelled while a tool holds the instance.
func TestATurnReleasesItsInstanceHoweverItEnds(t *testing.T) {
	call := agenttest.ToolCallResponse("call-1", "use_sandbox", map[string]any{})

	t.Run("success", func(t *testing.T) {
		l := &isolationtest.Leaser{Dir: t.TempDir()}
		s, lead := isolatedSandbox(t, l, leaseTool{}, [][]saigetypes.Delta{call, agenttest.TextResponse("done")})
		out, chunks, err := runLead(t, s, lead)
		if err != nil || out.Text != "done" {
			t.Fatalf("turn = %q, %v", out.Text, err)
		}
		if done := toolResult(t, chunks); done.Error != "" {
			t.Fatalf("tool failed: %s", done.Error)
		}
		if len(l.Instances()) != 1 || len(l.Open()) != 0 {
			t.Fatalf("leased %d, open %d; want one, released", len(l.Instances()), len(l.Open()))
		}
		if got := l.Instances()[0].Request().Run; got != "run-1" {
			t.Fatalf("the instance was leased for run %q", got)
		}
	})

	t.Run("failure", func(t *testing.T) {
		l := &isolationtest.Leaser{Dir: t.TempDir()}
		// The model fails after the tool, so the turn does.
		s, lead := isolatedSandbox(t, l, leaseTool{}, [][]saigetypes.Delta{call, {saigetypes.ErrorDelta{Error: errors.New("model down")}}})
		if _, _, err := runLead(t, s, lead); err == nil {
			t.Fatal("the turn did not fail")
		}
		if len(l.Instances()) != 1 || len(l.Open()) != 0 {
			t.Fatalf("leased %d, open %d; want one, released", len(l.Instances()), len(l.Open()))
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		l := &isolationtest.Leaser{Dir: t.TempDir()}
		holding := make(chan struct{})
		use := leaseTool{then: func(ctx context.Context) error {
			close(holding)
			<-ctx.Done()
			return ctx.Err()
		}}
		s, lead := isolatedSandbox(t, l, use, [][]saigetypes.Delta{call, agenttest.TextResponse("done")})
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := s.Execute(ctx, withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Attempt: 1, Text: "go"}, lead), nil)
			errc <- err
		}()
		<-holding
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("turn = %v, want cancelled", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(l.Open()) != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(l.Instances()) != 1 || len(l.Open()) != 0 {
			t.Fatalf("leased %d, open %d; want one, released", len(l.Instances()), len(l.Open()))
		}
	})

	t.Run("no tool, no lease", func(t *testing.T) {
		l := &isolationtest.Leaser{Dir: t.TempDir()}
		s, lead := isolatedSandbox(t, l, leaseTool{}, [][]saigetypes.Delta{agenttest.TextResponse("hi")})
		if _, _, err := runLead(t, s, lead); err != nil {
			t.Fatal(err)
		}
		if len(l.Instances()) != 0 {
			t.Fatal("a turn that used no tool leased an instance")
		}
	})
}
