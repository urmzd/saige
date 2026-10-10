package duraturo

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func question(runID string) types.Interrupt {
	return types.Interrupt{
		ID:      types.InterruptID(runID, []string{"call-1"}, "clarify", "call-2"),
		RunID:   runID,
		Path:    []string{"call-1"},
		Kind:    types.InterruptClarification,
		Payload: json.RawMessage(`{"question":"which file?"}`),
	}
}

func TestRouterPostReplyReplay(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	in := question("run")

	a := open(t, e, "run")
	if _, err := a.r.Post(a.ctx, in); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("first post: %v", err)
	}
	// A replay that posts the same interrupt again still waits.
	a.next()
	if _, err := a.r.Post(a.ctx, in); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("pending post: %v", err)
	}
	a.next()
	changed := in
	changed.Payload = json.RawMessage(`{"question":"other"}`)
	if _, err := a.r.Post(a.ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed post: %v", err)
	}

	router := e.Router()
	pend, err := router.Pending(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].ID != in.ID || pend[0].ExpiresAt.IsZero() || string(pend[0].Payload) != string(in.Payload) {
		t.Fatalf("pending = %+v", pend)
	}
	answer := types.InterruptReply{ID: in.ID, IdempotencyKey: "k1", Answer: json.RawMessage(`"main.go"`)}
	tests := []struct {
		name  string
		reply types.InterruptReply
		err   error
	}{
		{name: "first reply applies", reply: answer},
		{name: "same reply and key is a no-op", reply: answer},
		{name: "other key conflicts", reply: types.InterruptReply{ID: in.ID, IdempotencyKey: "k2", Answer: answer.Answer}, err: ErrConflict},
		{name: "same key with another answer conflicts", reply: types.InterruptReply{ID: in.ID, IdempotencyKey: "k1", Answer: json.RawMessage(`"x.go"`)}, err: ErrConflict},
		{name: "unknown interrupt", reply: types.InterruptReply{ID: "int_missing", IdempotencyKey: "k1"}, err: types.ErrInterruptNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := router.Reply(ctx, tt.reply); !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
	if pend, _ := router.Pending(ctx, "run"); len(pend) != 0 {
		t.Fatalf("answered interrupt still pending: %+v", pend)
	}

	a.next()
	ch, err := a.r.Post(a.ctx, in)
	if err != nil {
		t.Fatalf("replayed post: %v", err)
	}
	got, ok := <-ch
	if !ok || got.ID != in.ID || got.IdempotencyKey != "k1" || string(got.Answer) != `"main.go"` {
		t.Fatalf("reply = %+v", got)
	}
	if _, open := <-ch; open {
		t.Fatal("reply channel left open")
	}
}

func TestRouterExpiry(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		policy  types.InterruptExpiry
		postErr error
	}{
		{name: "deny is the default", policy: ""},
		{name: "deny records a denial", policy: types.InterruptExpireDeny},
		{name: "fail stops the run", policy: types.InterruptExpireFail, postErr: types.ErrInterruptExpired},
		{name: "escalate stops the run", policy: types.InterruptExpireEscalate, postErr: types.ErrInterruptExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine()
			in := question("run")
			in.Policy.OnExpire = tt.policy
			in.ExpiresAt = time.Now().Add(-time.Second)
			a := open(t, e, "run")
			if _, err := a.r.Post(a.ctx, in); !errors.Is(err, types.ErrSuspended) {
				t.Fatalf("first post: %v", err)
			}
			reply := types.InterruptReply{ID: in.ID, IdempotencyKey: "late"}
			if err := e.Router().Reply(ctx, reply); !errors.Is(err, types.ErrInterruptExpired) || !errors.Is(err, ErrClosed) {
				t.Fatalf("late reply: %v", err)
			}
			for replay := 0; replay < 2; replay++ {
				a.next()
				ch, err := a.r.Post(a.ctx, in)
				if !errors.Is(err, tt.postErr) || (tt.postErr == nil && err != nil) {
					t.Fatalf("replay %d err = %v, want %v", replay, err, tt.postErr)
				}
				if tt.postErr != nil {
					continue
				}
				got := <-ch
				if got.Decision.Approved || got.IdempotencyKey != expiredReplyKey {
					t.Fatalf("replay %d reply = %+v", replay, got)
				}
			}
			if tt.postErr == nil {
				// The denial is recorded now; a late reply still reads as expired.
				if err := e.Router().Reply(ctx, reply); !errors.Is(err, types.ErrInterruptExpired) || !errors.Is(err, ErrClosed) {
					t.Fatalf("reply after recorded expiry: %v", err)
				}
			}
		})
	}
}

func TestRouterApprovalsAndScoping(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	req := types.ApprovalRequest{ID: "marker/call", ToolCall: types.ToolCallPart{ID: "call", Name: "write"}, Markers: []types.Marker{{Kind: "approval"}}}
	for _, id := range []string{"a", "b"} {
		a := open(t, e, id)
		if _, err := a.r.ResolveApproval(a.ctx, req); !errors.Is(err, types.ErrSuspended) {
			t.Fatal(err)
		}
	}
	router := e.Router()
	pend, err := router.Pending(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].Kind != types.InterruptApproval || pend[0].RunID != "a" || len(pend[0].Markers) != 1 {
		t.Fatalf("pending = %+v", pend)
	}
	approve := types.InterruptReply{ID: req.ID, IdempotencyKey: "k", Decision: types.ApprovalDecision{Approved: true}}
	if err := router.Reply(ctx, approve); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous reply: %v", err)
	}
	scoped := &Router{Engine: e, RunID: "a"}
	if err := scoped.Reply(ctx, approve); err != nil {
		t.Fatal(err)
	}
	a := (&attempt{t: t, e: e, id: "a", n: 1}).next()
	decision, err := a.r.ResolveApproval(a.ctx, req)
	if err != nil || !decision.Approved {
		t.Fatalf("decision = %+v, %v", decision, err)
	}
	if pend, _ := router.Pending(ctx, "b"); len(pend) != 1 {
		t.Fatalf("run b lost its approval: %+v", pend)
	}
	if pend, err := router.Pending(ctx, "missing"); err != nil || len(pend) != 0 {
		t.Fatalf("unknown run: %+v, %v", pend, err)
	}
	// Posting an interrupt under an approval's ID is a conflict.
	if _, err := router.Post(ctx, types.Interrupt{ID: req.ID, RunID: "b"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("post over approval: %v", err)
	}
	// A host can post to a run with no worker attached, and the run sees it.
	in := question("b")
	if _, err := router.Post(ctx, in); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("host post: %v", err)
	}
	if pend, _ := router.Pending(ctx, "b"); len(pend) != 2 {
		t.Fatalf("pending after host post: %+v", pend)
	}
	if err := router.Reply(ctx, types.InterruptReply{ID: in.ID, IdempotencyKey: "k", Answer: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	b := (&attempt{t: t, e: e, id: "b", n: 1}).next()
	ch, err := b.r.Post(b.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-ch; string(got.Answer) != "1" {
		t.Fatalf("reply = %+v", got)
	}
}
