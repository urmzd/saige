package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/runtimetest"
	"github.com/urmzd/saige/flow/turn/sandbox"
)

func plane(t *testing.T) sandbox.Plane {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	return sandbox.Plane{Control: controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop()), Tools: tools}
}

func pinned() turn.Pinned {
	return turn.Pinned{
		Name:    "echo",
		Version: "1",
		Digest:  "sha256:0123456789abcdef",
		Spec:    turn.Spec{Name: "echo", Provider: "fake", Model: "fake"},
	}
}

func input() turn.Input {
	return turn.Input{Trigger: event.Envelope{
		ID: "e1", Source: "fiber:test", Type: event.TypeMessagePosted, Channel: "fiber:test",
		Time:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Data:    event.MessagePosted{Request: event.Request{Text: "say hello"}},
		Private: event.Private{"bot_user": "U0"},
	}}
}

// fake answers in two text deltas, and honours a stop on its context.
func fake(seen func(ctx context.Context, in turn.Input)) turn.Runtime {
	return turn.Func(func(ctx context.Context, _ turn.Pinned, in turn.Input, emit func(context.Context, event.Body) error) (turn.Result, error) {
		if seen != nil {
			seen(ctx, in)
		}
		select {
		case s := <-turn.StopSignal(ctx):
			return turn.Result{Stopped: &s}, nil
		default:
		}
		var text strings.Builder
		for _, part := range []string{"hel", "lo"} {
			if err := emit(ctx, event.Text{Text: part}); err != nil {
				return turn.Result{Text: text.String()}, err
			}
			text.WriteString(part)
		}
		return turn.Result{Text: text.String(), Prompt: "say hello"}, nil
	})
}

func TestSandboxedFakeConforms(t *testing.T) {
	t.Parallel()
	runtimetest.Run(t, runtimetest.Harness{
		New:    func(t *testing.T) turn.Runtime { return sandbox.Sandboxed(fake(nil), plane(t)) },
		Pinned: pinned(),
		Input:  input(),
	})
}

// The task runs with the caller's actor and the trigger's private data,
// neither of which crosses the task input.
func TestTheTaskActsForTheCaller(t *testing.T) {
	t.Parallel()
	var (
		actor   event.Actor
		private event.Private
		ws      bool
	)
	rt := sandbox.Sandboxed(fake(func(ctx context.Context, in turn.Input) {
		actor, _ = event.ActorFrom(ctx)
		private = in.Trigger.Private
		_, ws = sandbox.Workspace(ctx)
	}), plane(t))
	ctx := event.WithActor(t.Context(), event.Actor{ID: "U1"})
	res, err := rt.Turn(ctx, pinned(), input(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" {
		t.Fatalf("Text = %q", res.Text)
	}
	if actor.ID != "U1" {
		t.Errorf("actor = %+v, want the caller's", actor)
	}
	if private["bot_user"] != "U0" {
		t.Errorf("private = %v, want the trigger's", private)
	}
	if !ws {
		t.Error("the task has no workspace")
	}
}

// A failed turn reports its error and what it produced.
func TestAFailedTurnKeepsItsText(t *testing.T) {
	t.Parallel()
	rt := sandbox.Sandboxed(turn.Func(func(ctx context.Context, _ turn.Pinned, _ turn.Input, emit func(context.Context, event.Body) error) (turn.Result, error) {
		_ = emit(ctx, event.Text{Text: "half"})
		return turn.Result{Text: "half"}, errors.New("model went away")
	}), plane(t))
	var got []event.Body
	res, err := rt.Turn(t.Context(), pinned(), input(), func(_ context.Context, b event.Body) error {
		got = append(got, b)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "model went away") {
		t.Fatalf("err = %v", err)
	}
	if res.Text != "half" || len(got) != 1 {
		t.Fatalf("Text = %q, bodies = %v", res.Text, got)
	}
}

// A sandbox with nothing to run on fails closed.
func TestAnIncompleteSandboxRefuses(t *testing.T) {
	t.Parallel()
	for name, rt := range map[string]turn.Runtime{
		"inner": sandbox.Sandboxed(nil, plane(t)),
		"plane": sandbox.Sandboxed(fake(nil), sandbox.Plane{}),
	} {
		if _, err := rt.Turn(t.Context(), pinned(), input(), nil); !errors.Is(err, sandbox.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDeploymentNameCarriesThePin(t *testing.T) {
	t.Parallel()
	if got := sandbox.DeploymentName(pinned()); got != "fiber-echo-v1-0123456789ab" {
		t.Fatalf("DeploymentName = %q", got)
	}
}
