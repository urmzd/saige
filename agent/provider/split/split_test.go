package split

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/types"
)

// arm is a scripted provider. script returns the deltas for call n.
type arm struct {
	name   string
	caps   types.ModelCapabilities
	script func(n int) []types.Delta
	calls  *atomic.Int32
	gate   chan struct{} // when set, the stream waits on it before sending
}

func newArm(name string, script func(int) []types.Delta) arm {
	return arm{name: name, script: script, calls: &atomic.Int32{},
		caps: types.ModelCapabilities{Provider: "vendor-" + name, Model: name, Pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 2}}}
}

func says(text string) func(int) []types.Delta {
	return func(int) []types.Delta { return []types.Delta{types.TextContentDelta{Content: text}} }
}

func (a arm) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ds := a.script(int(a.calls.Add(1)))
	ch := make(chan types.Delta, len(ds))
	go func() {
		defer close(ch)
		if a.gate != nil {
			<-a.gate
		}
		for _, d := range ds {
			ch <- d
		}
	}()
	return ch, nil
}
func (a arm) Name() string                          { return a.caps.Provider }
func (a arm) Model() string                         { return a.name }
func (a arm) Capabilities() types.ModelCapabilities { return a.caps }

type result struct {
	text   string
	routes []types.RouteDelta
	err    error
	prompt int // prompt tokens summed over forwarded usage
}

func collect(t *testing.T, ctx context.Context, p types.Provider) result {
	t.Helper()
	ch, err := p.ChatStream(ctx, []types.Message{types.NewUserMessage("hi")}, nil)
	if err != nil {
		return result{err: err}
	}
	var r result
	for d := range ch {
		switch v := d.(type) {
		case types.TextContentDelta:
			r.text += v.Content
		case types.RouteDelta:
			r.routes = append(r.routes, v)
		case types.UsageDelta:
			r.prompt += v.PromptTokens
		case types.ErrorDelta:
			r.err = v.Error
		}
	}
	return r
}

func transient() error {
	return &types.ProviderError{Kind: types.ErrorKindTransient, Err: errors.New("overloaded")}
}

func TestNewValidates(t *testing.T) {
	p := newArm("a", says("a"))
	budget := types.NewBudget(types.BudgetPolicy{})
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no experiment", Config{Arms: []Arm{{Label: "a", Weight: 1, Provider: p}}}},
		{"no arms", Config{Experiment: "e"}},
		{"duplicate label", Config{Experiment: "e", Arms: []Arm{{Label: "a", Weight: 1, Provider: p}, {Label: "a", Weight: 1, Provider: p}}}},
		{"zero total weight", Config{Experiment: "e", Arms: []Arm{{Label: "a", Provider: p}}}},
		{"only canaries", Config{Experiment: "e", Arms: []Arm{{Label: "a", Weight: 1, Provider: p, Canary: &Guard{MaxErrorRate: 0.1}}}}},
		{"shadow without budget", Config{Experiment: "e", Arms: []Arm{{Label: "a", Weight: 1, Provider: p}}, Shadow: []ShadowArm{{Label: "s", Provider: p, Rate: 1}}}},
		{"shadow rate above one", Config{Experiment: "e", Arms: []Arm{{Label: "a", Weight: 1, Provider: p}}, Shadow: []ShadowArm{{Label: "s", Provider: p, Rate: 2, Budget: budget}}}},
		{"shadow label reuses an arm", Config{Experiment: "e", Arms: []Arm{{Label: "a", Weight: 1, Provider: p}}, Shadow: []ShadowArm{{Label: "a", Provider: p, Rate: 1, Budget: budget}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestAssignmentIsWeightedAndDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weights []int
		want    []float64 // expected share per arm
	}{
		{"even", []int{1, 1}, []float64{0.5, 0.5}},
		{"ninety ten", []int{90, 10}, []float64{0.9, 0.1}},
		{"zero weight arm never assigned", []int{1, 0, 1}, []float64{0.5, 0, 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var arms []Arm
			for i, w := range tc.weights {
				label := fmt.Sprint("arm", i)
				arms = append(arms, Arm{Label: label, Weight: w, Provider: newArm(label, says(label))})
			}
			s, err := New(Config{Experiment: "exp", Salt: "s1", Arms: arms})
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			const keys = 4000
			for k := range keys {
				key := fmt.Sprint("user-", k)
				label := s.Assign(key)
				if s.Assign(key) != label {
					t.Fatalf("assignment of %q is not deterministic", key)
				}
				counts[label]++
			}
			for i, share := range tc.want {
				got := float64(counts[fmt.Sprint("arm", i)]) / keys
				if got < share-0.03 || got > share+0.03 {
					t.Fatalf("arm%d share = %.3f, want about %.2f", i, got, share)
				}
			}
		})
	}
	if Bucket("a", "exp", "k") == Bucket("b", "exp", "k") && Bucket("a", "exp", "k2") == Bucket("b", "exp", "k2") {
		t.Fatal("salt does not change assignment")
	}
}

func TestSessionAssignmentIsSticky(t *testing.T) {
	a, b := newArm("a", says("a")), newArm("b", says("b"))
	var key atomic.Value
	key.Store("")
	s, _ := New(Config{Experiment: "exp", Arms: []Arm{{Label: "a", Weight: 1, Provider: a}, {Label: "b", Weight: 1, Provider: b}},
		Key: func(context.Context) string { return key.Load().(string) }})
	// Find keys that land on each arm.
	keyFor := map[string]string{}
	for k := 0; len(keyFor) < 2; k++ {
		keyFor[s.Assign(fmt.Sprint(k))] = fmt.Sprint(k)
	}
	key.Store(keyFor["a"])
	session := s.NewSession()
	if got := collect(t, context.Background(), session); got.text != "a" {
		t.Fatalf("first request = %+v", got)
	}
	key.Store(keyFor["b"])
	if got := collect(t, context.Background(), session); got.text != "a" {
		t.Fatalf("session moved arms: %+v", got)
	}
	if got := collect(t, context.Background(), s.NewSession()); got.text != "b" {
		t.Fatalf("new session reused the old assignment: %+v", got)
	}
}

func TestRouteDeltaNamesExperimentAndVariant(t *testing.T) {
	inner, err := router.New(router.Config{Profiles: []router.Profile{{ID: "profile-1", Provider: newArm("m", says("routed"))}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		provider    types.Provider
		wantProfile string
	}{
		{"plain arm gets one route", newArm("m", says("plain")), "only"},
		{"nested router route is annotated", inner.Session(), "profile-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := New(Config{Experiment: "exp", Arms: []Arm{{Label: "only", Weight: 1, Provider: tc.provider}}})
			got := collect(t, context.Background(), s)
			if got.err != nil || len(got.routes) != 1 {
				t.Fatalf("result = %+v", got)
			}
			r := got.routes[0]
			if r.Experiment != "exp" || r.Variant != "only" || r.Profile != tc.wantProfile {
				t.Fatalf("route = %+v", r)
			}
		})
	}
}

func TestCanaryGuard(t *testing.T) {
	failEarly := func(int) []types.Delta { return []types.Delta{types.ErrorDelta{Error: transient()}} }
	failLate := func(int) []types.Delta {
		return []types.Delta{types.TextContentDelta{Content: "par"}, types.ErrorDelta{Error: transient()}}
	}
	usageThenFail := func(int) []types.Delta {
		return []types.Delta{types.UsageDelta{PromptTokens: 10}, types.ErrorDelta{Error: transient()}}
	}
	refused := func(int) []types.Delta {
		return []types.Delta{types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindContentFilter, Err: errors.New("blocked")}}}
	}
	invalid := func(int) []types.Delta {
		return []types.Delta{types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindPermanent, Err: types.ErrInvalidModelConfig}}}
	}
	refusalFailsOver := func(err error) bool { return DefaultFailoverOn(err) || types.IsContentFilter(err) }
	for _, tc := range []struct {
		name         string
		canary       func(int) []types.Delta
		failoverOn   func(error) bool
		wantText     string
		wantErr      bool
		wantReasons  []string
		wantDemoted  bool
		requests     int
		wantCanaryOn int32 // canary calls after all requests
		wantControl  int32 // control calls after all requests
		wantSamples  int   // outcomes the guard recorded
	}{
		{"failure before output moves to control", failEarly, nil, "control", false, []string{"", ReasonCanaryFailover}, false, 1, 1, 1, 1},
		{"failure after output is returned", failLate, nil, "par", true, []string{""}, false, 1, 1, 0, 1},
		{"failure after usage moves to control", usageThenFail, nil, "control", false, []string{"", ReasonCanaryFailover}, false, 1, 1, 1, 1},
		{"sustained failures demote the canary", failEarly, nil, "control", false, []string{ReasonCanaryDemoted}, true, 3, 2, 3, 2},
		{"refusal is returned and not counted", refused, nil, "", true, []string{""}, false, 3, 3, 0, 0},
		{"invalid request is returned and not counted", invalid, nil, "", true, []string{""}, false, 3, 3, 0, 0},
		{"refusal moves to control when configured", refused, refusalFailsOver, "control", false, []string{"", ReasonCanaryFailover}, false, 1, 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canary := newArm("canary", tc.canary)
			control := newArm("control", says("control"))
			s, _ := New(Config{Experiment: "exp", FailoverOn: tc.failoverOn, Arms: []Arm{
				{Label: "control", Weight: 0, Provider: control},
				{Label: "canary", Weight: 1, Provider: canary, Canary: &Guard{MaxErrorRate: 0.5, MinSamples: 2}},
			}})
			var got result
			for range tc.requests {
				got = collect(t, context.Background(), s)
			}
			if got.text != tc.wantText || (got.err != nil) != tc.wantErr {
				t.Fatalf("result = %+v", got)
			}
			var reasons []string
			for _, r := range got.routes {
				reasons = append(reasons, r.Reason)
			}
			if fmt.Sprint(reasons) != fmt.Sprint(tc.wantReasons) {
				t.Fatalf("reasons = %q, want %q", reasons, tc.wantReasons)
			}
			if _, _, demoted, _ := s.CanaryStatus("canary"); demoted != tc.wantDemoted {
				t.Fatalf("demoted = %v", demoted)
			}
			if canary.calls.Load() != tc.wantCanaryOn || control.calls.Load() != tc.wantControl {
				t.Fatalf("calls: canary %d, control %d", canary.calls.Load(), control.calls.Load())
			}
			if _, n, _, _ := s.CanaryStatus("canary"); n != tc.wantSamples {
				t.Fatalf("guard samples = %d, want %d", n, tc.wantSamples)
			}
			s.ResetCanary("canary")
			if _, n, demoted, _ := s.CanaryStatus("canary"); demoted || n != 0 {
				t.Fatal("reset kept history")
			}
		})
	}
}

func TestForce(t *testing.T) {
	a, b := newArm("a", says("a")), newArm("b", says("b"))
	shadowCalls := &atomic.Int32{}
	shadow := newArm("shadow", func(int) []types.Delta { shadowCalls.Add(1); return nil })
	s, _ := New(Config{Experiment: "exp",
		Arms:   []Arm{{Label: "a", Weight: 1, Provider: a}, {Label: "b", Weight: 0, Provider: b, Canary: &Guard{MaxErrorRate: 0}}},
		Shadow: []ShadowArm{{Label: "shadow", Provider: shadow, Rate: 1, Budget: types.NewBudget(types.BudgetPolicy{})}},
	})
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		wantText string
		wantErr  error
	}{
		{"forced arm serves", Force(context.Background(), "exp", "b"), "b", nil},
		{"unknown label fails", Force(context.Background(), "exp", "missing"), "", ErrUnknownVariant},
		{"other experiment is ignored", Force(context.Background(), "other", "b"), "a", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(t, tc.ctx, s)
			if got.text != tc.wantText || !errors.Is(got.err, tc.wantErr) {
				t.Fatalf("result = %+v", got)
			}
			if tc.wantText == "b" && got.routes[0].Reason != ReasonForced {
				t.Fatalf("route = %+v", got.routes[0])
			}
		})
	}
	if err := s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if shadowCalls.Load() != 1 {
		t.Fatalf("shadow calls = %d, want only the unforced request", shadowCalls.Load())
	}
	if label, ok := Forced(Force(Force(context.Background(), "x", "1"), "y", "2"), "x"); !ok || label != "1" {
		t.Fatal("nested Force lost an experiment")
	}
}

func TestShadowTraffic(t *testing.T) {
	shadowScript := func(int) []types.Delta {
		return []types.Delta{
			types.TextContentDelta{Content: "shadow says"},
			types.ToolCallStartDelta{ID: "1", Name: "delete_everything"},
			types.ToolCallEndDelta{ID: "1"},
			types.UsageDelta{PromptTokens: 1_000_000, CompletionTokens: 0},
		}
	}
	for _, tc := range []struct {
		name      string
		rate      float64
		limit     types.Cost
		wantCalls int
		wantErr   bool
	}{
		{"sampled in", 1, 0, 1, false},
		{"sampled out", 0, 0, 0, false},
		{"exhausted shadow budget rejects before the call", 1, types.USD(0.000001), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := types.NewBudget(types.BudgetPolicy{Limit: tc.limit})
			if tc.limit > 0 {
				budget.Record("seed", types.Pricing{InputPerMTok: 1}, types.TokenUsage{InputTokens: 1})
			}
			shadow := newArm("shadow", shadowScript)
			gate := make(chan struct{})
			shadow.gate = gate
			var mu sync.Mutex
			var results []ShadowResult
			s, _ := New(Config{Experiment: "exp",
				Arms:     []Arm{{Label: "main", Weight: 1, Provider: newArm("main", says("main"))}},
				Shadow:   []ShadowArm{{Label: "shadow", Provider: shadow, Rate: tc.rate, Budget: budget}},
				OnShadow: func(r ShadowResult) { mu.Lock(); results = append(results, r); mu.Unlock() },
				Sample:   func() float64 { return 0.5 },
			})
			ctx, cancel := context.WithCancel(context.Background())
			if got := collect(t, ctx, s); got.text != "main" {
				t.Fatalf("caller saw %+v", got)
			}
			cancel() // the caller is done; the shadow keeps running
			close(gate)
			drainCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := s.Drain(drainCtx); err != nil {
				t.Fatal(err)
			}
			if int(shadow.calls.Load()) != tc.wantCalls {
				t.Fatalf("shadow calls = %d", shadow.calls.Load())
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.rate == 0 {
				if len(results) != 0 {
					t.Fatalf("results = %+v", results)
				}
				return
			}
			if len(results) != 1 {
				t.Fatalf("results = %+v", results)
			}
			r := results[0]
			if (r.Err != nil) != tc.wantErr || r.Experiment != "exp" || r.Label != "shadow" || r.Variant != "main" {
				t.Fatalf("result = %+v", r)
			}
			if !tc.wantErr {
				if r.Text != "shadow says" || r.ToolCalls != 1 {
					t.Fatalf("result = %+v", r)
				}
				if spent := budget.Spent(); spent != types.USD(1) {
					t.Fatalf("shadow budget spent %v, want $1", spent)
				}
			}
		})
	}
}

func TestCapabilitiesIntersectArms(t *testing.T) {
	a, b := newArm("a", says("a")), newArm("b", says("b"))
	a.caps.Caps = map[types.Capability]bool{types.CapTools: true, types.CapStructuredOutput: true}
	b.caps.Caps = map[types.Capability]bool{types.CapTools: true}
	s, _ := New(Config{Experiment: "exp", Arms: []Arm{{Label: "a", Weight: 1, Provider: a}, {Label: "b", Weight: 1, Provider: b}}})
	caps := s.Capabilities()
	if !caps.Supports(types.CapTools) || caps.Supports(types.CapStructuredOutput) {
		t.Fatalf("caps = %v", caps.List())
	}
	ch, err := s.ChatStreamWithSchema(context.Background(), nil, nil, &types.ParameterSchema{Type: "object"})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			streamErr = e.Error
		}
	}
	if !errors.Is(streamErr, types.ErrInvalidModelConfig) {
		t.Fatalf("an arm without schema support dropped the schema: %v", streamErr)
	}
}

// singleFlight is a session-scoped provider that rejects a call while another
// is in flight on the same session, as a routing session does.
type singleFlight struct {
	busy     *atomic.Bool
	gate     chan struct{}
	sessions *atomic.Int32
}

var errBusy = errors.New("session busy")

func (p singleFlight) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	if !p.busy.CompareAndSwap(false, true) {
		return nil, errBusy
	}
	ch := make(chan types.Delta, 1)
	go func() {
		defer close(ch)
		defer p.busy.Store(false)
		<-p.gate
		ch <- types.TextContentDelta{Content: "shadow"}
	}()
	return ch, nil
}

func (p singleFlight) NewSession() types.Provider {
	p.sessions.Add(1)
	return singleFlight{busy: &atomic.Bool{}, gate: p.gate, sessions: p.sessions}
}

func TestOverlappingShadowsGetTheirOwnSessions(t *testing.T) {
	gate := make(chan struct{})
	shadow := singleFlight{busy: &atomic.Bool{}, gate: gate, sessions: &atomic.Int32{}}
	var mu sync.Mutex
	var errs []error
	s, _ := New(Config{Experiment: "exp",
		Arms:     []Arm{{Label: "main", Weight: 1, Provider: newArm("main", says("main"))}},
		Shadow:   []ShadowArm{{Label: "shadow", Provider: shadow, Rate: 1, Budget: types.NewBudget(types.BudgetPolicy{})}},
		OnShadow: func(r ShadowResult) { mu.Lock(); errs = append(errs, r.Err); mu.Unlock() },
	})
	// Two turns of one session and one turn of another all overlap.
	other := s.NewSession()
	for _, p := range []types.Provider{s, s, other} {
		if got := collect(t, context.Background(), p); got.text != "main" {
			t.Fatalf("caller saw %+v", got)
		}
	}
	close(gate)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 3 {
		t.Fatalf("shadow results = %d", len(errs))
	}
	for _, err := range errs {
		if err != nil {
			t.Fatalf("overlapping shadow failed: %v", err)
		}
	}
	if shadow.sessions.Load() != 3 {
		t.Fatalf("shadow sessions = %d, want one per call", shadow.sessions.Load())
	}
}

// TestUsageBeforeOutputIsHeld checks that usage an arm reports before any
// output reaches the caller only from the arm that answers, or from the arm
// that failed when the request ends with its error.
func TestUsageBeforeOutputIsHeld(t *testing.T) {
	usage := func(n int, tail ...types.Delta) func(int) []types.Delta {
		return func(int) []types.Delta { return append([]types.Delta{types.UsageDelta{PromptTokens: n}}, tail...) }
	}
	refused := types.ErrorDelta{Error: &types.ProviderError{Kind: types.ErrorKindContentFilter, Err: errors.New("blocked")}}
	tests := []struct {
		name       string
		canary     func(int) []types.Delta
		wantText   string
		wantErr    bool
		wantPrompt int
	}{
		{"canary answers", usage(10, types.TextContentDelta{Content: "canary"}), "canary", false, 10},
		{"canary fails over", usage(10, types.ErrorDelta{Error: transient()}), "control", false, 3},
		{"canary error is returned with its usage", usage(10, refused), "", true, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(Config{Experiment: "exp", Arms: []Arm{
				{Label: "control", Weight: 0, Provider: newArm("control", usage(3, types.TextContentDelta{Content: "control"}))},
				{Label: "canary", Weight: 1, Provider: newArm("canary", tt.canary), Canary: &Guard{MaxErrorRate: 0.5, MinSamples: 5}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			got := collect(t, context.Background(), s)
			if got.text != tt.wantText || (got.err != nil) != tt.wantErr || got.prompt != tt.wantPrompt {
				t.Errorf("result = %+v, want text %q, error %v, prompt %d", got, tt.wantText, tt.wantErr, tt.wantPrompt)
			}
		})
	}
}
