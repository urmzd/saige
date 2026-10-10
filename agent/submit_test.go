package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// stepCall scripts one provider call: before is streamed at once, then the
// call waits for hold (when set) or cancellation, then after is streamed.
type stepCall struct {
	before []types.Delta
	hold   chan struct{}
	after  []types.Delta
}

// stepProvider answers call i with calls[i] and records every request.
type stepProvider struct {
	mu      sync.Mutex
	calls   []stepCall
	reqs    [][]types.Message
	started chan int
}

func newStepProvider(calls ...stepCall) *stepProvider {
	return &stepProvider{calls: calls, started: make(chan int, 32)}
}

func (p *stepProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	messages := req.Messages
	p.mu.Lock()
	idx := len(p.reqs)
	p.reqs = append(p.reqs, append([]types.Message(nil), messages...))
	var call stepCall
	if idx < len(p.calls) {
		call = p.calls[idx]
	} else {
		call = stepCall{before: agenttest.TextResponse("done")}
	}
	p.mu.Unlock()
	p.started <- idx
	ch := make(chan types.Delta, 64)
	go func() {
		defer close(ch)
		for _, d := range call.before {
			ch <- d
		}
		if call.hold != nil {
			select {
			case <-call.hold:
			case <-ctx.Done():
				return
			}
		}
		for _, d := range call.after {
			ch <- d
		}
	}()
	return ch, nil
}

func (p *stepProvider) requests() [][]types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]types.Message(nil), p.reqs...)
}

// prefillProvider is a stepProvider that declares assistant prefill.
type prefillProvider struct{ *stepProvider }

func (prefillProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{}.With(types.CapAssistantPrefill)
}

func textOf(msg types.Message) string {
	var b strings.Builder
	switch m := msg.(type) {
	case types.UserMessage:
		for _, c := range m.Parts {
			if tc, ok := c.(types.TextPart); ok {
				b.WriteString(tc.Text)
			}
		}
	case types.AssistantMessage:
		for _, c := range m.Parts {
			if tc, ok := c.(types.TextPart); ok {
				b.WriteString(tc.Text)
			}
		}
	}
	return b.String()
}

// waitStarted waits for provider call idx to begin.
func waitStarted(t *testing.T, p *stepProvider, idx int) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-p.started:
			if got == idx {
				return
			}
		case <-timeout:
			t.Fatalf("provider call %d never started", idx)
		}
	}
}

func hasMetadata(msgs []types.Message) bool {
	for _, m := range msgs {
		switch v := m.(type) {
		case types.UserMessage:
			for _, c := range v.Parts {
				if types.IsMetadata(c) {
					return true
				}
			}
		case types.AssistantMessage:
			for _, c := range v.Parts {
				if types.IsMetadata(c) {
					return true
				}
			}
		}
	}
	return false
}

func collect(s *EventStream) <-chan []types.Delta {
	out := make(chan []types.Delta, 1)
	go func() { out <- agenttest.CollectDeltas(s.Deltas()) }()
	return out
}

func indexOf(deltas []types.Delta, match func(types.Delta) bool) int {
	for i, d := range deltas {
		if match(d) {
			return i
		}
	}
	return -1
}

func TestSubmitQueueAndSteer(t *testing.T) {
	tests := []struct {
		name string
		mode SubmitMode
		// toolTurn makes the held call a tool call, so a steering message
		// lands after its result instead of at the end of the run.
		toolTurn bool
		// wantRequests is the number of provider calls the run makes.
		wantRequests int
	}{
		{name: "queue waits for the natural finish", mode: SubmitQueue, wantRequests: 2},
		{name: "steer after tool results", mode: SubmitSteer, toolTurn: true, wantRequests: 2},
		{name: "steer on a text-only turn joins at the finish", mode: SubmitSteer, wantRequests: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := make(chan struct{})
			first := stepCall{hold: hold, after: agenttest.TextResponse("first")}
			if tt.toolTurn {
				first.after = agenttest.ToolCallResponse("c1", "echo", map[string]any{})
			}
			provider := newStepProvider(first, stepCall{before: agenttest.TextResponse("second")})
			reg := types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "echo"}, Result: "ok"})
			a := must.Get(New(Config{Provider: provider, Tools: reg}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			waitStarted(t, provider, 0)
			id, err := stream.Submit(types.UserMsg(types.Text("extra")), tt.mode)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			close(hold)
			got := <-deltas
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}

			queued := indexOf(got, func(d types.Delta) bool {
				q, ok := d.(types.QueuedDelta)
				return ok && q.SubmissionID == string(id) && q.Mode == tt.mode.String() && q.Position == 1
			})
			injected := indexOf(got, func(d types.Delta) bool {
				in, ok := d.(types.InjectedDelta)
				return ok && in.SubmissionID == string(id) && in.NodeID != ""
			})
			if queued < 0 || injected < 0 || queued > injected {
				t.Fatalf("queued at %d, injected at %d: %v", queued, injected, got)
			}

			reqs := provider.requests()
			if len(reqs) != tt.wantRequests {
				t.Fatalf("requests = %d, want %d", len(reqs), tt.wantRequests)
			}
			last := reqs[len(reqs)-1]
			if textOf(last[len(last)-1]) != "extra" {
				t.Fatalf("last request ends with %#v, want the submitted message", last[len(last)-1])
			}
			if tt.toolTurn {
				if _, ok := last[len(last)-2].(types.SystemMessage); !ok {
					t.Fatalf("steer must follow the tool result, got %#v", last[len(last)-2])
				}
			}
			if hasMetadata(last) {
				t.Fatal("submission markers reached the provider")
			}
			if len(stream.Undelivered()) != 0 {
				t.Fatalf("Undelivered = %v", stream.Undelivered())
			}
		})
	}
}

func TestSubmitInterruptReplace(t *testing.T) {
	tests := []struct {
		name     string
		before   []types.Delta
		wantText string // committed partial text; "" means nothing committed
	}{
		{
			name:     "partial text is kept",
			before:   []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "half an ans"}},
			wantText: "half an ans",
		},
		{
			name: "tool calls are dropped",
			before: []types.Delta{
				types.PartStart{Index: 1, Kind: types.KindText}, types.PartDelta{Index: 1, Text: "let me look"}, types.PartEnd{Index: 1},
				types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "done", Name: "echo"}, types.PartEnd{Index: 2, Part: types.ToolCallPart{ID: "done", Name: "echo", Arguments: map[string]any{}}},
				types.PartStart{Index: 3, Kind: types.KindToolCall, ID: "open", Name: "echo"}, types.PartDelta{Index: 3, Args: `{"a":`},
			},
			wantText: "let me look",
		},
		{
			name:   "nothing streamed yet",
			before: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStepProvider(
				stepCall{before: tt.before, hold: make(chan struct{})},
				stepCall{before: agenttest.TextResponse("replaced")},
			)
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "echo"}, Result: "ok"}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			var got []types.Delta
			var id SubmissionID
			submit := func() {
				var err error
				if id, err = stream.Submit(types.UserMsg(types.Text("new direction")), SubmitInterruptReplace); err != nil {
					t.Fatalf("Submit: %v", err)
				}
			}
			if len(tt.before) == 0 {
				waitStarted(t, provider, 0)
				submit()
			}
			// The stream forwards the held call's deltas in order, so the
			// interrupt is sent once all of them have been aggregated.
			for d := range stream.Deltas() {
				got = append(got, d)
				if id == "" && len(got) == len(tt.before) {
					submit()
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if tool.CallCount() != 0 {
				t.Fatal("a tool call from the interrupted turn ran")
			}

			interrupted := indexOf(got, func(d types.Delta) bool {
				in, ok := d.(types.InterruptedDelta)
				return ok && in.SubmissionID == string(id)
			})
			var truncated types.TruncatedDelta
			ti := indexOf(got, func(d types.Delta) bool {
				td, ok := d.(types.TruncatedDelta)
				truncated = td
				return ok
			})
			injected := indexOf(got, func(d types.Delta) bool {
				in, ok := d.(types.InjectedDelta)
				return ok && in.SubmissionID == string(id) && in.Mode == "interrupt"
			})
			if interrupted < 0 || ti < interrupted || injected < ti {
				t.Fatalf("want Interrupted, Truncated, Injected in order: %v", got)
			}
			if truncated.Reason != "interrupted" || (truncated.NodeID != "") != (tt.wantText != "") {
				t.Fatalf("TruncatedDelta = %+v", truncated)
			}

			reqs := provider.requests()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want 2", len(reqs))
			}
			second := reqs[1]
			if textOf(second[len(second)-1]) != "new direction" {
				t.Fatalf("second request ends with %#v", second[len(second)-1])
			}
			if hasMetadata(second) {
				t.Fatal("truncation marker reached the provider")
			}
			// The committed branch must still store and reload.
			raw, err := a.Tree().MarshalJSON()
			if err != nil {
				t.Fatalf("marshal tree: %v", err)
			}
			if err := new(tree.Tree).UnmarshalJSON(raw); err != nil {
				t.Fatalf("reload tree: %v", err)
			}
			if tt.wantText != "" {
				am, ok := second[len(second)-2].(types.AssistantMessage)
				if !ok || textOf(am) != tt.wantText || len(assistantToolCalls(&am)) != 0 {
					t.Fatalf("partial turn = %#v, want text %q and no tool calls", second[len(second)-2], tt.wantText)
				}
			}
		})
	}
}

func TestSubmitInterruptBetweenCallsWaitsForTools(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	tool := &blockingTool{started: started, release: release}
	provider := newStepProvider(
		stepCall{before: agenttest.ToolCallResponse("c1", "block", map[string]any{})},
		stepCall{before: agenttest.TextResponse("after")},
	)
	a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
	deltas := collect(stream)
	<-started
	if _, err := stream.Submit(types.UserMsg(types.Text("stop that")), SubmitInterruptReplace); err != nil {
		t.Fatal(err)
	}
	close(release)
	got := <-deltas
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if indexOf(got, func(d types.Delta) bool { _, ok := d.(types.InterruptedDelta); return ok }) >= 0 {
		t.Fatal("no provider call was in flight, so nothing is interrupted")
	}
	reqs := provider.requests()
	second := reqs[1]
	if _, ok := second[len(second)-2].(types.SystemMessage); !ok || textOf(second[len(second)-1]) != "stop that" {
		t.Fatal("the running tool must finish and its result precede the message")
	}
}

type blockingTool struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingTool) Definition() types.ToolDef { return types.ToolDef{Name: "block"} }
func (b *blockingTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return "finished", nil
}

func TestSubmitErrors(t *testing.T) {
	provider := newStepProvider(stepCall{before: agenttest.TextResponse("hi")})
	a := must.Get(New(Config{Provider: provider}))
	ctx := context.Background()
	finished := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
	for range finished.Deltas() {
	}
	_ = finished.Wait()

	tests := []struct {
		name   string
		stream *EventStream
		msg    types.UserMessage
		mode   SubmitMode
		want   error
	}{
		{"finished run", finished, types.UserMsg(types.Text("late")), SubmitQueue, ErrRunFinished},
		{"replay stream", Replay(nil), types.UserMsg(types.Text("x")), SubmitQueue, ErrSubmitUnsupported},
		{"side on a stream", finished, types.UserMsg(types.Text("x")), SubmitSide, ErrSubmitUnsupported},
		{"empty message", finished, types.UserMessage{}, SubmitQueue, ErrInvalidSubmission},
		{"unknown mode", finished, types.UserMsg(types.Text("x")), SubmitMode(42), ErrInvalidSubmission},
		{"tool result", finished, types.UserMessage{Parts: []types.UserPart{types.ToolResultPart{CallID: "c"}}}, SubmitSteer, ErrInvalidSubmission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.stream.Submit(tt.msg, tt.mode); !errors.Is(err, tt.want) {
				t.Fatalf("Submit = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestAgentSubmit(t *testing.T) {
	hold := make(chan struct{})
	provider := newStepProvider(
		stepCall{hold: hold, after: agenttest.TextResponse("first")},
		stepCall{before: agenttest.TextResponse("second")},
		stepCall{before: agenttest.TextResponse("third")},
	)
	a := must.Get(New(Config{Provider: provider}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	main := a.Tree().Active()

	// Idle branch: a new run starts with the message as input.
	stream, _, err := a.Submit(ctx, "", types.UserMsg(types.Text("one")), SubmitQueue)
	if err != nil {
		t.Fatal(err)
	}
	deltas := collect(stream)
	waitStarted(t, provider, 0)

	// Active branch: the message joins the same run.
	joined, id, err := a.Submit(ctx, main, types.UserMsg(types.Text("two")), SubmitQueue)
	if err != nil || joined != stream || id == "" {
		t.Fatalf("Submit to active run = %v, %v, same stream %v", id, err, joined == stream)
	}
	close(hold)
	<-deltas
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}

	// Finished branch: a new run starts again.
	next, _, err := a.Submit(ctx, main, types.UserMsg(types.Text("three")), SubmitSteer)
	if err != nil || next == stream {
		t.Fatalf("Submit after finish = %v, new stream %v", err, next != stream)
	}
	for range next.Deltas() {
	}
	if err := next.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := len(provider.requests()); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestAgentSubmitSide(t *testing.T) {
	tests := []struct {
		name string
		// toolTip leaves the main branch on an unanswered tool call while the
		// side run starts.
		toolTip bool
	}{
		{name: "from the tip"},
		{name: "from before an unanswered tool call", toolTip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{})
			hold := make(chan struct{})
			first := stepCall{hold: hold, after: agenttest.TextResponse("main answer")}
			if tt.toolTip {
				first = stepCall{before: agenttest.ToolCallResponse("c1", "block", map[string]any{})}
			}
			provider := newStepProvider(first, stepCall{before: agenttest.TextResponse("side answer")}, stepCall{before: agenttest.TextResponse("main done")})
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(&blockingTool{started: started, release: release})}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			main := a.Tree().Active()

			mainStream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("main task"))})
			mainDeltas := collect(mainStream)
			if tt.toolTip {
				<-started
			} else {
				waitStarted(t, provider, 0)
			}
			mainTip, _ := a.Tree().Tip(main)

			side, _, err := a.Submit(ctx, main, types.UserMsg(types.Text("aside")), SubmitSide)
			if err != nil {
				t.Fatal(err)
			}
			for range side.Deltas() {
			}
			if err := side.Wait(); err != nil {
				t.Fatal(err)
			}
			if after, _ := a.Tree().Tip(main); after.ID != mainTip.ID {
				t.Fatal("the side run wrote to the main branch")
			}
			sideMsgs, err := a.Tree().FlattenBranch(side.branch)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range sideMsgs {
				if am, ok := m.(types.AssistantMessage); ok && len(assistantToolCalls(&am)) > 0 {
					t.Fatal("the side branch holds an unanswered tool call")
				}
			}
			if textOf(sideMsgs[len(sideMsgs)-1]) != "side answer" {
				t.Fatalf("side branch ends with %#v", sideMsgs[len(sideMsgs)-1])
			}

			close(hold)
			close(release)
			<-mainDeltas
			if err := mainStream.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func maxTokensResponse(text string) []types.Delta {
	return append(agenttest.TextResponse(text), types.UsageDelta{FinishReasons: []string{"max_tokens"}})
}

func TestContinue(t *testing.T) {
	tests := []struct {
		name      string
		prefill   bool
		wantNudge bool
	}{
		{name: "nudge without prefill", wantNudge: true},
		{name: "prefill when declared", prefill: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := newStepProvider(stepCall{before: maxTokensResponse("part one")}, stepCall{before: agenttest.TextResponse(" part two")})
			var provider types.Provider = sp
			if tt.prefill {
				provider = prefillProvider{sp}
			}
			a := must.Get(New(Config{Provider: provider}))
			ctx := context.Background()
			first := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("write"))})
			for range first.Deltas() {
			}
			if err := first.Wait(); err != nil {
				t.Fatal(err)
			}

			cont, err := a.Continue(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			for range cont.Deltas() {
			}
			if err := cont.Wait(); err != nil {
				t.Fatal(err)
			}
			req := sp.requests()[1]
			last := req[len(req)-1]
			if tt.wantNudge {
				if textOf(last) != DefaultContinuePrompt {
					t.Fatalf("request ends with %#v, want the continue prompt", last)
				}
			} else if am, ok := last.(types.AssistantMessage); !ok || textOf(am) != "part one" {
				t.Fatalf("request ends with %#v, want the partial turn", last)
			}
			if hasMetadata(req) {
				t.Fatal("truncation marker reached the provider")
			}
		})
	}
}

func TestContinueRefusals(t *testing.T) {
	ctx := context.Background()
	a := must.Get(New(Config{Provider: newStepProvider()}))
	if _, err := a.Continue(ctx, ""); !errors.Is(err, ErrNothingToContinue) {
		t.Fatalf("empty branch: %v", err)
	}
	if err := a.appendToBranch(ctx, a.Tree(), a.Tree().Active(), types.AssistantMessage{Parts: []types.AssistantPart{
		types.ToolCallPart{ID: "c", Name: "x"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Continue(ctx, ""); !errors.Is(err, ErrNothingToContinue) {
		t.Fatalf("tool call tip: %v", err)
	}
}

func TestAutoContinue(t *testing.T) {
	openCall := []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "x"}, types.PartEnd{Index: 0},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "echo"},
		types.UsageDelta{FinishReasons: []string{"max_tokens"}},
	}
	tests := []struct {
		name         string
		auto         int
		calls        []stepCall
		wantRequests int
		wantErr      error
	}{
		{
			name:         "off",
			calls:        []stepCall{{before: maxTokensResponse("a")}},
			wantRequests: 1,
		},
		{
			name:         "resumes until the turn finishes",
			auto:         3,
			calls:        []stepCall{{before: maxTokensResponse("a")}, {before: maxTokensResponse("b")}, {before: agenttest.TextResponse("c")}},
			wantRequests: 3,
		},
		{
			name:         "stops at the limit",
			auto:         1,
			calls:        []stepCall{{before: maxTokensResponse("a")}, {before: maxTokensResponse("b")}, {before: agenttest.TextResponse("c")}},
			wantRequests: 2,
		},
		{
			name:         "an open tool call is never continued",
			auto:         3,
			calls:        []stepCall{{before: openCall}},
			wantRequests: 1,
			wantErr:      types.ErrResponseTruncated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStepProvider(tt.calls...)
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "echo"}, Result: "ok"}
			a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(tool)}, WithAutoContinue(tt.auto)))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("write"))})
			for range stream.Deltas() {
			}
			err := stream.Wait()
			if tt.wantErr == nil && err != nil || tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Wait = %v, want %v", err, tt.wantErr)
			}
			if got := len(provider.requests()); got != tt.wantRequests {
				t.Fatalf("requests = %d, want %d", got, tt.wantRequests)
			}
			if tool.CallCount() != 0 {
				t.Fatal("a cut-off tool call ran")
			}
		})
	}
}

// TestSubmitConcurrent submits from many goroutines while a run is active.
// Every accepted message is either appended or reported as undelivered, and
// none is lost after the run's final safe point. Run with -race.
func TestSubmitConcurrent(t *testing.T) {
	const writers, perWriter = 8, 20
	provider := newStepProvider()
	// MaxIter 1 also checks that each appended turn gets fresh step limits.
	a := must.Get(New(Config{Provider: provider, MaxIter: 1}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
	deltas := collect(stream)

	var (
		mu       sync.Mutex
		accepted = map[SubmissionID]bool{}
		wg       sync.WaitGroup
	)
	modes := []SubmitMode{SubmitQueue, SubmitSteer, SubmitInterruptReplace}
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				id, err := stream.Submit(types.UserMsg(types.Text(fmt.Sprintf("w%d-%d", w, i))), modes[(w+i)%len(modes)])
				if errors.Is(err, ErrRunFinished) {
					return
				}
				if err != nil {
					t.Errorf("Submit: %v", err)
					return
				}
				mu.Lock()
				accepted[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	got := <-deltas
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}

	seen := map[SubmissionID]bool{}
	for _, d := range got {
		if in, ok := d.(types.InjectedDelta); ok {
			seen[SubmissionID(in.SubmissionID)] = true
		}
	}
	for _, sub := range stream.Undelivered() {
		seen[sub.ID] = true
	}
	for id := range accepted {
		if !seen[id] {
			t.Fatalf("submission %s was accepted but neither injected nor undelivered", id)
		}
	}
	if _, err := stream.Submit(types.UserMsg(types.Text("late")), SubmitQueue); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("late Submit = %v", err)
	}
}

// Agent.Submit racing the end of a run must join it or start the next one.
// A run that releases the branch between Submit's failed claim and its
// lookup of the active stream once made Submit return ErrRunActive for a
// free branch.
func TestAgentSubmitRacingRunEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 2000 {
		a := must.Get(New(Config{Provider: &mockProvider{response: "ok"}}))
		branch := a.Tree().Active()
		first := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("first"))}, branch)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range first.Deltas() {
			}
		}()
		s, _, err := a.Submit(ctx, branch, types.UserMsg(types.Text("second")), SubmitQueue)
		if err != nil {
			t.Fatalf("iteration %d: Submit = %v", i, err)
		}
		if s != first {
			for range s.Deltas() {
			}
		}
		<-done
		if err := s.Wait(); err != nil {
			t.Fatalf("iteration %d: run: %v", i, err)
		}
	}
}
