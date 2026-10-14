package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// probeProvider is a stepProvider that keeps the context of every call, so a
// test can tell that a stop cancelled the provider stream.
type probeProvider struct {
	*stepProvider
	mu   sync.Mutex
	ctxs []context.Context
}

func newProbeProvider(calls ...stepCall) *probeProvider {
	return &probeProvider{stepProvider: newStepProvider(calls...)}
}

func (p *probeProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	p.mu.Lock()
	p.ctxs = append(p.ctxs, ctx)
	p.mu.Unlock()
	return p.stepProvider.Stream(ctx, req)
}

func (p *probeProvider) callCtx(i int) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctxs[i]
}

// stopTool is a tool whose behaviour a test scripts: it reports when it
// starts, then returns at once, waits for its context, or waits for release
// while ignoring its context.
type stopTool struct {
	name     string
	mode     string // "ok", "block" or "ignore"
	started  chan struct{}
	release  chan struct{}
	calls    atomic.Int32
	canceled atomic.Bool
}

func newStopTool(name, mode string) *stopTool {
	return &stopTool{name: name, mode: mode, started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (t *stopTool) Definition() types.ToolDef { return types.ToolDef{Name: t.name} }

func (t *stopTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	t.calls.Add(1)
	t.started <- struct{}{}
	switch t.mode {
	case "block":
		<-ctx.Done()
		t.canceled.Store(true)
		return "", ctx.Err()
	case "ignore":
		<-t.release
	}
	return t.name + " done", nil
}

// toolTurn streams one assistant turn that calls each named tool once. The
// call IDs are "c-" plus the tool name.
func toolTurn(names ...string) []types.Delta {
	var out []types.Delta
	for i, n := range names {
		id := "c-" + n
		out = append(out,
			types.PartStart{Index: i, Kind: types.KindToolCall, ID: id, Name: n},
			types.PartEnd{Index: i, Part: types.ToolCallPart{ID: id, Name: n, Arguments: map[string]any{}}},
		)
	}
	return out
}

// branchMessages returns the active branch of a's tree.
func branchMessages(t *testing.T, a *Agent) []types.Message {
	t.Helper()
	msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	return msgs
}

// stopRecordOf returns the StopPart of msg, the note beside it, and whether
// msg is a stop record.
func stopRecordOf(msg types.Message) (types.StopPart, string, bool) {
	um, ok := msg.(types.UserMessage)
	if !ok {
		return types.StopPart{}, "", false
	}
	for _, p := range um.Parts {
		if sp, ok := p.(types.StopPart); ok {
			return sp, textOf(um), true
		}
	}
	return types.StopPart{}, "", false
}

// checkTranscript fails when msgs break a provider's message rules: an
// assistant message with nothing to send, a tool call without a result
// before the next turn, or a result for a call nobody made.
func checkTranscript(t *testing.T, msgs []types.Message) {
	t.Helper()
	open := map[string]bool{}
	closeTurn := func(at int) {
		for id := range open {
			t.Fatalf("message %d: tool call %s has no result", at, id)
		}
	}
	result := func(at int, r types.ToolResultPart) {
		if !open[r.CallID] {
			t.Fatalf("message %d: result for unknown tool call %s", at, r.CallID)
		}
		delete(open, r.CallID)
	}
	for i, m := range msgs {
		switch v := m.(type) {
		case types.AssistantMessage:
			closeTurn(i)
			sendable := 0
			for _, p := range v.Parts {
				switch pv := p.(type) {
				case types.ToolCallPart:
					open[pv.ID] = true
					sendable++
				case types.TextPart:
					if strings.TrimSpace(pv.Text) != "" {
						sendable++
					}
				default:
					if !types.IsMetadata(p) {
						sendable++
					}
				}
			}
			if sendable == 0 {
				t.Fatalf("message %d: empty assistant message %#v", i, v)
			}
		case types.SystemMessage:
			for _, p := range v.Parts {
				if r, ok := p.(types.ToolResultPart); ok {
					result(i, r)
				}
			}
		case types.UserMessage:
			for _, p := range v.Parts {
				if r, ok := p.(types.ToolResultPart); ok {
					result(i, r)
				}
			}
			if _, _, isStop := stopRecordOf(v); !isStop || len(open) > 0 {
				closeTurn(i)
			}
		}
	}
	closeTurn(len(msgs))
}

// stopOutcome is what a consumer saw of a stopped run.
type stopOutcome struct {
	deltas    []types.Delta
	stopped   []types.StoppedDelta
	truncated []types.TruncatedDelta
}

func outcomeOf(deltas []types.Delta) stopOutcome {
	o := stopOutcome{deltas: deltas}
	for _, d := range deltas {
		switch v := d.(type) {
		case types.StoppedDelta:
			o.stopped = append(o.stopped, v)
		case types.TruncatedDelta:
			o.truncated = append(o.truncated, v)
		}
	}
	return o
}

// requireStopped checks the signals every stopped run gives: one
// StoppedDelta with the request, before DoneDelta, no ErrorDelta, a nil
// Wait, and the request from Stopped.
func requireStopped(t *testing.T, stream *EventStream, deltas []types.Delta, want StopRequest) types.StoppedDelta {
	t.Helper()
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait = %v, want nil for a stopped run", err)
	}
	got, ok := stream.Stopped()
	if !ok || got != want {
		t.Fatalf("Stopped() = %+v, %v; want %+v, true", got, ok, want)
	}
	o := outcomeOf(deltas)
	if len(o.stopped) != 1 {
		t.Fatalf("StoppedDelta count = %d, want 1: %v", len(o.stopped), deltas)
	}
	sd := o.stopped[0]
	if sd.Reason != want.Reason || sd.By != want.By || sd.NodeID == "" {
		t.Fatalf("StoppedDelta = %+v, want reason %q by %q and a node", sd, want.Reason, want.By)
	}
	si := indexOf(deltas, func(d types.Delta) bool { _, ok := d.(types.StoppedDelta); return ok })
	di := indexOf(deltas, func(d types.Delta) bool { _, ok := d.(types.DoneDelta); return ok })
	if di != len(deltas)-1 || si > di {
		t.Fatalf("want StoppedDelta before a final DoneDelta: %v", deltas)
	}
	if indexOf(deltas, func(d types.Delta) bool { _, ok := d.(types.ErrorDelta); return ok }) >= 0 {
		t.Fatalf("a stopped run reported an error: %v", deltas)
	}
	return sd
}

// requireStopRecord checks that msgs ends with the stop record for want and
// returns the messages before it.
func requireStopRecord(t *testing.T, msgs []types.Message, want StopRequest) []types.Message {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("empty branch")
	}
	sp, note, ok := stopRecordOf(msgs[len(msgs)-1])
	if !ok {
		t.Fatalf("branch does not end with a stop record: %#v", msgs[len(msgs)-1])
	}
	if sp.Reason != want.Reason || sp.By != want.By {
		t.Fatalf("StopPart = %+v, want %+v", sp, want)
	}
	if !strings.Contains(note, want.Reason) {
		t.Fatalf("stop note %q does not carry the reason", note)
	}
	if want.By != "" && strings.Contains(note, want.By) {
		t.Fatalf("stop note %q names who stopped the run", note)
	}
	return msgs[:len(msgs)-1]
}

func TestStopMidStreamKeepsPartialTextAndReportsReason(t *testing.T) {
	req := StopRequest{Reason: "Stopped by a user", By: "user-42"}
	tests := []struct {
		name     string
		before   []types.Delta
		wantText string // committed partial text; "" means no assistant message
	}{
		{
			name:     "partial text is kept",
			before:   []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "half an ans"}},
			wantText: "half an ans",
		},
		{
			name: "text is kept and the turn's tool calls never run",
			before: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "let me look"}, types.PartEnd{Index: 0},
				types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "done", Name: "echo"},
				types.PartEnd{Index: 1, Part: types.ToolCallPart{ID: "done", Name: "echo", Arguments: map[string]any{}}},
				types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "open", Name: "echo"}, types.PartDelta{Index: 2, Args: `{"a":`},
			},
			wantText: "let me look",
		},
		{
			name:   "before the first token nothing is committed",
			before: nil,
		},
		{
			name:   "whitespace alone is not committed as an assistant message",
			before: []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: " \n"}},
		},
		{
			name:   "open thinking alone is not committed",
			before: []types.Delta{types.PartStart{Index: 0, Kind: types.KindThinking}, types.PartDelta{Index: 0, Thinking: "hmm"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newProbeProvider(stepCall{before: tt.before, hold: make(chan struct{})})
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "echo"}, Result: "ok"}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			stopped := false
			stop := func() {
				stopped = true
				if err := stream.Stop(req); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			if len(tt.before) == 0 {
				waitStarted(t, provider.stepProvider, 0)
				stop()
			}
			var got []types.Delta
			// The stream forwards the held call's deltas in order, so the
			// stop is sent once all of them have been aggregated.
			for d := range stream.Deltas() {
				got = append(got, d)
				if !stopped && len(got) == len(tt.before) {
					stop()
				}
			}
			requireStopped(t, stream, got, req)

			if err := provider.callCtx(0).Err(); err == nil {
				t.Fatal("the provider stream was not cancelled")
			}
			if n := len(provider.requests()); n != 1 {
				t.Fatalf("provider calls = %d, want 1", n)
			}
			if tool.CallCount() != 0 {
				t.Fatal("a tool call from the stopped turn ran")
			}
			o := outcomeOf(got)
			if len(o.truncated) != 1 || o.truncated[0].Reason != "stopped" || (o.truncated[0].NodeID != "") != (tt.wantText != "") {
				t.Fatalf("TruncatedDelta = %+v, want one with reason stopped", o.truncated)
			}

			msgs := branchMessages(t, a)
			checkTranscript(t, msgs)
			rest := requireStopRecord(t, msgs, req)
			last := rest[len(rest)-1]
			if tt.wantText == "" {
				if _, ok := last.(types.UserMessage); !ok || textOf(last) != "start" {
					t.Fatalf("want the input before the stop record, got %#v", last)
				}
				return
			}
			am, ok := last.(types.AssistantMessage)
			if !ok || textOf(am) != tt.wantText {
				t.Fatalf("partial turn = %#v, want text %q", last, tt.wantText)
			}
			marked := false
			for _, p := range am.Parts {
				if tp, ok := p.(types.TruncationPart); ok && tp.Reason == "stopped" {
					marked = true
				}
				if _, ok := p.(types.ToolCallPart); ok {
					t.Fatalf("a tool call was kept on the stopped turn: %#v", am)
				}
			}
			if !marked {
				t.Fatalf("partial turn has no stopped marker: %#v", am)
			}
		})
	}
}

func TestStopReportsUsageAndGuardsTheKeptText(t *testing.T) {
	req := StopRequest{Reason: "Stopped by a user"}
	tests := []struct {
		name string
		// verdict is what the output guardrail says about the partial text.
		verdict  GuardrailVerdict
		wantText string // committed text; "" means the partial turn is dropped
	}{
		{name: "passed text is kept", verdict: GuardrailVerdict{}, wantText: "the secret is"},
		{name: "rewritten text is kept as rewritten", verdict: Rewrite("the [redacted] is", "pii"), wantText: "the [redacted] is"},
		{name: "blocked text is dropped and the run still ends stopped", verdict: Block("pii")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "the secret is"}}
			provider := newProbeProvider(stepCall{before: before, hold: make(chan struct{})})
			guard := NewGuardrail("pii", func(context.Context, GuardrailInput) (GuardrailVerdict, error) { return tt.verdict, nil })
			a := must.Get(New(Config{Provider: provider}, WithOutputGuardrails(OutputGuardrail{Guardrail: guard})))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			var got []types.Delta
			for d := range stream.Deltas() {
				got = append(got, d)
				if len(got) == len(before) {
					if err := stream.Stop(req); err != nil {
						t.Fatalf("Stop: %v", err)
					}
				}
			}
			requireStopped(t, stream, got, req)

			usage := indexOf(got, func(d types.Delta) bool {
				u, ok := d.(types.UsageDelta)
				return ok && u.PromptTokens > 0 && u.CompletionTokens > 0
			})
			if usage < 0 {
				t.Fatalf("the stopped call reported no usage: %v", got)
			}

			msgs := branchMessages(t, a)
			checkTranscript(t, msgs)
			rest := requireStopRecord(t, msgs, req)
			var kept string
			for _, m := range rest {
				if am, ok := m.(types.AssistantMessage); ok {
					kept = textOf(am)
				}
			}
			if kept != tt.wantText {
				t.Fatalf("kept text = %q, want %q", kept, tt.wantText)
			}
		})
	}
}

func TestStopDuringToolCallsClosesEveryCallOut(t *testing.T) {
	req := StopRequest{Reason: "Stopped by a user"}
	stopText := stoppedToolText(req)
	tests := []struct {
		name string
		// tools are called in one turn, in this order: name -> mode.
		tools [][2]string
		opts  []Option
		// waitFor is the tool whose start triggers the stop.
		waitFor string
		// releaseAfterStop lets a tool that ignores its context return.
		releaseAfterStop string
		// want maps each tool to its recorded result: "stopped", or the
		// tool's own text.
		want map[string]string
		// ran is how often each tool executed.
		ran map[string]int32
	}{
		{
			name:    "a running tool is cancelled through its context",
			tools:   [][2]string{{"slow", "block"}},
			waitFor: "slow",
			want:    map[string]string{"slow": "stopped"},
			ran:     map[string]int32{"slow": 1},
		},
		{
			name:    "a pending tool call is never started",
			tools:   [][2]string{{"slow", "block"}, {"next", "ok"}},
			opts:    []Option{WithSequentialTools()},
			waitFor: "slow",
			want:    map[string]string{"slow": "stopped", "next": "stopped"},
			ran:     map[string]int32{"slow": 1, "next": 0},
		},
		{
			name:    "a call that finished before the stop keeps its result",
			tools:   [][2]string{{"quick", "ok"}, {"slow", "block"}},
			opts:    []Option{WithSequentialTools()},
			waitFor: "slow",
			want:    map[string]string{"quick": "quick done", "slow": "stopped"},
			ran:     map[string]int32{"quick": 1, "slow": 1},
		},
		{
			name:             "a tool that ignores its context keeps the result it produced",
			tools:            [][2]string{{"deaf", "ignore"}, {"slow", "block"}},
			waitFor:          "slow",
			releaseAfterStop: "deaf",
			want:             map[string]string{"deaf": "deaf done", "slow": "stopped"},
			ran:              map[string]int32{"deaf": 1, "slow": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := map[string]*stopTool{}
			var names []string
			registry := types.NewToolRegistry()
			for _, spec := range tt.tools {
				st := newStopTool(spec[0], spec[1])
				tools[spec[0]] = st
				names = append(names, spec[0])
				registry.Register(st)
			}
			provider := newProbeProvider(stepCall{before: toolTurn(names...)})
			a := must.Get(New(Config{Provider: provider, Tools: registry}, tt.opts...))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			if tt.releaseAfterStop != "" {
				<-tools[tt.releaseAfterStop].started
			}
			select {
			case <-tools[tt.waitFor].started:
			case <-ctx.Done():
				t.Fatalf("tool %s never started", tt.waitFor)
			}
			if err := stream.Stop(req); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if tt.releaseAfterStop != "" {
				close(tools[tt.releaseAfterStop].release)
			}
			got := <-deltas
			requireStopped(t, stream, got, req)

			if n := len(provider.requests()); n != 1 {
				t.Fatalf("provider calls = %d, want 1: the model must not see the results in this run", n)
			}
			for name, want := range tt.ran {
				if n := tools[name].calls.Load(); n != want {
					t.Errorf("tool %s ran %d times, want %d", name, n, want)
				}
			}
			if !tools[tt.waitFor].canceled.Load() {
				t.Errorf("tool %s did not see its context cancelled", tt.waitFor)
			}

			msgs := branchMessages(t, a)
			checkTranscript(t, msgs)
			requireStopRecord(t, msgs, req)
			results := map[string]types.ToolResultPart{}
			for _, m := range msgs {
				for _, r := range types.Each[types.ToolResultPart](m) {
					results[r.CallID] = r
				}
			}
			ends := map[string]types.ToolExecEndDelta{}
			for _, d := range got {
				if e, ok := d.(types.ToolExecEndDelta); ok {
					ends[e.ToolCallID] = e
				}
			}
			for name, want := range tt.want {
				r, ok := results["c-"+name]
				if !ok {
					t.Fatalf("tool call %s has no result", name)
				}
				end, ok := ends["c-"+name]
				if !ok {
					t.Fatalf("tool call %s has no ToolExecEndDelta", name)
				}
				if want == "stopped" {
					if !r.IsError || r.Text() != stopText {
						t.Errorf("result of %s = %q (error %v), want the stop result %q", name, r.Text(), r.IsError, stopText)
					}
					if end.Error != stopText {
						t.Errorf("ToolExecEndDelta of %s has error %q, want %q", name, end.Error, stopText)
					}
					continue
				}
				if r.IsError || r.Text() != want {
					t.Errorf("result of %s = %q (error %v), want %q", name, r.Text(), r.IsError, want)
				}
			}
		})
	}
}

func TestStoppedHistoryIsAcceptedForFollowUpTurn(t *testing.T) {
	req := StopRequest{Reason: "Arrêté par un utilisateur", By: "user-42"}
	tests := []struct {
		name string
		// first is the held provider call of the stopped run.
		first stepCall
		// tool, when set, is called by the first turn and blocks.
		tool bool
		// wantRoles is the role sequence the follow-up request carries.
		wantRoles []types.Role
	}{
		{
			name: "after a stop mid text",
			first: stepCall{hold: make(chan struct{}), before: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "The capital of France is"},
			}},
			wantRoles: []types.Role{types.RoleUser, types.RoleAssistant, types.RoleUser, types.RoleUser},
		},
		{
			name:      "after a stop before the first token",
			first:     stepCall{hold: make(chan struct{})},
			wantRoles: []types.Role{types.RoleUser, types.RoleUser, types.RoleUser},
		},
		{
			name:      "after a stop during a tool call",
			first:     stepCall{before: toolTurn("slow")},
			tool:      true,
			wantRoles: []types.Role{types.RoleUser, types.RoleAssistant, types.RoleSystem, types.RoleUser, types.RoleUser},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newProbeProvider(tt.first, stepCall{before: agenttest.TextResponse("Paris, as I was saying.")})
			slow := newStopTool("slow", "block")
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(slow)}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("capital of France?"))})
			var first []types.Delta
			stopped := false
			stop := func() {
				stopped = true
				if err := stream.Stop(req); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			switch {
			case tt.tool:
				go func() {
					<-slow.started
					_ = stream.Stop(req)
				}()
				stopped = true
			case len(tt.first.before) == 0:
				waitStarted(t, provider.stepProvider, 0)
				stop()
			}
			for d := range stream.Deltas() {
				first = append(first, d)
				if !stopped && len(first) == len(tt.first.before) {
					stop()
				}
			}
			requireStopped(t, stream, first, req)

			// The branch is free at once: the next turn starts without waiting.
			text, err := CollectText(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go on"))}))
			if err != nil {
				t.Fatalf("follow-up turn: %v", err)
			}
			if text != "Paris, as I was saying." {
				t.Fatalf("follow-up answer = %q", text)
			}

			reqs := provider.requests()
			if len(reqs) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(reqs))
			}
			sent := reqs[1]
			// The agent's system prompt leads every request; the
			// conversation follows it.
			if len(sent) > 0 && sent[0].Role() == types.RoleSystem {
				sent = sent[1:]
			}
			checkTranscript(t, sent)
			if hasMetadata(sent) {
				t.Fatal("a metadata part reached the provider")
			}
			var roles []types.Role
			var all strings.Builder
			for _, m := range sent {
				roles = append(roles, m.Role())
				all.WriteString(textOf(m))
				for _, r := range types.Each[types.ToolResultPart](m) {
					all.WriteString(r.Text())
				}
			}
			if fmt.Sprint(roles) != fmt.Sprint(tt.wantRoles) {
				t.Fatalf("follow-up request roles = %v, want %v", roles, tt.wantRoles)
			}
			if textOf(sent[len(sent)-1]) != "go on" {
				t.Fatalf("follow-up request ends with %#v", sent[len(sent)-1])
			}
			if !strings.Contains(all.String(), req.Reason) {
				t.Fatalf("the model is not told why the turn stopped: %q", all.String())
			}
			if strings.Contains(all.String(), req.By) {
				t.Fatalf("the model is told who stopped the turn: %q", all.String())
			}

			// The whole conversation still stores and reloads.
			msgs := branchMessages(t, a)
			checkTranscript(t, msgs)
			raw, err := a.Tree().MarshalJSON()
			if err != nil {
				t.Fatalf("marshal tree: %v", err)
			}
			var back tree.Tree
			if err := back.UnmarshalJSON(raw); err != nil {
				t.Fatalf("reload tree: %v", err)
			}
			reloaded, err := back.FlattenBranch(back.Active())
			if err != nil {
				t.Fatalf("flatten reloaded tree: %v", err)
			}
			found := false
			for _, m := range reloaded {
				if sp, _, ok := stopRecordOf(m); ok {
					found = sp.Reason == req.Reason && sp.By == req.By
				}
			}
			if !found {
				t.Fatal("the stop record did not survive a reload")
			}
		})
	}
}

func TestStopIsIdempotentAndWellDefinedAtEveryPoint(t *testing.T) {
	first := StopRequest{Reason: "first reason", By: "id-7781"}
	second := StopRequest{Reason: "second reason", By: "id-9902"}

	t.Run("a second stop is a no-op and the first reason wins", func(t *testing.T) {
		provider := newProbeProvider(stepCall{hold: make(chan struct{})})
		a := must.Get(New(Config{Provider: provider}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("start"))})
		deltas := collect(stream)
		waitStarted(t, provider.stepProvider, 0)
		for i, r := range []StopRequest{first, second, second} {
			if err := stream.Stop(r); err != nil {
				t.Fatalf("Stop %d: %v", i, err)
			}
		}
		got := <-deltas
		requireStopped(t, stream, got, first)
		// Stopping a run that already ended stopped stays a no-op.
		if err := stream.Stop(second); err != nil {
			t.Fatalf("Stop after a stopped run: %v", err)
		}
		requireStopped(t, stream, got, first)
		records := 0
		for _, m := range branchMessages(t, a) {
			if _, _, ok := stopRecordOf(m); ok {
				records++
			}
		}
		if records != 1 {
			t.Fatalf("stop records = %d, want 1", records)
		}
	})

	t.Run("a stop after completion changes nothing", func(t *testing.T) {
		provider := newProbeProvider(stepCall{before: agenttest.TextResponse("all done")})
		a := must.Get(New(Config{Provider: provider}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("start"))})
		got := <-collect(stream)
		if err := stream.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		before := len(branchMessages(t, a))
		if err := stream.Stop(first); !errors.Is(err, ErrRunFinished) {
			t.Fatalf("Stop after completion = %v, want ErrRunFinished", err)
		}
		if _, ok := stream.Stopped(); ok {
			t.Fatal("a completed run reports stopped")
		}
		if len(outcomeOf(got).stopped) != 0 {
			t.Fatal("a completed run sent StoppedDelta")
		}
		msgs := branchMessages(t, a)
		if len(msgs) != before {
			t.Fatalf("a late stop changed the branch: %d messages, was %d", len(msgs), before)
		}
		if am, ok := msgs[len(msgs)-1].(types.AssistantMessage); !ok || textOf(am) != "all done" {
			t.Fatalf("branch ends with %#v", msgs[len(msgs)-1])
		}
	})

	t.Run("a stop before the run starts keeps the input and makes no provider call", func(t *testing.T) {
		provider := newProbeProvider(stepCall{before: agenttest.TextResponse("never sent")})
		a := must.Get(New(Config{Provider: provider}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("start"))})
		if err := stream.Stop(first); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		got := <-collect(stream)
		requireStopped(t, stream, got, first)
		rest := requireStopRecord(t, branchMessages(t, a), first)
		// The tree's root holds the system prompt; the input follows it.
		if len(rest) != 2 || textOf(rest[1]) != "start" {
			t.Fatalf("want only the input before the stop record, got %#v", rest)
		}
		// The stop can land before or just after the call is made, never
		// after its answer is recorded.
		if n := len(provider.requests()); n > 1 {
			t.Fatalf("provider calls = %d", n)
		}
	})

	t.Run("streams that cannot wrap up refuse the stop", func(t *testing.T) {
		replayed := Replay(nil)
		for range replayed.Deltas() {
		}
		if err := replayed.Stop(first); !errors.Is(err, ErrStopUnsupported) {
			t.Fatalf("Stop on a replay = %v, want ErrStopUnsupported", err)
		}

		remote := make(chan types.Delta, 2)
		remote <- types.StoppedDelta{Reason: first.Reason, By: first.By, NodeID: "n1"}
		remote <- types.DoneDelta{}
		close(remote)
		rs := NewRemoteStream(remote, nil, nil, nil)
		if err := rs.Stop(second); !errors.Is(err, ErrStopUnsupported) {
			t.Fatalf("Stop on a remote stream = %v, want ErrStopUnsupported", err)
		}
		for range rs.Deltas() {
		}
		if got, ok := rs.Stopped(); !ok || got != first {
			t.Fatalf("remote Stopped() = %+v, %v; want the StoppedDelta it relayed", got, ok)
		}

		provider := newProbeProvider(stepCall{before: agenttest.TextResponse("done")})
		durable := must.Get(New(Config{Provider: provider}, WithStepRunner(newRecordingRunner())))
		ds := durable.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("start"))})
		if err := ds.Stop(first); !errors.Is(err, ErrStopUnsupported) {
			t.Fatalf("Stop under a durable runner = %v, want ErrStopUnsupported", err)
		}
		<-collect(ds)
		if err := ds.Wait(); err != nil {
			t.Fatalf("durable run: %v", err)
		}
	})

	t.Run("an empty reason gets the default", func(t *testing.T) {
		provider := newProbeProvider(stepCall{hold: make(chan struct{})})
		a := must.Get(New(Config{Provider: provider}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("start"))})
		deltas := collect(stream)
		waitStarted(t, provider.stepProvider, 0)
		if err := stream.Stop(StopRequest{Reason: "  "}); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		requireStopped(t, stream, <-deltas, StopRequest{Reason: DefaultStopReason})
	})
}

func TestPlainCancellationIsNotReportedAsStopped(t *testing.T) {
	tests := []struct {
		name string
		// end cancels the run; stopAfter also asks for a stop afterwards.
		end       func(stream *EventStream, cancel context.CancelFunc)
		stopAfter bool
	}{
		{name: "EventStream.Cancel", end: func(s *EventStream, _ context.CancelFunc) { s.Cancel() }},
		{name: "caller context", end: func(_ *EventStream, cancel context.CancelFunc) { cancel() }},
		{name: "cancel first, stop second", end: func(s *EventStream, _ context.CancelFunc) { s.Cancel() }, stopAfter: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newProbeProvider(stepCall{hold: make(chan struct{}), before: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "half"},
			}})
			a := must.Get(New(Config{Provider: provider}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			waitStarted(t, provider.stepProvider, 0)
			tt.end(stream, cancel)
			if tt.stopAfter {
				// The run may or may not have ended yet; either answer is
				// allowed, and neither turns the cancellation into a stop.
				if err := stream.Stop(StopRequest{Reason: "late"}); err != nil && !errors.Is(err, ErrRunFinished) {
					t.Fatalf("Stop after Cancel: %v", err)
				}
			}
			got := <-deltas
			if err := stream.Wait(); !errors.Is(err, types.ErrStreamCanceled) {
				t.Fatalf("Wait = %v, want ErrStreamCanceled", err)
			}
			if _, ok := stream.Stopped(); ok {
				t.Fatal("a cancelled run reports stopped")
			}
			if len(outcomeOf(got).stopped) != 0 {
				t.Fatal("a cancelled run sent StoppedDelta")
			}
			for _, m := range branchMessages(t, a) {
				if _, _, ok := stopRecordOf(m); ok {
					t.Fatal("a cancelled run recorded a stop")
				}
			}
		})
	}
}

func TestStopFromManyGoroutinesIsRaceFreeAndLeaksNothing(t *testing.T) {
	// Let goroutines of earlier tests settle before taking the baseline.
	baseline := settledGoroutines(t, 0)

	const runs = 24
	for i := range runs {
		tools := types.NewToolRegistry(newStopTool("slow", "block"))
		var first stepCall
		switch i % 3 {
		case 0: // stop lands mid text
			first = stepCall{hold: make(chan struct{}), before: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "half"},
			}}
		case 1: // stop lands during a tool call
			first = stepCall{before: toolTurn("slow")}
		case 2: // stop races a run that completes on its own
			first = stepCall{before: agenttest.TextResponse("done")}
		}
		provider := newProbeProvider(first)
		a := must.Get(New(Config{Provider: provider, Tools: tools}))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
		deltas := collect(stream)

		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if g%2 == 0 {
					runtime.Gosched()
				}
				err := stream.Stop(StopRequest{Reason: fmt.Sprintf("reason %d", g), By: "id-7781"})
				if err != nil && !errors.Is(err, ErrRunFinished) {
					t.Errorf("run %d: Stop = %v", i, err)
				}
				stream.Stopped()
			}()
		}
		wg.Wait()
		got := <-deltas
		cancel()

		// Whichever way the race went, the run ended cleanly and the two
		// reports of its outcome agree.
		if err := stream.Wait(); err != nil {
			t.Fatalf("run %d: Wait = %v", i, err)
		}
		req, stopped := stream.Stopped()
		sent := outcomeOf(got).stopped
		if stopped != (len(sent) == 1) || len(sent) > 1 {
			t.Fatalf("run %d: Stopped() = %v but %d StoppedDelta", i, stopped, len(sent))
		}
		msgs := branchMessages(t, a)
		checkTranscript(t, msgs)
		if stopped {
			if sent[0].Reason != req.Reason {
				t.Fatalf("run %d: delta reason %q, Stopped reason %q", i, sent[0].Reason, req.Reason)
			}
			requireStopRecord(t, msgs, req)
		}
		if i%3 != 2 && !stopped {
			t.Fatalf("run %d: a held run was not stopped", i)
		}
	}

	if after := settledGoroutines(t, baseline); after > baseline {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after\n%s", baseline, after, buf[:runtime.Stack(buf, true)])
	}
}

// settledGoroutines returns the goroutine count once it stops falling, or
// as soon as it is at most want.
func settledGoroutines(t *testing.T, want int) int {
	t.Helper()
	n := runtime.NumGoroutine()
	for range 200 {
		if n <= want {
			return n
		}
		time.Sleep(10 * time.Millisecond)
		next := runtime.NumGoroutine()
		if want == 0 && next >= n {
			return next
		}
		n = next
	}
	return n
}
