package runtime_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

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

// A revision granting a tool this deployment does not have used to fail every
// run of it at accept. It now runs without that tool, and the sandbox's logger
// says which tool and why, so an operator can fix the deployment without the
// agent being down meanwhile.
func TestARunGrantedAToolTheDeploymentLacksStillRuns(t *testing.T) {
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	history := newSpecs()
	history.revise("triage", "Triage.")
	rev := history.edit("triage", func(s *native.Spec) { s.Tools = []string{"jira.transition"} })

	var logs bytes.Buffer
	s, err := runtime.NewSandbox(plane, tools, history,
		runtime.WithSandboxLogger(slog.New(slog.NewTextHandler(&logs, nil))),
		runtime.WithCatalog(runtime.Connectors(nil)),
		runtime.WithAgentOptions(scripted(map[string][][]saigetypes.Delta{
			"triage": {agenttest.TextResponse("I cannot move tickets here.")},
		})))
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Execute(context.Background(), withPin(runtime.Request{RunID: "run-1", Tenant: "acme", Attempt: 1, Text: "Move PROJ-1"}, rev),
		func(context.Context, event.Body) error { return nil })
	if err != nil {
		t.Fatalf("Execute = %v, want the run to go ahead without jira.transition", err)
	}
	if out.Text != "I cannot move tickets here." {
		t.Errorf("answer %q", out.Text)
	}
	if !strings.Contains(logs.String(), "jira.transition") || !strings.Contains(logs.String(), "unavailable") {
		t.Errorf("the sandbox log does not report the dropped tool:\n%s", logs.String())
	}

	// A host that wants the old refusal still gets it.
	strict, err := runtime.NewSandbox(plane, tools, history,
		runtime.WithCatalog(runtime.Connectors(nil)),
		runtime.WithAgentOptions(native.WithStrictTools()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Execute(context.Background(), withPin(runtime.Request{RunID: "run-2", Tenant: "acme", Attempt: 1, Text: "Move PROJ-1"}, rev),
		func(context.Context, event.Body) error { return nil }); err == nil {
		t.Error("a strict sandbox ran a revision granted a tool it lacks")
	}
}
