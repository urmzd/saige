package native_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn/native"
)

// spyRequests records every model request and answers each with text.
func spyRequests(seen *[][]saigetypes.ToolDef, msgs *[]saigetypes.Message) native.ProviderFunc {
	return func(native.Spec) (saigetypes.Provider, error) {
		return providerFunc(func(_ context.Context, m []saigetypes.Message, tools []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
			*seen = append(*seen, tools)
			*msgs = m
			return scripted(agenttest.TextResponse("ok")), nil
		}), nil
	}
}

// A grant this process cannot honour (a connector not configured here, a tool
// an MCP admin disabled) costs the agent that tool and not the run. The run
// goes ahead with the tools it has, the model is told which are missing so it
// does not invent their calls, and the operator gets the reason.
func TestAnUnresolvableToolIsDroppedAndTheModelIsTold(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	var tools [][]saigetypes.ToolDef
	var msgs []saigetypes.Message
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Provider = "spy"
	rev.Spec.Tools = []string{"lookup", "jira_transition", "mcp_disabled"}
	catalog := native.ToolsFunc(func(name string) (saigetypes.Tool, error) {
		switch name {
		case "lookup":
			return lookupTool("lookup"), nil
		case "mcp_disabled":
			return nil, errors.New("held back: pending review on host internal-mcp.corp")
		}
		return nil, fmt.Errorf("%w: %q", native.ErrNoSuchTool, name)
	})
	r, err := native.New(rev,
		native.WithProvider("spy", spyRequests(&tools, &msgs)),
		native.WithTools(catalog),
		native.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("New = %v, want the run to go ahead without the missing tools", err)
	}
	_, res := collect(t, r, native.Turn{Text: "move the ticket"})
	if res.Text != "ok" {
		t.Fatalf("answer %q, want the model's", res.Text)
	}

	if len(tools) == 0 || len(tools[0]) != 1 || tools[0][0].Name != "lookup" {
		t.Fatalf("the model was offered %v, want only lookup", tools)
	}
	var system string
	for _, m := range msgs {
		if m.Role() == saigetypes.RoleSystem {
			system = renderMessages([]saigetypes.Message{m})
		}
	}
	if !strings.Contains(system, "You look things up.") ||
		!strings.Contains(system, "unavailable") || !strings.Contains(system, "jira_transition, mcp_disabled") {
		t.Errorf("system prompt %q does not tell the model which tools are unavailable", system)
	}
	if strings.Contains(system, "internal-mcp.corp") {
		t.Errorf("the reason reached the model: %q", system)
	}

	got := r.Unavailable()
	if len(got) != 2 || got[0].Name != "jira_transition" || got[1].Name != "mcp_disabled" ||
		!strings.Contains(got[1].Reason, "held back") {
		t.Errorf("Unavailable() = %+v, want both dropped tools with their reasons", got)
	}
	for _, want := range []string{"jira_transition", "mcp_disabled", "internal-mcp.corp"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the operator log does not mention %q:\n%s", want, logs.String())
		}
	}
}

// A runner with every tool resolved sends the spec's prompt untouched, so a
// deployment that has all its tools sees no change in its cached prefix.
func TestAFullyResolvedRunnerSendsThePromptUntouched(t *testing.T) {
	t.Parallel()

	var tools [][]saigetypes.ToolDef
	var msgs []saigetypes.Message
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Provider = "spy"
	rev.Spec.Tools = []string{"lookup"}
	r, err := native.New(rev, native.WithProvider("spy", spyRequests(&tools, &msgs)),
		native.WithTools(native.StaticTools(lookupTool("lookup"))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "hi"})
	for _, m := range msgs {
		if got := renderMessages([]saigetypes.Message{m}); m.Role() == saigetypes.RoleSystem &&
			(!strings.Contains(got, `"You look things up."`) || strings.Contains(got, "unavailable")) {
			t.Errorf("system prompt %s, want the spec's exactly", got)
		}
	}
	if len(r.Unavailable()) != 0 {
		t.Errorf("Unavailable() = %v, want none", r.Unavailable())
	}
}

// A dropped tool that required approval has nothing left to approve, and the
// approval must not turn the drop back into a failed run. A tool that was
// never granted still fails: that is a broken definition, not a missing tool.
func TestAnApprovalOnADroppedToolDoesNotFailTheRun(t *testing.T) {
	t.Parallel()

	rev := offlineRevision(1, "You deploy.")
	rev.Spec.Tools = []string{"deploy"}
	rev.Spec.Approvals = []string{"deploy"}
	r, err := native.New(rev, native.WithTools(native.StaticTools()))
	if err != nil {
		t.Fatalf("New = %v, want the run to go ahead without deploy", err)
	}
	if got := r.Unavailable(); len(got) != 1 || got[0].Name != "deploy" {
		t.Errorf("Unavailable() = %v, want deploy", got)
	}

	rev.Spec.Approvals = []string{"rollback"}
	if _, err := native.New(rev, native.WithTools(native.StaticTools())); !errors.Is(err, native.ErrInvalid) {
		t.Errorf("an approval for an ungranted tool: New = %v, want ErrInvalid", err)
	}
}

// Strict mode is the old contract, kept for hosts that would rather not run
// than run short: the lookup's own error, matching ErrNoSuchTool.
func TestStrictToolsFailsOnTheFirstUnresolvableTool(t *testing.T) {
	t.Parallel()

	rev := offlineRevision(1, "")
	rev.Spec.Tools = []string{"lookup", "jira_transition"}
	_, err := native.New(rev, native.WithStrictTools(),
		native.WithTools(native.StaticTools(lookupTool("lookup"))))
	if !errors.Is(err, native.ErrNoSuchTool) || !strings.Contains(err.Error(), "jira_transition") {
		t.Fatalf("New = %v, want ErrNoSuchTool naming jira_transition", err)
	}
	if !errors.Is(err, native.ErrInvalid) {
		t.Fatalf("New = %v, want it to match ErrInvalid too", err)
	}
}

// notedErr is a refusal whose cause was written for the model, the way
// pkg/mcptools reports a person with no credential of their own.
type notedErr struct{ operator, model string }

func (e notedErr) Error() string     { return e.operator }
func (e notedErr) ModelNote() string { return e.model }

// A dropped grant whose error carries a note for the model is announced with
// it, so the agent can tell the person what to do; the operator's reason,
// which may name infrastructure, still never reaches the model, and a grant
// dropped without a note is announced by name alone, as before.
func TestADroppedGrantsNoteForTheModelFollowsItsName(t *testing.T) {
	t.Parallel()

	var tools [][]saigetypes.ToolDef
	var msgs []saigetypes.Message
	rev := offlineRevision(1, "You triage.")
	rev.Spec.Provider = "spy"
	rev.Spec.Tools = []string{"lookup", native.MCPGroup("issues"), "jira_transition"}
	catalog := groupCatalog{Tools: native.ToolsFunc(func(name string) (saigetypes.Tool, error) {
		if name == "lookup" {
			return lookupTool("lookup"), nil
		}
		return nil, fmt.Errorf("%w: %q on host jira.internal.corp", native.ErrNoSuchTool, name)
	})}
	refusal := notedErr{
		operator: `mcptools: provider "issues" on mcp.internal.corp: ana has no credential`,
		model:    "it needs a credential of your own,\n which you have not connected.",
	}
	r, err := native.New(rev,
		native.WithProvider("spy", spyRequests(&tools, &msgs)),
		native.WithTools(noted{groupCatalog: catalog, err: refusal}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "triage"})

	var system string
	for _, m := range msgs {
		if m.Role() == saigetypes.RoleSystem {
			system = renderMessages([]saigetypes.Message{m})
		}
	}
	for _, want := range []string{"mcp:issues, jira_transition", `mcp:issues: it needs a credential of your own, which you have not connected.`} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt %s does not contain %q", system, want)
		}
	}
	for _, leak := range []string{"internal.corp", "ana has no credential"} {
		if strings.Contains(system, leak) {
			t.Errorf("the operator's reason %q reached the model: %s", leak, system)
		}
	}
	got := r.Unavailable()
	if len(got) != 2 || got[0].Note == "" || !strings.Contains(got[0].Reason, "ana has no credential") || got[1].Note != "" {
		t.Errorf("Unavailable() = %+v, want the group with its note and reason, the tool with its reason only", got)
	}
}

// noted is a catalog whose every group fails with err.
type noted struct {
	groupCatalog
	err error
}

func (n noted) ToolGroup(name string) (native.ToolGroup, error) {
	return native.ToolGroup{}, fmt.Errorf("%w: %q: %w", native.ErrNoSuchToolGroup, name, n.err)
}
