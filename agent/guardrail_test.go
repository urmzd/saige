package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// pricedScript is a scripted provider with a rate card, so a budget can cost
// its calls.
type pricedScript struct {
	*agenttest.ScriptedProvider
	model string
}

func (p pricedScript) Name() string  { return "priced" }
func (p pricedScript) Model() string { return p.model }
func (p pricedScript) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "priced", Model: p.model, Known: true,
		Pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 2, AsOf: "2026-07-01"}}
}

func blockWord(word string) Guardrail {
	return NewGuardrail("no-"+word, func(_ context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		if strings.Contains(in.Text, word) {
			return Block("mentions " + word), nil
		}
		return Pass(), nil
	})
}

func guardrailRecords(t *testing.T, tr *tree.Tree) []types.GuardrailPart {
	t.Helper()
	msgs, err := tr.FlattenBranch(tr.Active())
	if err != nil {
		t.Fatal(err)
	}
	var out []types.GuardrailPart
	add := func(c any) {
		if g, ok := c.(types.GuardrailPart); ok {
			out = append(out, g)
		}
	}
	for _, m := range msgs {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Parts {
				add(c)
			}
		case types.UserMessage:
			for _, c := range v.Parts {
				add(c)
			}
		case types.AssistantMessage:
			for _, c := range v.Parts {
				add(c)
			}
		}
	}
	return out
}

func collectWithDeltas(t *testing.T, a *Agent, input string) (Transcript, []types.Delta, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var deltas []types.Delta
	tr, err := Collect(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text(input))}), func(d types.Delta) { deltas = append(deltas, d) })
	return tr, deltas, err
}

func guardrailDeltas(deltas []types.Delta) []types.GuardrailDelta {
	var out []types.GuardrailDelta
	for _, d := range deltas {
		if g, ok := d.(types.GuardrailDelta); ok {
			out = append(out, g)
		}
	}
	return out
}

func TestSequentialInputGuardrailBlocksBeforeTheModel(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("never")}}
	var stop RunStopEvent
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p},
		WithInputGuardrails(InputGuardrail{Guardrail: blockWord("secret")}),
		WithHooks(Hooks{RunStop: func(_ context.Context, ev *RunStopEvent) error { stop = *ev; return nil }}))

	_, deltas, err := collectWithDeltas(t, a, "tell me the secret")
	var tripped *GuardrailTrippedError
	if !errors.Is(err, ErrGuardrailTripped) || !errors.As(err, &tripped) {
		t.Fatalf("err = %v, want a tripped guardrail", err)
	}
	if tripped.Guardrail != "no-secret" || tripped.Phase != types.GuardrailPhaseInput || tripped.Canceled {
		t.Errorf("tripped = %+v", tripped)
	}
	if p.CallCount() != 0 {
		t.Errorf("model called %d times", p.CallCount())
	}
	if g := guardrailDeltas(deltas); len(g) != 1 || g[0].Action != types.GuardrailActionBlock || g[0].Reason != "mentions secret" {
		t.Errorf("guardrail deltas = %+v", g)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	for _, m := range msgs {
		if um, ok := m.(types.UserMessage); ok && strings.Contains(userText(um), "secret") {
			t.Error("the blocked message was recorded")
		}
	}
	if recs := guardrailRecords(t, a.Tree()); len(recs) != 1 || recs[0].Action != types.GuardrailActionBlock {
		t.Errorf("tree records = %+v", recs)
	}
	if stop.Reason != RunStopGuardrail {
		t.Errorf("RunStop reason = %q", stop.Reason)
	}

	// The record survives serialization.
	raw, err := json.Marshal(a.Tree())
	if err != nil {
		t.Fatal(err)
	}
	var back tree.Tree
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if recs := guardrailRecords(t, &back); len(recs) != 1 {
		t.Errorf("records after a round trip = %+v", recs)
	}
}

func TestInputGuardrailRewrite(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}
	redact := NewGuardrail("redact", func(_ context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		return Rewrite(strings.ReplaceAll(in.Text, "ana@example.com", "[email]"), "email"), nil
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p}, WithInputGuardrails(InputGuardrail{Guardrail: redact}))
	_, deltas, err := collectWithDeltas(t, a, "mail ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sent := p.Requests()[0].Messages
	last := sent[len(sent)-1].(types.UserMessage)
	if got := userText(last); got != "mail [email]" {
		t.Errorf("model saw %q", got)
	}
	for _, c := range last.Parts {
		if _, ok := c.(types.GuardrailPart); ok {
			t.Error("guardrail metadata reached the model")
		}
	}
	if recs := guardrailRecords(t, a.Tree()); len(recs) != 1 || recs[0].Action != types.GuardrailActionRewrite {
		t.Errorf("tree records = %+v", recs)
	}
	if g := guardrailDeltas(deltas); len(g) != 1 || g[0].Text != "mail [email]" {
		t.Errorf("guardrail deltas = %+v", g)
	}
}

// stallingProvider streams some text, signals that it started, and then
// waits until its call is cancelled.
type stallingProvider struct {
	started  chan struct{}
	canceled atomic.Bool
}

func (p *stallingProvider) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 4)
	go func() {
		defer close(ch)
		ch <- types.PartStart{Index: 0, Kind: types.KindText}
		ch <- types.PartDelta{Index: 0, Text: "partial "}
		close(p.started)
		select {
		case <-ctx.Done():
			p.canceled.Store(true)
		case <-time.After(5 * time.Second):
			ch <- types.PartDelta{Index: 0, Text: "finished"}
			ch <- types.PartEnd{Index: 0}
		}
	}()
	return ch, nil
}

func TestParallelGuardrailCancelsTheModelCall(t *testing.T) {
	p := &stallingProvider{started: make(chan struct{})}
	slow := NewGuardrail("policy", func(ctx context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		// The model call is under way before the verdict.
		select {
		case <-p.started:
		case <-ctx.Done():
			return GuardrailVerdict{}, ctx.Err()
		}
		return Block("off policy"), nil
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p},
		WithInputGuardrails(InputGuardrail{Guardrail: slow, Mode: GuardrailParallel}))
	start := time.Now()
	_, deltas, err := collectWithDeltas(t, a, "anything")
	var tripped *GuardrailTrippedError
	if !errors.As(err, &tripped) || !tripped.Canceled {
		t.Fatalf("err = %v, want a tripped guardrail that cancelled the call", err)
	}
	if time.Since(start) > 4*time.Second || !p.canceled.Load() {
		t.Error("the model call was not cancelled")
	}
	if g := guardrailDeltas(deltas); len(g) != 1 || !g[0].Canceled {
		t.Errorf("guardrail deltas = %+v", g)
	}
	recs := guardrailRecords(t, a.Tree())
	if len(recs) != 1 || !recs[0].Canceled {
		t.Errorf("tree records = %+v, want the cancellation recorded", recs)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	for _, m := range msgs {
		if _, ok := m.(types.AssistantMessage); ok {
			t.Error("the cancelled turn was recorded")
		}
	}
	var usage bool
	for _, d := range deltas {
		if _, ok := d.(types.UsageDelta); ok {
			usage = true
		}
	}
	if !usage {
		t.Error("the cancelled call's usage was not reported")
	}
}

func TestParallelGuardrailPassKeepsTheTurn(t *testing.T) {
	started := make(chan struct{})
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("answer")}}
	signal := &signalProvider{Provider: p, started: started}
	pass := NewGuardrail("policy", func(ctx context.Context, _ GuardrailInput) (GuardrailVerdict, error) {
		select {
		case <-started: // the call began before the verdict
		case <-time.After(5 * time.Second):
			return GuardrailVerdict{}, errors.New("the model call did not start in parallel")
		}
		return Pass(), nil
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: signal},
		WithInputGuardrails(InputGuardrail{Guardrail: pass, Mode: GuardrailParallel}))
	tr, _, err := collectWithDeltas(t, a, "hello")
	if err != nil || tr.Text != "answer" {
		t.Fatalf("text %q err %v", tr.Text, err)
	}
}

type signalProvider struct {
	types.Provider
	started chan struct{}
}

func (s *signalProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	m, tools := req.Messages, req.Tools
	close(s.started)
	return s.Provider.Stream(ctx, types.Request{Messages: m, Tools: tools})
}

func TestParallelGuardrailCannotRewrite(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("answer")}}
	rewrite := NewGuardrail("redact", func(context.Context, GuardrailInput) (GuardrailVerdict, error) {
		return Rewrite("clean", "pii"), nil
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p},
		WithInputGuardrails(InputGuardrail{Guardrail: rewrite, Mode: GuardrailParallel}))
	_, _, err := collectWithDeltas(t, a, "hello")
	if !errors.Is(err, ErrGuardrailTripped) || !strings.Contains(err.Error(), "cannot rewrite") {
		t.Fatalf("err = %v", err)
	}
}

func TestOutputGuardrailRewritesTheAnswer(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("write to ana@example.com")}}
	redact := NewGuardrail("redact", func(_ context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		if in.Phase != types.GuardrailPhaseOutput {
			return GuardrailVerdict{}, errors.New("wrong phase")
		}
		return Rewrite(strings.ReplaceAll(in.Text, "ana@example.com", "[email]"), "email"), nil
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p}, WithOutputGuardrails(OutputGuardrail{Guardrail: redact}))
	tr, deltas, err := collectWithDeltas(t, a, "who?")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "write to [email]" || tr.AllText != "write to [email]" {
		t.Errorf("transcript text %q all %q", tr.Text, tr.AllText)
	}
	if g := guardrailDeltas(deltas); len(g) != 1 || g[0].Phase != types.GuardrailPhaseOutput || g[0].Text != "write to [email]" {
		t.Errorf("guardrail deltas = %+v", g)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	final := msgs[len(msgs)-1].(types.AssistantMessage)
	if got := contentText(final.Parts); got != "write to [email]" {
		t.Errorf("recorded answer %q", got)
	}
	if recs := guardrailRecords(t, a.Tree()); len(recs) != 1 || recs[0].Phase != types.GuardrailPhaseOutput {
		t.Errorf("tree records = %+v", recs)
	}
}

func TestOutputGuardrailBlockDropsTheAnswer(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("the secret is 42")}}
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p}, WithOutputGuardrails(OutputGuardrail{Guardrail: blockWord("secret")}))
	_, _, err := collectWithDeltas(t, a, "q")
	if !errors.Is(err, ErrGuardrailTripped) {
		t.Fatalf("err = %v", err)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	for _, m := range msgs {
		if am, ok := m.(types.AssistantMessage); ok && strings.Contains(contentText(am.Parts), "secret") {
			t.Error("the blocked answer was recorded")
		}
	}
}

func TestOutputGuardrailChecksForcedFinalAnswer(t *testing.T) {
	p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "lookup", map[string]any{"q": "x"}),
		agenttest.TextResponse("the secret is 42"),
	}}
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(hookTool("lookup")),
		MaxIter: 1, OnMaxIter: MaxIterForceFinal},
		WithOutputGuardrails(OutputGuardrail{Guardrail: blockWord("secret")}))
	if _, _, err := collectWithDeltas(t, a, "q"); !errors.Is(err, ErrGuardrailTripped) {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardrailErrorFailsClosed(t *testing.T) {
	broken := NewGuardrail("broken", func(context.Context, GuardrailInput) (GuardrailVerdict, error) {
		return GuardrailVerdict{}, errors.New("detector offline")
	})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "ok"}},
		WithInputGuardrails(InputGuardrail{Guardrail: broken}))
	_, _, err := collectWithDeltas(t, a, "q")
	if !errors.Is(err, ErrGuardrailTripped) || !strings.Contains(err.Error(), "detector offline") {
		t.Fatalf("err = %v", err)
	}
}

// A guardrail that calls a model is charged to the run's budget through
// GuardrailInput.Metered, and its usage is reported.
func TestGuardrailModelCallsAreCharged(t *testing.T) {
	classifier := pricedScript{ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.TextResponse("ALLOW"), types.UsageDelta{PromptTokens: 1000, CompletionTokens: 10}),
	}}, model: "cheap"}
	check := NewGuardrail("classify", func(ctx context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		rx, err := in.Metered(classifier).Stream(ctx, types.Request{Messages: []types.Message{types.UserMsg(types.Text(in.Text))}})
		if err != nil {
			return GuardrailVerdict{}, err
		}
		for range rx {
		}
		return Pass(), nil
	})
	main := pricedScript{ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.TextResponse("ok"), types.UsageDelta{PromptTokens: 10, CompletionTokens: 1}),
	}}, model: "main"}
	budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(1), PerCallTokens: 100_000})
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: main}, WithBudget(budget),
		WithInputGuardrails(InputGuardrail{Guardrail: check}))
	tr, _, err := collectWithDeltas(t, a, "q")
	if err != nil {
		t.Fatal(err)
	}
	var charged types.Report
	for _, r := range budget.Breakdown() {
		if r.Model == "cheap" {
			charged = r
		}
	}
	if charged.Usage.Requests != 1 || charged.Cost <= 0 {
		t.Errorf("classifier charge = %+v, want one priced request", charged)
	}
	if tr.Usage.PromptTokens < 1000 {
		t.Errorf("classifier usage not reported: %+v", tr.Usage)
	}
}

// A guardrail verdict is recorded under a durable runner: the replay applies
// it without calling the guardrail.
func TestGuardrailVerdictReplaysFromRecord(t *testing.T) {
	runner := newRecordingRunner()
	var calls atomic.Int32
	redact := NewGuardrail("redact", func(_ context.Context, in GuardrailInput) (GuardrailVerdict, error) {
		calls.Add(1)
		return Rewrite(strings.ReplaceAll(in.Text, "42", "[n]"), "number"), nil
	})
	input := []types.Message{types.UserMsg(types.Text("q"))}
	first := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "it is 42"}},
		WithOutputGuardrails(OutputGuardrail{Guardrail: redact}))
	final, err := first.RunDurable(context.Background(), runner, input, "")
	if err != nil || contentText(final.Parts) != "it is [n]" {
		t.Fatalf("final %v err %v", final, err)
	}
	other := NewGuardrail("redact", func(context.Context, GuardrailInput) (GuardrailVerdict, error) {
		calls.Add(1)
		return Block("would block now"), nil
	})
	replay := NewAgent(AgentConfig{SystemPrompt: "s", Provider: panicProvider{}},
		WithOutputGuardrails(OutputGuardrail{Guardrail: other}))
	final, err = replay.RunDurable(context.Background(), runner, input, "")
	if err != nil || contentText(final.Parts) != "it is [n]" {
		t.Fatalf("replayed final %v err %v", final, err)
	}
	if calls.Load() != 1 {
		t.Errorf("guardrail called %d times, want once", calls.Load())
	}
}

func TestSubmittedMessagesPassInputGuardrails(t *testing.T) {
	release := make(chan struct{})
	p := &gatedProvider{release: release, text: "first"}
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p},
		WithInputGuardrails(InputGuardrail{Guardrail: blockWord("secret"), Mode: GuardrailParallel}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("hello"))})
	if _, err := stream.Submit(types.UserMsg(types.Text("now the secret")), SubmitQueue); err != nil {
		t.Fatal(err)
	}
	close(release)
	_, err := Collect(stream, nil)
	if !errors.Is(err, ErrGuardrailTripped) {
		t.Fatalf("err = %v, want the submitted message blocked", err)
	}
}

// gatedProvider answers once release is closed.
type gatedProvider struct {
	release chan struct{}
	text    string
}

func (p *gatedProvider) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 3)
	go func() {
		defer close(ch)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		for _, d := range agenttest.TextResponse(p.text) {
			ch <- d
		}
	}()
	return ch, nil
}
