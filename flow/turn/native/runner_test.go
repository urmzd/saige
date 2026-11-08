package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	ragtypes "github.com/urmzd/saige/rag/types"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

func offlineRevision(rev int, prompt string) native.Revision {
	version := ""
	if rev > 0 {
		version = fmt.Sprintf("%d.0.0", rev)
	}
	return native.Revision{
		Name:    "docs-bot",
		Version: version,
		Spec: native.Spec{
			Name:         "docs-bot",
			Provider:     native.ProviderOffline,
			Model:        "scripted",
			SystemPrompt: prompt,
		},
	}
}

// collect runs a turn and returns the chunks it produced, in order.
func collect(t *testing.T, r *native.Runner, turn native.Turn) ([]event.Body, native.Result) {
	t.Helper()
	var got []event.Body
	res, err := r.Run(context.Background(), turn, func(_ context.Context, c event.Body) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got, res
}

// The zero-infrastructure path has to work end to end: no API key, no daemon, no
// network, and still a real agent loop producing real events.
func TestOfflineProviderStreamsAScriptedTurn(t *testing.T) {
	t.Parallel()

	r, err := native.New(offlineRevision(1, "You document things."),
		native.WithScript(agenttest.TextResponse("the docs are updated")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, res := collect(t, r, native.Turn{Text: "update the docs"})

	if res.Text != "the docs are updated" {
		t.Errorf("answer is %q, want %q", res.Text, "the docs are updated")
	}
	// The answer, then the one model call's usage.
	if len(got) != 2 || got[0].Type() != event.TypeText || got[1].Type() != event.TypeUsageCall {
		t.Fatalf("got %d chunks (%v), want a text chunk and a usage.call", len(got), kinds(got))
	}
	if res.Chunks != 2 {
		t.Errorf("Result.Chunks is %d, want 2", res.Chunks)
	}
}

// A run pins a revision, so the runner must report and execute that revision's
// definition and nothing else.
func TestRunnerExecutesThePinnedRevisionsPrompt(t *testing.T) {
	t.Parallel()

	var seen []saigetypes.Message
	spy := func(spec native.Spec) (saigetypes.Provider, error) {
		return providerFunc(func(_ context.Context, msgs []saigetypes.Message, _ []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
			seen = msgs
			return scripted(agenttest.TextResponse("ok")), nil
		}), nil
	}

	rev := offlineRevision(6, "revision six instructions")
	rev.Spec.Provider = "spy"
	r, err := native.New(rev, native.WithProvider("spy", spy))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := r.Revision().Version; got != "6.0.0" {
		t.Fatalf("Revision().Version is %s, want 6.0.0", got)
	}
	if got := r.SystemPrompt(); got != "revision six instructions" {
		t.Fatalf("SystemPrompt() is %q, want revision six's", got)
	}
	collect(t, r, native.Turn{Text: "hello"})

	if len(seen) == 0 {
		t.Fatal("the provider was never called")
	}
	if !messagesContain(seen, "revision six instructions") {
		t.Fatalf("the model was not given revision six's system prompt: %+v", seen)
	}
}

// Citations arrive before the answer, so a sink can render footnotes for text it
// has not shown yet, and the retrieved context reaches the model.
func TestRetrievalPublishesCitationsBeforeTheAnswer(t *testing.T) {
	t.Parallel()

	var seen []saigetypes.Message
	spy := func(spec native.Spec) (saigetypes.Provider, error) {
		return providerFunc(func(_ context.Context, msgs []saigetypes.Message, _ []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
			seen = msgs
			return scripted(agenttest.TextResponse("deploys happen on merge [1]")), nil
		}), nil
	}
	rev := offlineRevision(1, "answer from the runbook")
	rev.Spec.Provider = "spy"
	rev.Spec.Sources = []native.Source{{Name: "runbook", URI: "file://runbook.md"}}

	r, err := native.New(rev,
		native.WithProvider("spy", spy),
		native.WithRetriever(retrieverFunc(func(context.Context, string) (*ragtypes.AssembledContext, error) {
			return &ragtypes.AssembledContext{
				Prompt: "Context: deploys run on merge to main.",
				Blocks: []ragtypes.ContextBlock{{Text: "deploys run on merge to main", Citation: "[1]"}},
			}, nil
		})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, res := collect(t, r, native.Turn{Text: "when do deploys happen?"})

	if len(got) < 2 {
		t.Fatalf("got %v, want a citation followed by text", kinds(got))
	}
	if got[0].Type() != event.TypeCitation {
		t.Errorf("first chunk is %s, want %s", got[0].Type(), event.TypeCitation)
	}
	if got[1].Type() != event.TypeText {
		t.Errorf("second chunk is %s, want %s", got[1].Type(), event.TypeText)
	}
	if res.Citations != 1 {
		t.Errorf("Result.Citations is %d, want 1", res.Citations)
	}
	if !messagesContain(seen, "deploys run on merge to main") {
		t.Fatalf("the retrieved context never reached the model: %+v", seen)
	}
}

// A spec with no sources must not pay for retrieval, whatever the deployment has
// configured: sources are the grant.
func TestRetrievalIsSkippedWhenTheSpecNamesNoSources(t *testing.T) {
	t.Parallel()

	called := false
	r, err := native.New(offlineRevision(1, ""),
		native.WithScript(agenttest.TextResponse("hi")),
		native.WithRetriever(retrieverFunc(func(context.Context, string) (*ragtypes.AssembledContext, error) {
			called = true
			return nil, nil
		})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "hello"})
	if called {
		t.Fatal("retrieval ran for a spec with no sources")
	}
}

// A connector action that runs must be reported as an action event carrying the
// address of what it touched, and the model must be bound to the tag's surface
// rather than choosing its own.
func TestConnectorActionReportsWhatItDidAndWhere(t *testing.T) {
	t.Parallel()

	target := address.MustParse("github://urmzd/fiber/issues/42")
	var invokedAt address.Address
	reg := connector.NewRegistry()
	if err := reg.Register(&fakeConnector{actions: []connector.Action{{
		Name:        "github_comment",
		Description: "Comment on the issue",
		Schema:      json.RawMessage(`{"type":"object","required":["body"],"properties":{"body":{"type":"string"}}}`),
		Invoke: func(_ context.Context, at address.Address, _ json.RawMessage) (connector.Result, error) {
			invokedAt = at
			return connector.Result{
				Summary: "posted a comment",
				Address: address.MustParse("github://urmzd/fiber/issues/42/comments/7"),
			}, nil
		},
	}}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var actions []event.Action
	rev := offlineRevision(1, "")
	rev.Spec.Tools = []string{"github_comment"}
	r, err := native.New(rev,
		native.WithTools(native.ConnectorTools(reg, target, func(a event.Action) error {
			actions = append(actions, a)
			return nil
		})),
		native.WithScript(
			agenttest.ToolCallResponse("call_1", "github_comment", map[string]any{"body": "on it"}),
			agenttest.TextResponse("commented"),
		))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, res := collect(t, r, native.Turn{Text: "comment on the issue"})

	if len(actions) != 1 {
		t.Fatalf("got %d action reports, want 1", len(actions))
	}
	if actions[0].Name != "github_comment" {
		t.Errorf("action name is %q, want github_comment", actions[0].Name)
	}
	if actions[0].Target != target.String() {
		t.Errorf("action target is %q, want %q", actions[0].Target, target)
	}
	if actions[0].Address != "github://urmzd/fiber/issues/42/comments/7" {
		t.Errorf("action address is %q, want the created comment", actions[0].Address)
	}
	if invokedAt.String() != target.String() {
		t.Errorf("the action was invoked against %q, want the tag's surface %q", invokedAt, target)
	}
	if res.Text != "commented" {
		t.Errorf("answer is %q, want %q", res.Text, "commented")
	}
	if !hasKind(got, event.TypeToolCall) || !hasKind(got, event.TypeToolDone) {
		t.Errorf("chunks are %v, want a tool call and a tool done", kinds(got))
	}
	// The agent loop names the tool on the delta that reports its result, so
	// the done event says which tool finished.
	for _, c := range got {
		if c.Type() != event.TypeToolDone {
			continue
		}
		var done event.ToolDone
		if err := json.Unmarshal(raw(c), &done); err != nil {
			t.Fatalf("decode tool done: %v", err)
		}
		if done.Name != "github_comment" {
			t.Errorf("tool done names %q, want github_comment", done.Name)
		}
	}
}

// The prompt a turn sends is part of a durable run's input, so identical tag
// metadata must produce an identical prompt however Go happens to order its maps.
func TestMetadataRendersInAStableOrder(t *testing.T) {
	t.Parallel()

	meta := map[string]string{"pr": "42", "author": "urmzd", "title": "fix the docs"}
	var prompts []string
	for range 8 {
		spy := func(native.Spec) (saigetypes.Provider, error) {
			return providerFunc(func(_ context.Context, msgs []saigetypes.Message, _ []saigetypes.ToolDef) (<-chan saigetypes.Delta, error) {
				prompts = append(prompts, renderMessages(msgs))
				return scripted(agenttest.TextResponse("ok")), nil
			}), nil
		}
		rev := offlineRevision(1, "")
		rev.Spec.Provider = "spy"
		r, err := native.New(rev, native.WithProvider("spy", spy))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		collect(t, r, native.Turn{Text: "review this", Meta: meta})
	}
	for i, p := range prompts {
		if p != prompts[0] {
			t.Fatalf("prompt %d differs from the first:\n%s\n---\n%s", i, prompts[0], p)
		}
	}
	if !strings.Contains(prompts[0], "- author: urmzd") {
		t.Fatalf("metadata is not in the prompt:\n%s", prompts[0])
	}
}

// A definition this deployment cannot honour must fail at construction, not
// halfway through a conversation. A granted tool it cannot resolve is such a
// definition only under WithStrictTools; otherwise it is dropped (see
// TestAnUnresolvableToolIsDroppedAndTheModelIsTold).
func TestConstructionRefusesWhatItCannotRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rev  native.Revision
		opts []native.Option
	}{
		{
			name: "an unpinned revision",
			rev:  offlineRevision(0, ""),
		},
		{
			name: "a provider this process does not have",
			rev: native.Revision{Name: "docs-bot", Version: "1.0.0", Spec: native.Spec{
				Name: "docs-bot", Provider: "gpt-9", Model: "x",
			}},
		},
		{
			name: "a tool no connector contributes",
			rev: func() native.Revision {
				r := offlineRevision(1, "")
				r.Spec.Tools = []string{"jira_transition"}
				return r
			}(),
			opts: []native.Option{native.WithTools(native.StaticTools()), native.WithStrictTools()},
		},
		{
			name: "a tool with no catalog at all",
			rev: func() native.Revision {
				r := offlineRevision(1, "")
				r.Spec.Tools = []string{"jira_transition"}
				return r
			}(),
			opts: []native.Option{native.WithStrictTools()},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := native.New(tc.rev, tc.opts...); !errors.Is(err, native.ErrInvalid) {
				t.Fatalf("New returned %v, want an error matching native.ErrInvalid", err)
			}
		})
	}
}

// A provider failure is the turn's failure, and the caller needs the reason.
func TestProviderErrorFailsTheTurn(t *testing.T) {
	t.Parallel()

	boom := errors.New("model refused")
	rev := offlineRevision(1, "")
	r, err := native.New(rev, native.WithScript([]saigetypes.Delta{saigetypes.ErrorDelta{Error: boom}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Run(context.Background(), native.Turn{Text: "hi"}, nil); err == nil {
		t.Fatal("Run succeeded on an error delta")
	} else if !strings.Contains(err.Error(), "model refused") {
		t.Fatalf("error is %v, want it to carry the provider's reason", err)
	}
}

// A consumer that cannot keep up stops the turn: burning model tokens for a
// stream nobody is reading is worse than failing.
func TestAnEmitFailureAbortsTheTurn(t *testing.T) {
	t.Parallel()

	gone := errors.New("subscriber gone")
	r, err := native.New(offlineRevision(1, ""), native.WithScript(agenttest.TextResponse("hello")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Run(context.Background(), native.Turn{Text: "hi"}, func(context.Context, event.Body) error {
		return gone
	})
	if !errors.Is(err, gone) {
		t.Fatalf("Run returned %v, want the emit failure", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────

type providerFunc func(context.Context, []saigetypes.Message, []saigetypes.ToolDef) (<-chan saigetypes.Delta, error)

func (f providerFunc) Stream(ctx context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	return f(ctx, req.Messages, req.Tools)
}

func scripted(deltas []saigetypes.Delta) <-chan saigetypes.Delta {
	ch := make(chan saigetypes.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch
}

type retrieverFunc func(context.Context, string) (*ragtypes.AssembledContext, error)

func (f retrieverFunc) Retrieve(ctx context.Context, q string) (*ragtypes.AssembledContext, error) {
	return f(ctx, q)
}

type fakeConnector struct {
	actions []connector.Action
}

func (c *fakeConnector) Name() string                { return "github" }
func (c *fakeConnector) Actions() []connector.Action { return c.actions }

func kinds(cs []event.Body) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Type()
	}
	return out
}

func hasKind(cs []event.Body, k string) bool {
	for _, c := range cs {
		if c.Type() == k {
			return true
		}
	}
	return false
}

func renderMessages(msgs []saigetypes.Message) string {
	b, _ := json.Marshal(msgs)
	return string(b)
}

func messagesContain(msgs []saigetypes.Message, want string) bool {
	return strings.Contains(renderMessages(msgs), want)
}

// A tool that asks for human approval is refused inside a turn: nobody can
// answer it, so the tool must not run and the turn must still finish.
func TestApprovalRequestIsRefused(t *testing.T) {
	t.Parallel()

	inner := &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "deploy"}, Result: "deployed"}
	marked := &saigetypes.MarkedTool{Inner: inner, Markers: []saigetypes.Marker{{Kind: "human_approval"}}}
	rev := offlineRevision(1, "")
	rev.Spec.Tools = []string{"deploy"}
	r, err := native.New(rev,
		native.WithTools(native.StaticTools(marked)),
		native.WithScript(
			agenttest.ToolCallResponse("call_1", "deploy", map[string]any{}),
			agenttest.TextResponse("not deployed"),
		))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, res := collect(t, r, native.Turn{Text: "deploy it"})

	if len(inner.Calls) != 0 {
		t.Errorf("the tool ran %d times, want 0", len(inner.Calls))
	}
	if res.Text != "not deployed" {
		t.Errorf("answer is %q, want %q", res.Text, "not deployed")
	}
	if !hasKind(got, event.TypeToolDone) {
		t.Errorf("chunks are %v, want a tool done for the refused call", kinds(got))
	}
}
