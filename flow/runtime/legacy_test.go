package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	drun "github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/internal/legacy"
	"github.com/urmzd/saige/flow/runtime"
)

// untouched is an executor a run must never reach.
type untouched struct{ calls atomic.Int32 }

func (u *untouched) Execute(context.Context, runtime.Request, func(context.Context, event.Body) error) (runtime.Outcome, error) {
	u.calls.Add(1)
	return runtime.Outcome{}, errors.New("a run from before the rename reached the agent")
}

// A run accepted before the event union (fiber.turn) or before the rename
// (legacy.FiberTurnBeforeRename), parked on a question when the deployment was upgraded, is
// not left pending forever, replayed under a definition it never pinned, or
// failed as an unknown workflow with nothing said. The janitor's poll reaches
// it, and it ends failed once, terminally, with a reason on the ledger, on its
// channel and in the log, without running the agent.
func TestLegacyTurnFailsTerminally(t *testing.T) {
	for _, name := range []string{runtime.ActivityTurnV1, legacy.FiberTurnBeforeRename} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			logged := &warnings{}
			w.log = slog.New(logged)
			exec := &untouched{}
			rt := w.runtime(t, exec)

			// What the old version left behind: a pending run under the old
			// name and input shape, off the queue, with its question recorded.
			tg := tag("docs-bot", "evt-old", "explain")
			runID := runtime.RunID(tg.Tenant, tg.ID)
			input, err := json.Marshal(map[string]any{"tag": tg, "rev": 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := w.ledger.Accept(ctx, drun.Run{
				ID: runID, Name: name, Input: input,
				Status: drun.StatusPending, CreatedAt: time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			question := strings.TrimSuffix(name, "turn") + "question.q1"
			if _, err := w.ledger.Record(ctx, drun.Record{
				RunID: runID, Key: question + "#0", Kind: drun.KindStep, Name: question,
				Codec: "application/json", Status: drun.RecordOK, Output: []byte(`{"id":"q1"}`), Attempt: 1,
			}); err != nil {
				t.Fatal(err)
			}

			w.workers(t, rt, 1, 20*time.Millisecond)
			var ended drun.Run
			waitFor(t, func() bool {
				got, err := w.ledger.GetRun(ctx, runID)
				ended = got
				return err == nil && got.Status.Terminal()
			})

			if ended.Status != drun.StatusFailed {
				t.Fatalf("the run ended %s, want failed", ended.Status)
			}
			if !strings.Contains(ended.Error, "started before the event union; send the message again") ||
				strings.Contains(ended.Error, "unknown workflow") {
				t.Errorf("ledger error = %q, want the event union named as the reason", ended.Error)
			}
			failed := lifecycleOf(t, w.bus.of(runID), event.TypeFailed)
			if len(failed) != 1 {
				t.Fatalf("published %v, want exactly one run.failed", kindsOf(w.bus.of(runID)))
			}
			if body := failed[0]; !body.Terminal || body.Error != ended.Error {
				t.Errorf("run.failed = %+v, want terminal with the ledger's reason %q", body, ended.Error)
			}
			if !slices.ContainsFunc(logged.all(), func(m string) bool { return strings.Contains(m, "before the event union") }) {
				t.Errorf("logged %q, want the run's failure named", logged.all())
			}
			if n := exec.calls.Load(); n != 0 {
				t.Errorf("the agent ran %d times for a run it cannot resume", n)
			}
			// Nothing is left to wait on: a late answer is refused.
			if _, _, err := rt.Answer(ctx, runtime.AnswerRequest{Tenant: "acme", RunID: runID, QuestionID: "q1", Answer: event.Answer{Text: "yes"}, By: "ana"}); err == nil {
				t.Error("an answer to the ended run was accepted")
			}
		})
	}
}
