package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/failure"
	"github.com/urmzd/saige/flow/turn/native"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// loopingModel asks for a tool on every call, with new arguments each time,
// and never answers.
type loopingModel struct{ calls atomic.Int64 }

func (m *loopingModel) Stream(_ context.Context, _ saigetypes.Request) (<-chan saigetypes.Delta, error) {
	n := m.calls.Add(1)
	ch := make(chan saigetypes.Delta, 8)
	for _, d := range agenttest.ToolCallResponse(fmt.Sprintf("c%d", n), "lookup", map[string]any{"n": n}) {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// failingModel fails every call with err.
type failingModel struct {
	calls atomic.Int64
	err   error
}

func (m *failingModel) Stream(context.Context, saigetypes.Request) (<-chan saigetypes.Delta, error) {
	m.calls.Add(1)
	return nil, m.err
}

func providerFailure(kind saigetypes.ErrorKind, status int) *saigetypes.ProviderError {
	return &saigetypes.ProviderError{Provider: "p", Model: "m", Kind: kind, Code: status, Err: errors.New("refused")}
}

// modelSandbox is a sandbox whose agents all run on model, with one
// tool-calling iteration per turn.
func modelSandbox(t *testing.T, model saigetypes.Provider) *runtime.Sandbox {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	s, err := runtime.NewSandbox(plane, tools, newSpecs(), runtime.WithAgentOptions(
		native.WithMaxIter(1),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil })))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A sandboxed turn's error crosses the task boundary with what it matches
// and with the decision whether a retry would repeat it.
func TestASandboxedTurnsErrorKeepsItsClassification(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		model   saigetypes.Provider
		is      error
		repeats bool
	}{
		"iteration limit": {model: &loopingModel{}, is: saigetypes.ErrMaxIterations, repeats: true},
		"context length": {
			model: &failingModel{err: providerFailure(saigetypes.ErrorKindContextLength, 400)},
			is:    saigetypes.ErrContextLength, repeats: true,
		},
		"rate limited": {
			model: &failingModel{err: providerFailure(saigetypes.ErrorKindRateLimit, 429)},
			is:    saigetypes.ErrRateLimited,
		},
		"rate limited, then context length": {
			model: &failingModel{err: &saigetypes.FallbackError{Errors: []error{
				providerFailure(saigetypes.ErrorKindRateLimit, 429),
				providerFailure(saigetypes.ErrorKindContextLength, 400),
			}}},
			is: saigetypes.ErrContextLength,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rev := newSpecs().revise("docs-bot", "You document things.")
			s := modelSandbox(t, tc.model)
			_, err := s.Execute(context.Background(),
				withPin(runtime.Request{RunID: "run-" + name, Tenant: "acme", Attempt: 1, Text: "x"}, rev),
				func(context.Context, event.Body) error { return nil })
			if err == nil {
				t.Fatal("the turn succeeded, want a failure")
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("the error does not match %v: %v", tc.is, err)
			}
			if got := failure.Repeats(err); got != tc.repeats {
				t.Errorf("Repeats is %v, want %v: %v", got, tc.repeats, err)
			}
		})
	}
}

// A sandboxed turn that reaches its iteration limit fails the run on its
// first attempt, as an unsandboxed one does.
func TestASandboxedTurnThatReachesItsIterationLimitIsNotRetried(t *testing.T) {
	t.Parallel()

	model := &loopingModel{}
	h := newHarness(t, newExecutor(""), runtime.WithRuntime(modelSandbox(t, model)))
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-sandbox-limit", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
		t.Fatal("the run succeeded, want a terminal failure")
	}
	failed := h.bus.ofKind(event.TypeFailed)
	if len(failed) != 1 {
		t.Fatalf("got %d failed events, want 1", len(failed))
	}
	body, err := decode[lifecycle](failed[0])
	if err != nil {
		t.Fatalf("decode failed event: %v", err)
	}
	if !body.Terminal {
		t.Error("the failure was reported as non-terminal")
	}
	if n := model.calls.Load(); n != 1 {
		t.Errorf("the model was called %d times, want 1 (one attempt of one iteration)", n)
	}
}

// A sandboxed turn whose failure may not come back is attempted again.
func TestASandboxedTurnThatWasRateLimitedIsRetried(t *testing.T) {
	t.Parallel()

	model := &failingModel{err: providerFailure(saigetypes.ErrorKindRateLimit, 429)}
	h := newHarness(t, newExecutor(""), runtime.WithRuntime(modelSandbox(t, model)))
	h.specs.revise("docs-bot", "v1")
	h.work(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-sandbox-429", "hello"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
		t.Fatal("the run succeeded, want a failure")
	}
	if n := len(h.bus.ofKind(event.TypeFailed)); n < 2 {
		t.Errorf("got %d failed events, want one per attempt", n)
	}
}

// A failure that may not come back leaves the run to its next attempt.
func TestAFailureThatMayNotRepeatIsRetried(t *testing.T) {
	t.Parallel()

	for name, cause := range map[string]error{
		"tool error limit":  saigetypes.ErrToolErrorLimit,
		"guardrail tripped": saigetypes.ErrGuardrailTripped,
		"rate limited":      providerFailure(saigetypes.ErrorKindRateLimit, 429),
		"rate limited, then context length": &saigetypes.FallbackError{Errors: []error{
			providerFailure(saigetypes.ErrorKindRateLimit, 429),
			providerFailure(saigetypes.ErrorKindContextLength, 400),
		}},
		"unavailable, then invalid model config": &saigetypes.FallbackError{Errors: []error{
			providerFailure(saigetypes.ErrorKindUnavailable, 503),
			fmt.Errorf("backup: %w", saigetypes.ErrInvalidModelConfig),
		}},
		"iteration limit as the deadline passed": errors.Join(context.DeadlineExceeded, saigetypes.ErrMaxIterations),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exec := newExecutor("second time lucky")
			exec.failNext(1, fmt.Errorf("native: agent %q version %s: %w", "docs-bot", "v1", cause))
			h := newHarness(t, exec)
			h.specs.revise("docs-bot", "v1")
			h.work(t)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-"+name, "hello"))
			if err != nil {
				t.Fatalf("Accept: %v", err)
			}
			out, err := h.rt.Result(ctx, acc.RunID)
			if err != nil {
				t.Fatalf("Result: %v", err)
			}
			if out.Text != "second time lucky" {
				t.Errorf("run output is %q, want the second attempt's answer", out.Text)
			}
			if n := h.exec.count(); n != 2 {
				t.Errorf("the executor ran %d times, want 2", n)
			}
		})
	}
}

// guardrailCutShort stands in for the agent loop's error when a guardrail
// check ends with its context: it matches ErrGuardrailTripped and keeps only
// the cause's text.
type guardrailCutShort struct{ cause error }

func (e *guardrailCutShort) Error() string {
	return "input guardrail slow tripped: guardrail failed: " + e.cause.Error()
}
func (e *guardrailCutShort) Is(target error) bool { return target == saigetypes.ErrGuardrailTripped }

// heldTurn holds its first turn until the turn's context ends and returns
// what shape makes of the context's error; every later turn answers.
type heldTurn struct {
	started chan struct{}
	once    sync.Once
	runs    atomic.Int64
	shape   func(error) error
}

func (e *heldTurn) Execute(ctx context.Context, _ runtime.Request, _ func(context.Context, event.Body) error) (runtime.Outcome, error) {
	if e.runs.Add(1) > 1 {
		return runtime.Outcome{Text: "answered on a later attempt"}, nil
	}
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return runtime.Outcome{}, e.shape(ctx.Err())
}

// startWorker runs a worker until the returned function is called.
func startWorker(h *harness, concurrency int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.New(h.ledger, h.queue,
			worker.WithRegistry(h.rt.Registry()),
			worker.WithConcurrency(concurrency),
			worker.WithJanitorEvery(0),
			worker.WithLeaseTTL(2*time.Second),
			worker.WithBackoff(func(int) time.Duration { return time.Millisecond }),
		).Run(ctx)
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

// A worker that shuts down in the middle of a turn leaves the run for the
// next worker, whatever error the turn returned as its context ended.
func TestAWorkerShutdownDuringATurnLeavesTheRunToItsNextAttempt(t *testing.T) {
	t.Parallel()

	for name, shape := range map[string]func(error) error{
		"the context's error": func(cause error) error { return fmt.Errorf("native: turn: %w", cause) },
		"a guardrail check":   func(cause error) error { return &guardrailCutShort{cause: cause} },
		"an iteration limit": func(error) error {
			return fmt.Errorf("native: turn: %w", saigetypes.ErrMaxIterations)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exec := &heldTurn{started: make(chan struct{}), shape: shape}
			h := newHarness(t, newExecutor(""), runtime.WithRuntime(runtime.AsRuntime(exec)))
			h.specs.revise("docs-bot", "v1")
			stop := startWorker(h, 1)
			defer stop()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-shutdown-"+name, "hello"))
			if err != nil {
				t.Fatalf("Accept: %v", err)
			}
			<-exec.started
			stop()

			r, err := h.ledger.GetRun(ctx, acc.RunID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if r.Status == run.StatusFailed {
				t.Fatalf("the run failed when its worker shut down: %s", r.Error)
			}

			defer startWorker(h, 1)()
			out, err := h.rt.Result(ctx, acc.RunID)
			if err != nil {
				t.Fatalf("Result after a second worker started: %v", err)
			}
			if out.Text != "answered on a later attempt" {
				t.Errorf("run output is %q, want the second attempt's answer", out.Text)
			}
			if n := exec.runs.Load(); n != 2 {
				t.Errorf("the turn ran %d times, want 2", n)
			}
		})
	}
}

// With several workers on the queue, a failure a retry would repeat ends the
// run once: one failed record, one failed event, one execution, nothing left
// to claim.
func TestARepeatingFailureEndsTheRunOnceUnderSeveralWorkers(t *testing.T) {
	t.Parallel()

	for name, cause := range map[string]error{
		"max iterations": saigetypes.ErrMaxIterations,
		"context length": providerFailure(saigetypes.ErrorKindContextLength, 400),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exec := newExecutor("")
			exec.err = fmt.Errorf("native: agent: %w", cause)
			h := newHarness(t, exec)
			h.specs.revise("docs-bot", "v1")
			stops := []func(){startWorker(h, 4), startWorker(h, 4), startWorker(h, 4)}
			stopAll := func() {
				for _, stop := range stops {
					stop()
				}
			}
			defer stopAll()

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev-once-"+name, "hello"))
			if err != nil {
				t.Fatalf("Accept: %v", err)
			}
			if _, err := h.rt.Result(ctx, acc.RunID); err == nil {
				t.Fatal("the run succeeded, want a terminal failure")
			}
			// A redelivery would show up as a later attempt.
			time.Sleep(300 * time.Millisecond)
			stopAll()

			r, recs, err := h.ledger.Load(ctx, acc.RunID)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if r.Status != run.StatusFailed {
				t.Errorf("status %s, want failed", r.Status)
			}
			failedRecords := 0
			for _, rec := range recs {
				if rec.Status == run.RecordFailed {
					failedRecords++
				}
			}
			if failedRecords != 1 {
				t.Errorf("%d failed records, want 1", failedRecords)
			}
			if n := len(h.bus.ofKind(event.TypeFailed)); n != 1 {
				t.Errorf("%d failed events, want 1", n)
			}
			if n := exec.count(); n != 1 {
				t.Errorf("the executor ran %d times, want 1", n)
			}
			claimCtx, claimCancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer claimCancel()
			if item, _, err := h.queue.Claim(claimCtx, time.Second); err == nil {
				t.Errorf("the queue still holds %s (attempt %d)", item.RunID, item.Attempt)
			}
		})
	}
}
