package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// recordingTool records the arguments and call info each call received.
type recordingTool struct {
	mu    sync.Mutex
	args  []map[string]any
	infos []types.ToolCallInfo
	ws    []workspace.Workspace
	out   string
	err   error
}

func (r *recordingTool) Definition() types.ToolDef {
	return types.ToolDef{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}}
}

func (r *recordingTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, args)
	info, _ := types.ToolCallInfoFromContext(ctx)
	r.infos = append(r.infos, info)
	ws, _ := workspace.FromContext(ctx)
	r.ws = append(r.ws, ws)
	return r.out, r.err
}

func TestToolRedactorAtToolBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        string
		err        error
		wantResult string
		wantError  string
	}{
		{"result tokenized", "owner of ada@example.com is bob@example.com", nil, "owner of <<EMAIL_1>> is <<EMAIL_2>>", ""},
		{"error tokenized", "", errors.New("no account for ada@example.com"), "", "no account for <<EMAIL_1>>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := privacy.NewVault(nil)
			// The user's message reached the model already tokenized.
			if tok, _ := vault.Tokenize(context.Background(), "ada@example.com"); tok != "<<EMAIL_1>>" {
				t.Fatal(tok)
			}
			tool := &recordingTool{out: tc.out, err: tc.err}
			var gateSaw map[string]any
			gate := types.GateFunc(func(_ context.Context, _ types.ToolDef, args map[string]any) types.GateDecision {
				gateSaw = args
				return types.Allow()
			})
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", map[string]any{"email": "<<EMAIL_1>>", "nested": []any{"<<EMAIL_1>>"}}),
				agenttest.TextResponse("done"),
			}}
			a := NewAgent(AgentConfig{
				Name:     "support",
				Provider: provider,
				Tools:    types.NewToolRegistry(tool),
			}, WithToolRedactor(privacy.NewToolRedactor(vault)), WithToolGate(gate))

			deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("look up <<EMAIL_1>>")}).Deltas())
			agenttest.AssertNoErrors(t, deltas)

			if len(tool.args) != 1 || tool.args[0]["email"] != "ada@example.com" || tool.args[0]["nested"].([]any)[0] != "ada@example.com" {
				t.Fatalf("tool received %v, want restored values", tool.args)
			}
			if gateSaw["email"] != "<<EMAIL_1>>" {
				t.Fatalf("gate saw %v, want placeholders", gateSaw)
			}
			if info := tool.infos[0]; info.ID != "c1" || info.Name != "lookup" || info.Agent != "support" || info.RunID == "" || info.Branch == "" {
				t.Fatalf("call info = %+v", tool.infos[0])
			}

			var end types.ToolExecEndDelta
			for _, d := range deltas {
				if e, ok := d.(types.ToolExecEndDelta); ok {
					end = e
				}
			}
			if end.Result != tc.wantResult || end.Error != tc.wantError {
				t.Fatalf("ToolExecEndDelta = %+v", end)
			}

			results := toolResults(t, a)
			if len(results) != 1 {
				t.Fatalf("tool results = %d", len(results))
			}
			if strings.Contains(results[0].Text, "@example.com") {
				t.Fatalf("tree holds a real value: %q", results[0].Text)
			}
			second := provider.Requests()[1].Messages
			for _, m := range second {
				if sm, ok := m.(types.SystemMessage); ok {
					for _, c := range sm.Content {
						if tr, ok := c.(types.ToolResultContent); ok && strings.Contains(tr.Text, "@example.com") {
							t.Fatalf("provider received a real value: %q", tr.Text)
						}
					}
				}
			}
		})
	}
}

func TestWorkspaceAttachedAndNarrowedForChildren(t *testing.T) {
	ws := workspace.NewMemory()
	tool := &recordingTool{out: "ok"}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "lookup", map[string]any{}),
		agenttest.TextResponse("done"),
	}}
	a := NewAgent(AgentConfig{Name: "lead", Provider: provider, Tools: types.NewToolRegistry(tool)}, WithWorkspace(ws))
	agenttest.AssertNoErrors(t, agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}).Deltas()))
	if len(tool.ws) != 1 || tool.ws[0] != workspace.Workspace(ws) {
		t.Fatalf("tool saw workspace %v, want the configured one", tool.ws)
	}

	child := childConfig(t, AgentConfig{Name: "lead", Provider: provider, Workspace: ws}, SubAgentDef{Name: "worker", Description: "w"})
	if child.Workspace == nil {
		t.Fatal("child has no workspace")
	}
	if _, err := child.Workspace.Put(context.Background(), "x", []byte("y"), nil); !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("child write err = %v, want ErrReadOnly", err)
	}
	if _, err := ws.Put(context.Background(), "shared", []byte("parent data"), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := child.Workspace.Read(context.Background(), workspace.Ref{Name: "shared"}, 0, 0); err != nil || string(got) != "parent data" {
		t.Fatalf("child read = %q, %v", got, err)
	}

	redactor := privacy.NewToolRedactor(privacy.NewVault(nil))
	if c := childConfig(t, AgentConfig{Name: "lead", Provider: provider, ToolRedactor: redactor}, SubAgentDef{Name: "worker", Description: "w"}); c.ToolRedactor != types.ToolRedactor(redactor) {
		t.Fatal("child must share the parent's redactor")
	}
	if c := childConfig(t, AgentConfig{Name: "lead", Provider: provider}, SubAgentDef{Name: "worker", Description: "w"}); c.Workspace != nil {
		t.Fatal("child of an agent without a workspace has one")
	}
}

// textInvoker is a custom SubAgentInvoker whose child replies with fixed
// text and an optional error, as a remote agent might.
type textInvoker struct {
	text string
	err  error
}

func (textInvoker) Definition() types.ToolDef {
	return types.ToolDef{Name: "remote", Parameters: types.ParameterSchema{Type: "object"}}
}
func (textInvoker) Execute(context.Context, map[string]any) (string, error) { return "", nil }
func (i textInvoker) InvokeAgent(ctx context.Context, _ string) *EventStream {
	ctx, cancel := context.WithCancel(ctx)
	s := newEventStream(ctx, cancel)
	go func() {
		s.send(types.TextContentDelta{Content: i.text})
		s.close(i.err)
	}()
	return s
}

func TestToolRedactorCoversCustomInvokers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invoker    textInvoker
		wantResult string
		wantError  string
	}{
		{"result tokenized", textInvoker{text: "reach ada@example.com"}, "reach <<EMAIL_1>>", ""},
		{"error tokenized, partial text kept", textInvoker{text: "reach ada@example.com", err: errors.New("lost bob@example.com")}, "reach <<EMAIL_1>>", "lost <<EMAIL_2>>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "remote", map[string]any{"task": "find"}),
				agenttest.TextResponse("done"),
			}}
			a := NewAgent(AgentConfig{Name: "lead", Provider: provider, Tools: types.NewToolRegistry(tc.invoker)},
				WithToolRedactor(privacy.NewToolRedactor(privacy.NewVault(nil))))
			deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}).Deltas())

			var end types.ToolExecEndDelta
			for _, d := range deltas {
				if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "c1" {
					end = e
				}
			}
			if end.Result != tc.wantResult || end.Error != tc.wantError {
				t.Fatalf("ToolExecEndDelta = %+v", end)
			}
			for _, r := range toolResults(t, a) {
				if strings.Contains(r.Text, "@example.com") {
					t.Fatalf("tree holds a real value: %q", r.Text)
				}
			}
			for _, m := range provider.Requests()[1].Messages {
				if sm, ok := m.(types.SystemMessage); ok {
					for _, c := range sm.Content {
						if tr, ok := c.(types.ToolResultContent); ok && strings.Contains(tr.Text, "@example.com") {
							t.Fatalf("provider received a real value: %q", tr.Text)
						}
					}
				}
			}
		})
	}
}

// citingTool returns a fixed result with citations.
type citingTool struct{ cites []types.Citation }

func (c *citingTool) Definition() types.ToolDef {
	return types.ToolDef{Name: "cite", Parameters: types.ParameterSchema{Type: "object"}}
}

func (c *citingTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := c.ExecuteRich(ctx, args)
	return r.Text, err
}

func (c *citingTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{Text: "see the linked resource", Citations: c.cites}, nil
}

// TestToolRedactorTokenizesCitations checks that a rich tool's citations
// reach the stream and the registry with placeholders, not real values.
func TestToolRedactorTokenizesCitations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redactor bool
		want     string // substring the streamed citation must contain
		wantReal bool   // the real value may appear
	}{
		{name: "with a redactor", redactor: true, want: "<<EMAIL_"},
		{name: "without a redactor", want: "a@b.com", wantReal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cite := types.NewCitation(types.CitationTool, "mailto:a@b.com", "Mail a@b.com")
			cite.Quote = "written by a@b.com"
			cite.Meta = map[string]any{"owner": "a@b.com", "rank": 1}
			tool := &citingTool{cites: []types.Citation{cite}}
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "cite", map[string]any{}),
				agenttest.TextResponse("done"),
			}}
			var opts []AgentOption
			if tc.redactor {
				opts = append(opts, WithToolRedactor(privacy.NewToolRedactor(privacy.NewVault(nil))))
			}
			a := NewAgent(AgentConfig{Name: "a", Provider: provider, Tools: types.NewToolRegistry(tool)}, opts...)
			deltas := agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}).Deltas())
			agenttest.AssertNoErrors(t, deltas)

			var got []types.Citation
			for _, d := range deltas {
				if cd, ok := d.(types.CitationDelta); ok {
					got = append(got, cd.Citation)
				}
			}
			if len(got) != 1 {
				t.Fatalf("streamed citations = %+v, want one", got)
			}
			got = append(got, a.Citations().Sources()...)
			got = append(got, a.Citations().Quotes(1)...)
			for _, c := range got {
				all := fmt.Sprint(c.URI, c.Title, c.Quote, c.Meta)
				if !strings.Contains(all, tc.want) {
					t.Errorf("citation %+v does not contain %q", c, tc.want)
				}
				if !tc.wantReal && strings.Contains(all, "a@b.com") {
					t.Errorf("citation %+v holds a real value", c)
				}
				if c.Meta["rank"] != 1 {
					t.Errorf("non-string meta lost: %+v", c.Meta)
				}
			}
		})
	}
}
