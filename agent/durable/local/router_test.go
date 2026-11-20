package local

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// startRun records a run of revision v1 without executing it, so a host can
// post to it with no worker attached.
func startRun(t *testing.T, e *Engine, id string) {
	t.Helper()
	core, w, err := e.workflow("v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Start(context.Background(), w, id, []types.Message{types.UserMsg(types.Text("start"))}); err != nil {
		t.Fatal(err)
	}
}

func question(runID string) types.Interrupt {
	return types.Interrupt{
		ID:      types.InterruptID(runID, []string{"call-1"}, "clarify", "call-2"),
		RunID:   runID,
		Path:    []string{"call-1"},
		Kind:    types.InterruptClarification,
		Payload: json.RawMessage(`{"question":"which file?"}`),
	}
}

func TestRouterHostPostReplyAndScoping(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	router := e.Router()
	q := types.Interrupt{ID: "clarify-1", Kind: types.InterruptClarification, Payload: json.RawMessage(`{"question":"which file?"}`)}
	for _, id := range []string{"a", "b"} {
		startRun(t, e, id)
		in := q
		in.RunID = id
		if _, err := router.Post(ctx, in); !errors.Is(err, types.ErrSuspended) {
			t.Fatalf("host post on %s: %v", id, err)
		}
	}
	pend, err := router.Pending(ctx, "a")
	if err != nil || len(pend) != 1 || pend[0].ID != q.ID || pend[0].ExpiresAt.IsZero() {
		t.Fatalf("pending = %+v, %v", pend, err)
	}
	answer := types.InterruptReply{ID: q.ID, IdempotencyKey: "k1", Answer: json.RawMessage(`"main.go"`)}
	if err := router.Reply(ctx, answer); !errors.Is(err, ErrConflict) {
		t.Fatalf("reply to an ID two runs hold: %v, want ErrConflict", err)
	}
	scoped := &Router{Engine: e, RunID: "a"}
	tests := []struct {
		name  string
		reply types.InterruptReply
		err   error
	}{
		{name: "first reply applies", reply: answer},
		{name: "same reply and key is a no-op", reply: answer},
		{name: "other key conflicts", reply: types.InterruptReply{ID: q.ID, IdempotencyKey: "k2", Answer: answer.Answer}, err: ErrConflict},
		{name: "same key with another answer conflicts", reply: types.InterruptReply{ID: q.ID, IdempotencyKey: "k1", Answer: json.RawMessage(`"x.go"`)}, err: ErrConflict},
		{name: "unknown interrupt", reply: types.InterruptReply{ID: "int_missing", IdempotencyKey: "k1"}, err: types.ErrInterruptNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := scoped.Reply(ctx, tt.reply); !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
		})
	}
	if pend, _ := router.Pending(ctx, "a"); len(pend) != 0 {
		t.Fatalf("answered interrupt still pending: %+v", pend)
	}
	if pend, _ := router.Pending(ctx, "b"); len(pend) != 1 {
		t.Fatalf("run b lost its interrupt: %+v", pend)
	}
	in := q
	in.RunID = "a"
	ch, err := router.Post(ctx, in)
	if err != nil {
		t.Fatalf("post after the reply: %v", err)
	}
	if got := <-ch; got.IdempotencyKey != "k1" || string(got.Answer) != `"main.go"` {
		t.Fatalf("reply = %+v", got)
	}
	if pend, err := router.Pending(ctx, "missing"); err != nil || len(pend) != 0 {
		t.Fatalf("unknown run: %+v, %v", pend, err)
	}
}

func TestRouterRefusesALateReply(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	startRun(t, e, "run")
	in := question("run")
	in.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := e.Router().Post(ctx, in); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("post: %v", err)
	}
	err := e.Router().Reply(ctx, types.InterruptReply{ID: in.ID, IdempotencyKey: "late"})
	if !errors.Is(err, types.ErrInterruptExpired) || !errors.Is(err, ErrClosed) {
		t.Fatalf("late reply: %v", err)
	}
}
