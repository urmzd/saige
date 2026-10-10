package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestMemoryInterruptRouterReply(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		replies []types.InterruptReply
		want    []error
		// withdraw withdraws the interrupt after the first reply, as a
		// waiter does once it consumes the reply.
		withdraw bool
	}{
		{
			name:    "first reply delivered",
			replies: []types.InterruptReply{{ID: "a", IdempotencyKey: "k1"}},
			want:    []error{nil},
		},
		{
			name:    "same key is idempotent",
			replies: []types.InterruptReply{{ID: "a", IdempotencyKey: "k1"}, {ID: "a", IdempotencyKey: "k1"}},
			want:    []error{nil, nil},
		},
		{
			name:    "different key conflicts",
			replies: []types.InterruptReply{{ID: "a", IdempotencyKey: "k1"}, {ID: "a", IdempotencyKey: "k2"}},
			want:    []error{nil, ErrMarkerResolved},
		},
		{
			name:    "no key never repeats",
			replies: []types.InterruptReply{{ID: "a"}, {ID: "a"}},
			want:    []error{nil, ErrMarkerResolved},
		},
		{
			name:     "same key is idempotent after the waiter withdrew",
			replies:  []types.InterruptReply{{ID: "a", IdempotencyKey: "k1"}, {ID: "a", IdempotencyKey: "k1"}},
			want:     []error{nil, nil},
			withdraw: true,
		},
		{
			name:     "different key conflicts after the waiter withdrew",
			replies:  []types.InterruptReply{{ID: "a", IdempotencyKey: "k1"}, {ID: "a", IdempotencyKey: "k2"}},
			want:     []error{nil, ErrMarkerResolved},
			withdraw: true,
		},
		{
			name:    "unknown ID",
			replies: []types.InterruptReply{{ID: "missing"}},
			want:    []error{types.ErrInterruptNotFound},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewMemoryInterruptRouter()
			ch, err := r.Post(ctx, types.Interrupt{ID: "a", RunID: "run"})
			if err != nil {
				t.Fatal(err)
			}
			for i, reply := range tt.replies {
				if err := r.Reply(ctx, reply); !errors.Is(err, tt.want[i]) || (tt.want[i] == nil && err != nil) {
					t.Fatalf("reply %d: err = %v, want %v", i, err, tt.want[i])
				}
				if i == 0 && tt.withdraw {
					if !r.withdraw("a") {
						t.Fatal("withdraw did not report the reply")
					}
				}
			}
			if tt.want[0] == nil {
				if got := <-ch; got.ID != "a" {
					t.Fatalf("delivered %+v", got)
				}
				select {
				case extra := <-ch:
					t.Fatalf("second delivery %+v", extra)
				default:
				}
			}
		})
	}
}

func TestMemoryInterruptRouterPostAndPending(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryInterruptRouter()
	base := time.Now().UTC()
	for i, in := range []types.Interrupt{
		{ID: "b", RunID: "run", CreatedAt: base.Add(time.Second)},
		{ID: "a", RunID: "run", CreatedAt: base},
		{ID: "c", RunID: "other", CreatedAt: base},
	} {
		if _, err := r.Post(ctx, in); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	if _, err := r.Post(ctx, types.Interrupt{ID: "a"}); err == nil {
		t.Fatal("duplicate pending ID was accepted")
	}
	if _, err := r.Post(ctx, types.Interrupt{}); err == nil {
		t.Fatal("empty ID was accepted")
	}
	if _, err := r.Post(ctx, types.Interrupt{ID: "late", ExpiresAt: base.Add(-time.Second)}); !errors.Is(err, types.ErrInterruptExpired) {
		t.Fatalf("expired post: %v", err)
	}

	tests := []struct {
		runID string
		want  []string
	}{
		{"run", []string{"a", "b"}},
		{"other", []string{"c"}},
		{"", []string{"a", "c", "b"}},
	}
	for _, tt := range tests {
		got, _ := r.Pending(ctx, tt.runID)
		var ids []string
		for _, in := range got {
			ids = append(ids, in.ID)
		}
		if len(ids) != len(tt.want) {
			t.Fatalf("Pending(%q) = %v, want %v", tt.runID, ids, tt.want)
		}
		for i := range ids {
			if ids[i] != tt.want[i] {
				t.Fatalf("Pending(%q) = %v, want %v", tt.runID, ids, tt.want)
			}
		}
	}

	if err := r.Reply(ctx, types.InterruptReply{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Pending(ctx, "run"); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("answered interrupt still pending: %+v", got)
	}
	r.withdraw("b")
	if err := r.Reply(ctx, types.InterruptReply{ID: "b"}); !errors.Is(err, types.ErrInterruptNotFound) {
		t.Fatalf("reply after withdraw: %v", err)
	}
}

func TestMemoryInterruptRouterExpiry(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryInterruptRouter()
	ch, err := r.Post(ctx, types.Interrupt{ID: "x", ExpiresAt: time.Now().Add(20 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expired interrupt delivered a reply")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not expire")
	}
	r.withdraw("x")
	if err := r.Reply(ctx, types.InterruptReply{ID: "x"}); !errors.Is(err, types.ErrInterruptExpired) {
		t.Fatalf("late reply: %v", err)
	}
	if got, _ := r.Pending(ctx, ""); len(got) != 0 {
		t.Fatalf("expired interrupt still pending: %+v", got)
	}
}

// nextMarker returns the next MarkerDelta on stream.
func nextMarker(t *testing.T, stream *EventStream) types.MarkerDelta {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case d, ok := <-stream.Deltas():
			if !ok {
				t.Fatal("stream closed before a marker")
			}
			if m, ok := d.(types.MarkerDelta); ok {
				return m
			}
		case <-timeout:
			t.Fatal("no marker")
		}
	}
}

func TestInterruptExpiryPolicies(t *testing.T) {
	const ttl = 30 * time.Millisecond
	tests := []struct {
		name      string
		policy    types.InterruptExpiry
		path      []string
		answer    bool // answer the escalated interrupt
		wantOK    bool
		wantStop  error
		wantPosts int
	}{
		{name: "deny", policy: types.InterruptExpireDeny, wantPosts: 1},
		{name: "fail", policy: types.InterruptExpireFail, wantStop: types.ErrInterruptExpired, wantPosts: 1},
		{name: "escalate at root denies", policy: types.InterruptExpireEscalate, wantPosts: 1},
		{name: "escalate in child, answered", policy: types.InterruptExpireEscalate, path: []string{"call-parent"}, answer: true, wantOK: true, wantPosts: 2},
		{name: "escalate in child, unanswered", policy: types.InterruptExpireEscalate, path: []string{"call-parent"}, wantPosts: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := newEventStream(ctx, cancel)
			stream.path = tt.path
			a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}}, WithInterruptExpiry(ttl, types.InterruptPolicy{OnExpire: tt.policy}))
			type outcome struct {
				msg string
				ok  bool
			}
			done := make(chan outcome, 1)
			go func() {
				msg, _, ok := a.awaitApproval(ctx, stream, types.ToolCallPart{ID: "call", Name: "write"}, []types.Marker{{Kind: "approval"}})
				done <- outcome{msg, ok}
			}()
			var posted []types.Interrupt
			for i := 0; i < tt.wantPosts; i++ {
				m := nextMarker(t, stream)
				if m.Interrupt == nil || m.Interrupt.ExpiresAt.IsZero() {
					t.Fatalf("marker %d has no interrupt deadline: %+v", i, m.Interrupt)
				}
				posted = append(posted, *m.Interrupt)
				if i == 1 && tt.answer {
					if err := stream.ReplyInterrupt(ctx, types.InterruptReply{ID: m.Interrupt.ID, Decision: types.ApprovalDecision{Approved: true}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := <-done
			if got.ok != tt.wantOK {
				t.Fatalf("approved = %v (%s), want %v", got.ok, got.msg, tt.wantOK)
			}
			if err := stream.runError(); !errors.Is(err, tt.wantStop) || (tt.wantStop == nil && err != nil) {
				t.Fatalf("run error = %v, want %v", err, tt.wantStop)
			}
			if len(posted) == 2 {
				if len(posted[1].Path) != 0 || posted[1].ID == posted[0].ID {
					t.Fatalf("escalation not addressed to the caller: %+v", posted[1])
				}
			}
		})
	}
}

func TestMarkerCarriesInterruptAndReplyByID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := newEventStream(ctx, cancel)
	a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}})
	done := make(chan bool, 1)
	go func() {
		_, _, ok := a.awaitApproval(ctx, stream, types.ToolCallPart{ID: "call", Name: "write"}, []types.Marker{{Kind: "approval"}})
		done <- ok
	}()
	m := nextMarker(t, stream)
	in := m.Interrupt
	if in == nil || in.Kind != types.InterruptApproval || in.RunID != stream.runID || in.ID != types.InterruptID(stream.runID, nil, "gate", "call") {
		t.Fatalf("interrupt = %+v", in)
	}
	if pending := stream.PendingInterrupts(); len(pending) != 1 || pending[0].ID != in.ID {
		t.Fatalf("pending = %+v", pending)
	}
	reply := types.InterruptReply{ID: in.ID, IdempotencyKey: "k", Decision: types.ApprovalDecision{Approved: true}}
	if err := stream.ReplyInterrupt(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if err := stream.ReplyInterrupt(ctx, reply); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if !<-done {
		t.Fatal("approval by interrupt ID was not applied")
	}
	if err := stream.ReplyInterrupt(ctx, reply); err != nil {
		t.Fatalf("idempotent retry after the wait: %v", err)
	}
	if err := stream.ResolveMarkerErr("call", Resolution{Approved: true}); !errors.Is(err, ErrMarkerResolved) {
		t.Fatalf("second decision after the wait: %v", err)
	}
}

func TestClarificationTool(t *testing.T) {
	tests := []struct {
		name    string
		answer  func(s *EventStream, m types.MarkerDelta) error
		want    string
		wantErr bool
	}{
		{
			name: "answer by interrupt",
			answer: func(s *EventStream, m types.MarkerDelta) error {
				raw, _ := json.Marshal("blue")
				return s.ReplyInterrupt(context.Background(), types.InterruptReply{ID: m.Interrupt.ID, Answer: raw})
			},
			want: "blue",
		},
		{
			name: "answer by marker message",
			answer: func(s *EventStream, m types.MarkerDelta) error {
				return s.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true, Message: "green"})
			},
			want: "green",
		},
		{
			name: "declined",
			answer: func(s *EventStream, m types.MarkerDelta) error {
				return s.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: false, Message: "skip"})
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("ask", ClarificationToolName, map[string]any{"question": "Which color?"}),
				agenttest.TextResponse("done"),
			}}
			a := NewAgent(AgentConfig{Provider: provider, Tools: types.NewToolRegistry(ClarificationTool())})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("paint it"))})
			m := nextMarker(t, stream)
			if m.Interrupt == nil || m.Interrupt.Kind != types.InterruptClarification || string(m.Interrupt.Payload) != `{"question":"Which color?"}` {
				t.Fatalf("clarification interrupt = %+v", m.Interrupt)
			}
			if err := tt.answer(stream, m); err != nil {
				t.Fatal(err)
			}
			var end types.ToolExecEndDelta
			for d := range stream.Deltas() {
				if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "ask" {
					end = e
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != (end.Error != "") || end.Result != tt.want {
				t.Fatalf("result = %+v", end)
			}
		})
	}
}

func TestClarificationWithoutConsumerFails(t *testing.T) {
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("ask", ClarificationToolName, map[string]any{"question": "Which?"}),
		agenttest.TextResponse("done"),
	}}
	a := NewAgent(AgentConfig{Provider: provider, Tools: types.NewToolRegistry(ClarificationTool())})
	err := withinDeadline(t, 3*time.Second, func() error {
		_, err := a.RunDurable(context.Background(), nil, []types.Message{types.UserMsg(types.Text("go"))}, "")
		return err
	})
	if !errors.Is(err, errNonStreamingApproval) {
		t.Fatalf("err = %v, want %v", err, errNonStreamingApproval)
	}
}
