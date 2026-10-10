package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// TestSubmitJoinsRunAfterCompaction checks that a run which moved to a
// compacted branch still owns it: a submission to the active branch joins
// the run instead of starting a second one there.
func TestSubmitJoinsRunAfterCompaction(t *testing.T) {
	tests := []struct {
		name string
		mode SubmitMode
	}{
		{name: "queue", mode: SubmitQueue},
		{name: "steer", mode: SubmitSteer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := make(chan struct{})
			provider := newStepProvider(
				stepCall{before: agenttest.TextResponse("summary of the request")},
				stepCall{hold: hold, after: agenttest.TextResponse("answer")},
			)
			a := NewAgent(AgentConfig{
				Provider:     provider,
				SystemPrompt: "sys",
				CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummarize, MaxInputTokens: 1},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			before := a.Tree().Active()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			waitStarted(t, provider, 1)
			if a.Tree().Active() == before {
				t.Fatal("the run did not compact onto a new branch")
			}

			// An empty branch means the active one, which is now the
			// compacted branch the run writes.
			joined, id, err := a.Submit(ctx, "", types.UserMsg(types.Text("more")), tt.mode)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if joined != stream || id == "" {
				t.Fatal("Submit started a second run on the branch the first run writes")
			}
			if _, err := a.start(ctx, nil, a.Tree().Active()); !errors.Is(err, ErrRunActive) {
				t.Fatal("a second run claimed the compacted branch")
			}
			close(hold)
			<-deltas
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if err := checkToolPairing(a.Tree(), a.Tree().Active()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestInterruptOnLastStepIsAnswered checks that an interrupting message is a
// new user turn: it is answered even when the stopped call used the last
// step the limit allowed.
func TestInterruptOnLastStepIsAnswered(t *testing.T) {
	tests := []struct {
		name    string
		maxIter int
	}{
		{name: "one step", maxIter: 1},
		{name: "two steps", maxIter: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStepProvider(
				stepCall{hold: make(chan struct{})},
				stepCall{before: agenttest.TextResponse("replaced")},
			)
			a := NewAgent(AgentConfig{Provider: provider, MaxIter: tt.maxIter})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			waitStarted(t, provider, 0)
			if _, err := stream.Submit(types.UserMsg(types.Text("new direction")), SubmitInterruptReplace); err != nil {
				t.Fatal(err)
			}
			<-deltas
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
			if err != nil {
				t.Fatal(err)
			}
			last, ok := msgs[len(msgs)-1].(types.AssistantMessage)
			if !ok || textOf(last) != "replaced" {
				t.Fatalf("branch ends with %#v, want the reply to the interrupt", msgs[len(msgs)-1])
			}
		})
	}
}

// TestBeginTurnCancelsForPendingInterrupt checks that an interrupt that
// arrived after the last safe point stops the next call before it runs.
func TestBeginTurnCancelsForPendingInterrupt(t *testing.T) {
	tests := []struct {
		name       string
		pending    []Submission
		wantCancel bool
	}{
		{name: "nothing pending"},
		{name: "queued only", pending: []Submission{{ID: "q", Mode: SubmitQueue}}},
		{name: "steer only", pending: []Submission{{ID: "s", Mode: SubmitSteer}}},
		{name: "interrupt", pending: []Submission{{ID: "q", Mode: SubmitQueue}, {ID: "i", Mode: SubmitInterruptReplace}}, wantCancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &inbox{pending: tt.pending}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			in.beginTurn(cancel)
			cancelled := errors.Is(context.Cause(ctx), errInterruptRequested)
			if cancelled != tt.wantCancel {
				t.Fatalf("cancelled = %v, want %v", cancelled, tt.wantCancel)
			}
			by := in.endTurn()
			if tt.wantCancel != (by == "i") {
				t.Fatalf("interruptedBy = %q", by)
			}
		})
	}
}

// TestDurableRunRefusesSubmissions checks that a run under a durable step
// runner takes no submitted messages: they are not in the runner's recorded
// input, so a replay would rebuild a different transcript.
func TestDurableRunRefusesSubmissions(t *testing.T) {
	modes := []SubmitMode{SubmitQueue, SubmitSteer, SubmitInterruptReplace}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			hold := make(chan struct{})
			provider := newStepProvider(stepCall{hold: hold, after: agenttest.TextResponse("done")})
			a := NewAgent(AgentConfig{Provider: provider}, WithStepRunner(newRecordingRunner()))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deltas := collect(stream)
			waitStarted(t, provider, 0)
			if _, err := stream.Submit(types.UserMsg(types.Text("x")), mode); !errors.Is(err, ErrSubmitUnsupported) {
				t.Fatalf("EventStream.Submit = %v, want ErrSubmitUnsupported", err)
			}
			if _, _, err := a.Submit(ctx, "", types.UserMsg(types.Text("x")), mode); !errors.Is(err, ErrRunActive) {
				t.Fatalf("Agent.Submit = %v, want ErrRunActive", err)
			}
			close(hold)
			<-deltas
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
		})
	}
}

// TestSubmitFromConsumerWithFullBuffer checks that Submit does not wait for
// the consumer: called from the reading goroutine while the delta buffer is
// full, it returns at once and the acknowledgement still precedes the
// injection.
func TestSubmitFromConsumerWithFullBuffer(t *testing.T) {
	tests := []struct {
		name string
		mode SubmitMode
	}{
		{name: "queue", mode: SubmitQueue},
		{name: "steer", mode: SubmitSteer},
		{name: "interrupt", mode: SubmitInterruptReplace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hold := make(chan struct{})
			var flood []types.Delta
			flood = append(flood, types.PartStart{Index: 0, Kind: types.KindText})
			for range 200 {
				flood = append(flood, types.PartDelta{Index: 0, Text: "x"})
			}
			provider := newStepProvider(
				stepCall{before: flood, hold: hold, after: []types.Delta{types.PartEnd{Index: 0}}},
				stepCall{before: agenttest.TextResponse("second")},
			)
			a := NewAgent(AgentConfig{Provider: provider})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			deadline := time.Now().Add(5 * time.Second)
			for len(stream.deltas) < cap(stream.deltas) {
				if time.Now().After(deadline) {
					t.Fatal("the delta buffer never filled")
				}
				time.Sleep(time.Millisecond)
			}

			done := make(chan SubmissionID, 1)
			go func() {
				id, err := stream.Submit(types.UserMsg(types.Text("extra")), tt.mode)
				if err != nil {
					t.Errorf("Submit: %v", err)
				}
				done <- id
			}()
			var id SubmissionID
			select {
			case id = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Submit waited for the consumer")
			}

			close(hold)
			got := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			queued := indexOf(got, func(d types.Delta) bool {
				q, ok := d.(types.QueuedDelta)
				return ok && q.SubmissionID == string(id)
			})
			injected := indexOf(got, func(d types.Delta) bool {
				in, ok := d.(types.InjectedDelta)
				return ok && in.SubmissionID == string(id)
			})
			if queued < 0 || injected < 0 || queued > injected {
				t.Fatalf("queued at %d, injected at %d", queued, injected)
			}
		})
	}
}

// TestAgentSubmitReportsStartedRun checks that the ID Agent.Submit returns
// for a run it starts is reported like a joined submission.
func TestAgentSubmitReportsStartedRun(t *testing.T) {
	tests := []struct {
		name string
		mode SubmitMode
	}{
		{name: "idle branch", mode: SubmitQueue},
		{name: "side run", mode: SubmitSide},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStepProvider(stepCall{before: agenttest.TextResponse("hi")})
			a := NewAgent(AgentConfig{Provider: provider})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			stream, id, err := a.Submit(ctx, "", types.UserMsg(types.Text("hello")), tt.mode)
			if err != nil || id == "" {
				t.Fatalf("Submit = %q, %v", id, err)
			}
			got := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			queued := indexOf(got, func(d types.Delta) bool {
				q, ok := d.(types.QueuedDelta)
				return ok && q.SubmissionID == string(id) && q.Mode == tt.mode.String()
			})
			var node string
			injected := indexOf(got, func(d types.Delta) bool {
				in, ok := d.(types.InjectedDelta)
				node = in.NodeID
				return ok && in.SubmissionID == string(id) && in.Mode == tt.mode.String()
			})
			if queued < 0 || injected < queued {
				t.Fatalf("queued at %d, injected at %d: %v", queued, injected, got)
			}
			n := findNode(t, a, types.NodeID(node))
			if n == nil || textOf(n.Message) != "hello" {
				t.Fatalf("injected node = %#v, want the submitted message", n)
			}
		})
	}
}

// findNode searches the tree of a for id.
func findNode(t *testing.T, a *Agent, id types.NodeID) *types.Node {
	t.Helper()
	queue := []*types.Node{a.Tree().Root()}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.ID == id {
			return n
		}
		children, err := a.Tree().Children(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		queue = append(queue, children...)
	}
	return nil
}
