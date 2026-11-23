package native_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/guardrail"
	"github.com/urmzd/saige/agent/provider/fallback"
	"github.com/urmzd/saige/agent/provider/router"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/failure"
	"github.com/urmzd/saige/flow/turn/native"
	turnsandbox "github.com/urmzd/saige/flow/turn/sandbox"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// These tests run saige's agent loop and read its error the way a durable
// run does, through failure.Repeats.

// alwaysFails is a model whose every call fails with err.
type alwaysFails struct{ err error }

func (f alwaysFails) Stream(context.Context, saigetypes.Request) (<-chan saigetypes.Delta, error) {
	return nil, f.err
}

func modelFailure(name string, kind saigetypes.ErrorKind, status int) error {
	return &saigetypes.ProviderError{Provider: name, Model: "m", Kind: kind, Code: status, Err: errors.New("refused"), RetryAfter: time.Second}
}

func invoke(ctx context.Context, a *agent.Agent) error {
	_, err := agent.Collect(a.Invoke(ctx, []saigetypes.Message{saigetypes.UserMsg(saigetypes.Text("hello"))}), nil)
	return err
}

func newAgent(t *testing.T, p saigetypes.Provider, opts ...agent.Option) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{SystemPrompt: "s", Provider: p}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func answers() *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("fine")}}
}

// A guardrail whose own model call was rate limited did not judge the input.
func TestAGuardrailWhoseModelWasRateLimitedIsRetried(t *testing.T) {
	t.Parallel()
	check := guardrail.Classifier("", alwaysFails{modelFailure("judge", saigetypes.ErrorKindRateLimit, 429)}, "be nice")
	a := newAgent(t, answers(), agent.WithInputGuardrails(agent.InputGuardrail{Guardrail: check}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := invoke(ctx, a)
	if !errors.Is(err, saigetypes.ErrGuardrailTripped) {
		t.Fatalf("err = %v, want a guardrail error", err)
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// waitsForCancel is a guardrail that reports it began, then waits for its
// context to end.
type waitsForCancel struct{ entered chan struct{} }

func (waitsForCancel) Name() string { return "slow" }

func (g waitsForCancel) Check(ctx context.Context, _ agent.GuardrailInput) (agent.GuardrailVerdict, error) {
	close(g.entered)
	<-ctx.Done()
	return agent.GuardrailVerdict{}, ctx.Err()
}

// A run whose context ends while a guardrail is checking is retried.
func TestARunCanceledDuringAGuardrailCheckIsRetried(t *testing.T) {
	t.Parallel()
	check := waitsForCancel{entered: make(chan struct{})}
	a := newAgent(t, answers(), agent.WithInputGuardrails(agent.InputGuardrail{Guardrail: check}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-check.entered; cancel() }()
	err := invoke(ctx, a)
	if err == nil {
		t.Fatal("the run succeeded, want an error")
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// A guardrail that blocks is retried: its next check may see other output
// or answer differently.
func TestATrippedGuardrailIsRetried(t *testing.T) {
	t.Parallel()
	a := newAgent(t, answers(), agent.WithInputGuardrails(agent.InputGuardrail{Guardrail: guardrail.MaxLength(1)}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := invoke(ctx, a)
	if !errors.Is(err, saigetypes.ErrGuardrailTripped) {
		t.Fatalf("err = %v, want a guardrail error", err)
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// A fallback chain repeats only when no member failed in passing.
func TestAFallbackChainRepeatsOnlyWhenEveryMemberWould(t *testing.T) {
	t.Parallel()
	tooLong := modelFailure("backup", saigetypes.ErrorKindContextLength, 400)
	for name, tc := range map[string]struct {
		members []error
		repeats bool
	}{
		"rate limited, then context length": {
			members: []error{modelFailure("primary", saigetypes.ErrorKindRateLimit, 429), tooLong},
		},
		"unavailable, then invalid model config": {
			members: []error{modelFailure("primary", saigetypes.ErrorKindUnavailable, 503), saigetypes.ErrInvalidModelConfig},
		},
		"unavailable, then media unavailable": {
			members: []error{modelFailure("primary", saigetypes.ErrorKindUnavailable, 503), saigetypes.ErrMediaUnavailable},
		},
		"context length on both": {
			members: []error{modelFailure("primary", saigetypes.ErrorKindContextLength, 400), tooLong},
			repeats: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var providers []saigetypes.Provider
			for _, m := range tc.members {
				providers = append(providers, alwaysFails{m})
			}
			chain, err := fallback.New(fallback.Config{Providers: providers})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err = invoke(ctx, newAgent(t, chain))
			if err == nil {
				t.Fatal("the run succeeded, want an error")
			}
			if got := failure.Repeats(err); got != tc.repeats {
				t.Errorf("Repeats is %v, want %v: %v", got, tc.repeats, err)
			}
		})
	}
}

// streamsError is a model whose every call fails mid-stream with err.
type streamsError struct{ err error }

func (f streamsError) Stream(context.Context, saigetypes.Request) (<-chan saigetypes.Delta, error) {
	ch := make(chan saigetypes.Delta, 1)
	ch <- saigetypes.ErrorDelta{Error: f.err}
	close(ch)
	return ch, nil
}

// A router whose home profile was overloaded, then whose backup refused the
// request locally, is retried: a retry reaches the home profile again. The
// first case is the chain "unavailable, then invalid model config" of
// failure's table, built through a real router.
func TestARouterThatFailedOverFromAPassingFailureIsRetried(t *testing.T) {
	t.Parallel()
	for name, refused := range map[string]error{
		"unavailable, then invalid model config": fmt.Errorf("backup: %w", saigetypes.ErrInvalidModelConfig),
		"unavailable, then media unavailable":    fmt.Errorf("google: part 0: %w", saigetypes.ErrMediaUnavailable),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, err := router.New(router.Config{Profiles: []router.Profile{
				{ID: "home", Provider: alwaysFails{modelFailure("home", saigetypes.ErrorKindUnavailable, 503)}},
				{ID: "backup", Provider: alwaysFails{refused}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err = invoke(ctx, newAgent(t, r.Session()))
			if !errors.Is(err, refused) {
				t.Fatalf("err = %v, want the backup's failure", err)
			}
			var chain *saigetypes.FallbackError
			if !errors.As(err, &chain) || len(chain.Errors) != 2 {
				t.Fatalf("err = %v, want a chain of both failures", err)
			}
			if failure.Repeats(err) {
				t.Errorf("Repeats is true: %v", err)
			}
		})
	}
}

// A fallback chain that falls back on rate limits only, whose second member
// failed mid-stream with an error it does not fall back on, is retried: the
// first member was rate limited.
func TestANarrowedFallbackChainThatPassedARateLimitIsRetried(t *testing.T) {
	t.Parallel()
	rule, err := fallback.Rule(fallback.OnRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := fallback.New(fallback.Config{FallbackOn: rule, Providers: []saigetypes.Provider{
		streamsError{modelFailure("primary", saigetypes.ErrorKindRateLimit, 429)},
		streamsError{modelFailure("backup", saigetypes.ErrorKindContextLength, 400)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = invoke(ctx, newAgent(t, chain))
	if !errors.Is(err, saigetypes.ErrContextLength) {
		t.Fatalf("err = %v, want the backup's failure", err)
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// A run that ends because its tool calls kept failing is retried: the
// tools' faults may be gone on the next attempt.
func TestARunWhoseToolCallsKeptFailingIsRetried(t *testing.T) {
	t.Parallel()
	p := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("c1", "lookup", map[string]any{"q": 1}),
		agenttest.ToolCallResponse("c2", "lookup", map[string]any{"q": 2}),
		agenttest.TextResponse("done"),
	}}
	lookup := &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "lookup"}, Err: errors.New("dial tcp: connection reset by peer")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := invoke(ctx, newAgent(t, p, agent.WithTools(lookup)))
	if !errors.Is(err, saigetypes.ErrToolErrorLimit) {
		t.Fatalf("err = %v, want the tool error limit", err)
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// failsThenWaits is a tool whose first call fails and whose second waits
// for its context to end.
type failsThenWaits struct {
	mu      sync.Mutex
	calls   int
	waiting chan struct{}
}

func (*failsThenWaits) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "lookup", Description: "look something up"}
}

func (f *failsThenWaits) Execute(ctx context.Context, _ map[string]any) (string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == 1 {
		return "", errors.New("upstream 503")
	}
	close(f.waiting)
	<-ctx.Done()
	return "", ctx.Err()
}

// A run whose context ends during a tool call is retried.
func TestARunCanceledDuringAToolCallIsRetried(t *testing.T) {
	t.Parallel()
	p := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("c1", "lookup", map[string]any{"q": 1}),
		agenttest.ToolCallResponse("c2", "lookup", map[string]any{"q": 2}),
		agenttest.TextResponse("done"),
	}}
	lookup := &failsThenWaits{waiting: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-lookup.waiting; cancel() }()
	err := invoke(ctx, newAgent(t, p, agent.WithTools(lookup)))
	if err == nil {
		t.Fatal("the run succeeded, want an error")
	}
	if failure.Repeats(err) {
		t.Errorf("Repeats is true: %v", err)
	}
}

// A run that used every iteration it was given repeats.
func TestARunThatReachedItsIterationLimitRepeats(t *testing.T) {
	t.Parallel()
	p := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		agenttest.ToolCallResponse("c1", "lookup", map[string]any{"q": 1}),
		agenttest.ToolCallResponse("c2", "lookup", map[string]any{"q": 2}),
		agenttest.ToolCallResponse("c3", "lookup", map[string]any{"q": 3}),
	}}
	lookup := &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "lookup"}, Result: "nothing yet"}
	a, err := agent.New(agent.Config{SystemPrompt: "s", Provider: p, MaxIter: 2}, agent.WithTools(lookup))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = invoke(ctx, a)
	if !errors.Is(err, saigetypes.ErrMaxIterations) {
		t.Fatalf("err = %v, want the iteration limit", err)
	}
	if !failure.Repeats(err) {
		t.Errorf("Repeats is false: %v", err)
	}
}

// The native runtime decides whether its failure repeats, and the decision
// is still there once the turn has run inside the sandbox decorator, which
// keeps only the error's message.
func TestTheNativeRuntimesDecisionLeavesTheSandbox(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cause   error
		repeats bool
	}{
		"context length": {cause: modelFailure("p", saigetypes.ErrorKindContextLength, 400), repeats: true},
		"rate limited":   {cause: modelFailure("p", saigetypes.ErrorKindRateLimit, 429)},
		"rate limited, then context length": {cause: &saigetypes.FallbackError{Errors: []error{
			modelFailure("primary", saigetypes.ErrorKindRateLimit, 429),
			modelFailure("backup", saigetypes.ErrorKindContextLength, 400),
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inner := &native.Runtime{Options: []native.Option{
				native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) {
					return alwaysFails{tc.cause}, nil
				}),
			}}
			in := turn.Input{Trigger: posted("update the docs", nil)}

			_, err := inner.Turn(t.Context(), pinned(), in, nil)
			if repeats, decided := turn.Repeats(err); !decided || repeats != tc.repeats {
				t.Errorf("unsandboxed: Repeats = %v, %v, want %v, true: %v", repeats, decided, tc.repeats, err)
			}

			ws, werr := workspace.NewLocal(t.TempDir())
			if werr != nil {
				t.Fatal(werr)
			}
			tools := tool.NewRegistry()
			plane := turnsandbox.Plane{Control: controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop()), Tools: tools}
			_, err = turnsandbox.Sandboxed(inner, plane).Turn(t.Context(), pinned(), in, nil)
			if err == nil {
				t.Fatal("the sandboxed turn succeeded, want a failure")
			}
			if got := failure.Repeats(err); got != tc.repeats {
				t.Errorf("sandboxed: Repeats is %v, want %v: %v", got, tc.repeats, err)
			}
		})
	}
}
