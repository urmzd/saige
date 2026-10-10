// Package split divides traffic between provider configurations for an
// experiment or a staged rollout.
//
// Each arm is a complete provider configuration with a weight. A session is
// assigned to one arm by hashing the experiment salt, the experiment name,
// and an assignment key, so the same key always lands in the same arm and the
// assignment stays fixed for the life of the session. Create one session per
// conversation owner with NewSession, as with package router.
//
// An arm may carry a canary Guard. A canary whose recent error rate passes
// the guard's limit is demoted, and its traffic moves to the control arm (the
// first arm without a guard). A canary attempt that fails with an error
// Config.FailoverOn accepts, before anything has been forwarded, is retried on
// the control arm in the same request. Once output or usage has been
// forwarded the error is returned instead, so two arms never mix one response.
// Only errors FailoverOn accepts count against the guard: a refusal or a
// request the caller got wrong says nothing about the canary's health.
//
// Shadow arms receive a sampled copy of requests in the background. Their
// output is drained and reported through Config.OnShadow and never reaches
// the caller, so a tool call a shadow model makes is never executed. Each
// shadow call reserves from the shadow arm's own budget before it starts. Put
// the split inside any privacy decorator, so a shadow provider receives the
// same redacted messages as the primary.
//
// Every attempt is reported with a types.RouteDelta whose Experiment and
// Variant name the split and the arm. Force pins a request to one arm, which
// lets offline evaluations exercise the exact configuration production uses.
package split

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/internal/schemacheck"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Buckets is the resolution of an assignment: a key hashes to one of this
// many buckets, and arm weights divide the buckets.
const Buckets = 10_000

// Route reasons reported on types.RouteDelta.Reason.
const (
	ReasonForced         = "forced"          // Force selected the arm
	ReasonCanaryDemoted  = "canary_demoted"  // the assigned canary is demoted; the control arm serves
	ReasonCanaryFailover = "canary_failover" // the canary failed before output; the control arm serves
)

// ErrUnknownVariant reports a forced label that the experiment does not define.
var ErrUnknownVariant = errors.New("unknown split variant")

// Guard bounds a canary arm's error rate. The guard keeps the outcomes of the
// most recent Window attempts, shared by every session of the split. Once at
// least MinSamples are recorded and the failure fraction exceeds MaxErrorRate,
// the canary is demoted until ResetCanary is called.
type Guard struct {
	MaxErrorRate float64
	// MinSamples is the number of attempts needed before the rate is
	// trusted. Zero means 10.
	MinSamples int
	// Window is the number of most recent attempts considered. Zero means 100.
	Window int
}

// Arm is one configuration in the split.
type Arm struct {
	Label    string
	Weight   int
	Provider types.Provider
	// Canary, when set, makes the arm a guarded canary.
	Canary *Guard
}

// ShadowArm mirrors a sample of requests to another provider.
type ShadowArm struct {
	Label    string
	Provider types.Provider
	// Rate is the fraction of requests mirrored, from 0 to 1.
	Rate float64
	// Budget is the shadow arm's own allowance. It is required, so shadow
	// spend is never charged to, or hidden from, the primary budget.
	Budget *types.Budget
	// Timeout bounds one shadow call. Zero means one minute.
	Timeout time.Duration
}

// ShadowResult describes one completed shadow call.
type ShadowResult struct {
	Experiment string
	// Label is the shadow arm; Variant is the arm that served the caller.
	Label   string
	Variant string
	// Text is the shadow's text output. ToolCalls counts the tool calls it
	// asked for, none of which were executed.
	Text      string
	ToolCalls int
	Usage     types.UsageDelta
	Latency   time.Duration
	// Err is the shadow's failure, including a budget rejection before the
	// call started.
	Err error
}

// Config defines a split.
type Config struct {
	Experiment string
	// Salt varies assignments between experiments that share keys. Changing
	// it reassigns every key.
	Salt   string
	Arms   []Arm
	Shadow []ShadowArm
	// Key returns the assignment key for a request, such as a user or
	// conversation ID. It is read once per session, on the first request. Nil,
	// or an empty key, uses a random key per session.
	Key func(context.Context) string
	// OnShadow receives every shadow result. It runs on the shadow's
	// goroutine and must be safe for concurrent use.
	OnShadow func(ShadowResult)
	// Sample returns a uniform value in [0, 1) for shadow sampling. Nil uses
	// math/rand/v2.
	Sample func() float64
	// FailoverOn reports whether a canary error is a failure of the canary
	// itself: such an error is retried on the control arm when nothing has
	// been forwarded, and counts against the canary's guard. Other errors are
	// returned to the caller and leave the guard unchanged. Nil uses
	// DefaultFailoverOn. Sending a refused prompt to the control arm is a
	// policy decision, so content-filter errors are excluded unless this
	// function accepts them.
	FailoverOn func(error) bool
}

// DefaultFailoverOn accepts transient errors, other than cancellation.
func DefaultFailoverOn(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	return types.IsTransient(err)
}

// shared is the state every session of one split holds in common.
type shared struct {
	cfg      Config
	guards   map[string]*guardState
	control  int
	inflight *tracker
}

// Split is a provider that routes each session to one arm. The value New
// returns is itself a session; NewSession returns independent ones.
type Split struct {
	shared *shared
	arms   []types.Provider

	mu       sync.Mutex
	key      string
	assigned int // index into arms, -1 until the first request
}

var (
	_ types.Provider                 = (*Split)(nil)
	_ types.NamedProvider            = (*Split)(nil)
	_ types.ModelProvider            = (*Split)(nil)
	_ types.TargetSwitcher           = (*Split)(nil)
	_ types.CapabilityReporter       = (*Split)(nil)
	_ types.StructuredOutputProvider = (*Split)(nil)
	_ types.OptionsProvider          = (*Split)(nil)
	_ types.SessionProvider          = (*Split)(nil)
	_ types.Closer                   = (*Split)(nil)
)

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithSample sets Config.Sample.
func WithSample(fn func() float64) Option { return func(c *Config) { c.Sample = fn } }

// New validates cfg and returns a split session. An invalid configuration
// is an error wrapping types.ErrInvalidConfig.
func New(cfg Config, opts ...Option) (*Split, error) {
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Experiment == "" {
		return nil, fmt.Errorf("%w: split requires an experiment name", types.ErrInvalidConfig)
	}
	if len(cfg.Arms) == 0 {
		return nil, fmt.Errorf("%w: split requires at least one arm", types.ErrInvalidConfig)
	}
	labels := map[string]bool{}
	total, control := 0, -1
	for i, a := range cfg.Arms {
		if a.Label == "" || labels[a.Label] || a.Provider == nil || a.Weight < 0 {
			return nil, fmt.Errorf("%w: invalid or duplicate split arm %q", types.ErrInvalidConfig, a.Label)
		}
		if a.Canary != nil && (a.Canary.MaxErrorRate < 0 || a.Canary.MaxErrorRate > 1) {
			return nil, fmt.Errorf("%w: split arm %q: canary error rate must be between 0 and 1", types.ErrInvalidConfig, a.Label)
		}
		labels[a.Label] = true
		total += a.Weight
		if a.Canary == nil && control < 0 {
			control = i
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("%w: split arms need a positive total weight", types.ErrInvalidConfig)
	}
	if control < 0 {
		return nil, fmt.Errorf("%w: split requires a control arm without a canary guard", types.ErrInvalidConfig)
	}
	for _, s := range cfg.Shadow {
		if s.Label == "" || labels[s.Label] || s.Provider == nil || s.Rate < 0 || s.Rate > 1 {
			return nil, fmt.Errorf("%w: invalid or duplicate shadow arm %q", types.ErrInvalidConfig, s.Label)
		}
		if s.Budget == nil {
			return nil, fmt.Errorf("%w: shadow arm %q requires its own budget", types.ErrInvalidConfig, s.Label)
		}
		labels[s.Label] = true
	}
	cfg.Arms = append([]Arm(nil), cfg.Arms...)
	cfg.Shadow = append([]ShadowArm(nil), cfg.Shadow...)
	if cfg.Sample == nil {
		cfg.Sample = rand.Float64
	}
	if cfg.FailoverOn == nil {
		cfg.FailoverOn = DefaultFailoverOn
	}
	sh := &shared{cfg: cfg, guards: map[string]*guardState{}, control: control, inflight: newTracker()}
	arms := make([]types.Provider, len(cfg.Arms))
	for i, a := range cfg.Arms {
		arms[i] = a.Provider
		if a.Canary != nil {
			sh.guards[a.Label] = newGuardState(*a.Canary)
		}
	}
	return &Split{shared: sh, arms: arms, key: types.NewID(), assigned: -1}, nil
}

// NewSession implements types.SessionProvider. The new session has its own
// assignment and its own arm sessions, and shares canary guards and shadow
// tracking with this split.
func (s *Split) NewSession() types.Provider {
	arms := make([]types.Provider, len(s.arms))
	for i, p := range s.arms {
		arms[i] = types.NewProviderSession(p)
	}
	return &Split{shared: s.shared, arms: arms, key: types.NewID(), assigned: -1}
}

// Name implements types.NamedProvider.
func (s *Split) Name() string { return "split" }

// Model implements types.ModelProvider with the assigned arm's model, or the
// control arm's before the first request.
func (s *Split) Model() string {
	s.mu.Lock()
	i := s.assigned
	s.mu.Unlock()
	if i < 0 {
		i = s.shared.control
	}
	return types.ProviderModel(s.arms[i])
}

// WithTarget implements types.TargetSwitcher for a model target by
// re-targeting every arm, as a fallback chain does. The result is a fresh
// session.
func (s *Split) WithTarget(t types.Target) (types.Provider, error) {
	arms, err := types.RetargetMembers(s.arms, t, "split")
	if err != nil {
		return nil, err
	}
	return &Split{shared: s.shared, arms: arms, key: s.key, assigned: -1}, nil
}

// Capabilities implements types.CapabilityReporter as the intersection over
// the arms, since any arm may serve a request. Shadow arms never serve one
// and are not included. Capabilities that need request options are dropped
// when no arm can receive them.
func (s *Split) Capabilities() types.ModelCapabilities {
	out, _ := types.ProviderCapabilities(s.arms[0])
	for _, p := range s.arms[1:] {
		next, _ := types.ProviderCapabilities(p)
		out = out.Intersect(next)
	}
	return optionscheck.Narrow(out, s.arms...)
}

// Unwrap returns the arms in configuration order. Shadow arms are not
// included. See package wrapper.
func (s *Split) Unwrap() []types.Provider {
	return append([]types.Provider(nil), s.arms...)
}

// Drain waits until no shadow call is in flight, or for ctx to end.
func (s *Split) Drain(ctx context.Context) error {
	select {
	case <-s.shared.inflight.wait():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close implements types.Closer. It waits for shadow calls (until ctx
// ends), then closes every
// arm and shadow provider. Sessions share these providers, so close the split
// only after its last session.
func (s *Split) Close(ctx context.Context) error {
	_ = s.Drain(ctx)
	var errs []error
	for _, p := range s.arms {
		if err := types.CloseProvider(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	for _, sh := range s.shared.cfg.Shadow {
		if err := types.CloseProvider(ctx, sh.Provider); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CanaryStatus reports a canary arm's recent error rate, the attempts it is
// based on, and whether the arm is demoted. ok is false for an arm without a
// guard.
func (s *Split) CanaryStatus(label string) (rate float64, samples int, demoted, ok bool) {
	g := s.shared.guards[label]
	if g == nil {
		return 0, 0, false, false
	}
	rate, samples, demoted = g.status()
	return rate, samples, demoted, true
}

// ResetCanary clears a canary's history and lifts a demotion.
func (s *Split) ResetCanary(label string) {
	if g := s.shared.guards[label]; g != nil {
		g.reset()
	}
}

// Bucket returns the bucket in [0, Buckets) that key hashes to under the
// given salt and experiment.
func Bucket(salt, experiment, key string) int {
	sum := sha256.Sum256([]byte(salt + "|" + experiment + "|" + key))
	return int(binary.BigEndian.Uint64(sum[:8]) % Buckets)
}

// armFor maps a bucket to an arm by cumulative weight.
func (sh *shared) armFor(bucket int) int {
	total := 0
	for _, a := range sh.cfg.Arms {
		total += a.Weight
	}
	point := bucket * total / Buckets
	acc := 0
	for i, a := range sh.cfg.Arms {
		acc += a.Weight
		if point < acc {
			return i
		}
	}
	return len(sh.cfg.Arms) - 1
}

// Assign returns the arm label key would be assigned to.
func (s *Split) Assign(key string) string {
	cfg := s.shared.cfg
	return cfg.Arms[s.shared.armFor(Bucket(cfg.Salt, cfg.Experiment, key))].Label
}

// assignment returns the session's arm, assigning it on first use.
func (s *Split) assignment(ctx context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.assigned >= 0 {
		return s.assigned
	}
	key := s.key
	if s.shared.cfg.Key != nil {
		if k := s.shared.cfg.Key(ctx); k != "" {
			key = k
		}
	}
	cfg := s.shared.cfg
	s.assigned = s.shared.armFor(Bucket(cfg.Salt, cfg.Experiment, key))
	return s.assigned
}

type forceKey struct{}

// Force returns a context that pins requests in the named experiment to the
// arm with the given label. A forced request skips canary demotion, canary
// failover, and shadow traffic. A label the experiment does not define fails
// the request with ErrUnknownVariant. Splits for other experiments are not
// affected.
func Force(ctx context.Context, experiment, label string) context.Context {
	prev, _ := ctx.Value(forceKey{}).(map[string]string)
	next := make(map[string]string, len(prev)+1)
	for k, v := range prev {
		next[k] = v
	}
	next[experiment] = label
	return context.WithValue(ctx, forceKey{}, next)
}

// Forced returns the label Force set for experiment, if any.
func Forced(ctx context.Context, experiment string) (string, bool) {
	m, _ := ctx.Value(forceKey{}).(map[string]string)
	label, ok := m[experiment]
	return label, ok
}

// request is one provider call as the caller made it.
type request struct {
	messages []types.Message
	tools    []types.ToolDef
	schema   *types.ParameterSchema
	opts     *types.RequestOptions
}

func (q request) call(ctx context.Context, p types.Provider) (<-chan types.Delta, error) {
	if q.opts != nil && !types.AcceptsOptions(p) {
		return nil, optionscheck.Unsupported(p)
	}
	if q.schema != nil && !types.AcceptsSchema(p) {
		return nil, schemacheck.Unsupported(p, "provider cannot enforce a response schema")
	}
	return p.Stream(ctx, types.Request{Messages: q.messages, Tools: q.tools, Schema: q.schema, Options: q.opts})
}

// Stream implements types.Provider. An arm that cannot enforce a schema or
// receive request options rejects the request rather than drop them.
func (s *Split) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return s.stream(ctx, request{messages: req.Messages, tools: req.Tools, schema: req.Schema, opts: req.Options})
}

// SupportsSchema implements types.StructuredOutputProvider.
func (s *Split) SupportsSchema() bool { return true }

// SupportsOptions implements types.OptionsProvider.
func (s *Split) SupportsOptions() bool { return true }

func (s *Split) stream(ctx context.Context, q request) (<-chan types.Delta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := s.shared.cfg
	arm, reason, forced := 0, "", false
	if label, ok := Forced(ctx, cfg.Experiment); ok {
		arm = -1
		for i, a := range cfg.Arms {
			if a.Label == label {
				arm = i
			}
		}
		if arm < 0 {
			return nil, fmt.Errorf("%w %q in experiment %q", ErrUnknownVariant, label, cfg.Experiment)
		}
		reason, forced = ReasonForced, true
	} else {
		arm = s.assignment(ctx)
		if g := s.shared.guards[cfg.Arms[arm].Label]; g != nil && g.demoted() {
			arm, reason = s.shared.control, ReasonCanaryDemoted
		}
	}
	if !forced {
		s.startShadows(ctx, q, cfg.Arms[arm].Label)
	}
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		s.relay(ctx, out, q, arm, reason, forced)
	}()
	return out, nil
}

// relay runs the attempt on arm and, for a canary that fails with an error
// FailoverOn accepts before anything was forwarded, one more on the control
// arm.
func (s *Split) relay(ctx context.Context, out chan<- types.Delta, q request, arm int, reason string, forced bool) {
	cfg := s.shared.cfg
	send := func(d types.Delta) bool {
		select {
		case out <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		label := cfg.Arms[arm].Label
		guard := s.shared.guards[label]
		if forced {
			guard = nil
		}
		committed, held, err := s.attempt(ctx, send, q, arm, reason)
		if ctx.Err() != nil {
			return
		}
		canaryFault := err != nil && cfg.FailoverOn(err)
		if guard != nil && (err == nil || canaryFault) {
			guard.record(canaryFault)
		}
		if err == nil {
			return
		}
		if guard != nil && canaryFault && !committed && arm != s.shared.control {
			arm, reason = s.shared.control, ReasonCanaryFailover
			continue
		}
		// The request ends here, so usage held from the failed arm is real
		// spend on that arm: report it with the error.
		for _, h := range held {
			if !send(h) {
				return
			}
		}
		send(types.ErrorDelta{Error: err})
		return
	}
}

// attempt streams one arm. It reports the arm's error, if any, whether
// output had been forwarded when it failed, and the usage it held back
// because the arm failed before any output. Every RouteDelta carries the
// experiment and variant; when the arm does not report its own route, one is
// emitted before its first output.
func (s *Split) attempt(ctx context.Context, send func(types.Delta) bool, q request, arm int, reason string) (committed bool, held []types.Delta, err error) {
	cfg := s.shared.cfg
	label := cfg.Arms[arm].Label
	p := s.arms[arm]
	routed := false
	route := func() bool {
		routed = true
		return send(types.RouteDelta{Profile: label, Provider: wrapper.InnermostName(p), Model: types.ProviderModel(p),
			Experiment: cfg.Experiment, Variant: label, Reason: reason})
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := q.call(attemptCtx, p)
	if err == nil && src == nil {
		err = errors.New("provider returned a nil stream")
	}
	if err != nil {
		if !route() {
			return false, nil, nil
		}
		return false, nil, err
	}
	// held keeps the usage deltas that arrive before the arm's first output.
	// They are forwarded with that output or when the stream completes. When
	// the arm fails first they are returned to the caller, which drops them
	// on failover, so the caller never sees usage from two rate cards for one
	// call.
	flush := func() bool {
		for _, h := range held {
			if !send(h) {
				return false
			}
		}
		held = nil
		return true
	}
	for {
		select {
		case <-ctx.Done():
			go drain(src)
			return committed, nil, nil
		case d, ok := <-src:
			if !ok {
				if !routed && !route() {
					return committed, nil, nil
				}
				flush()
				return committed, nil, nil
			}
			switch v := d.(type) {
			case types.ErrorDelta:
				go drain(src)
				if !routed && !route() {
					return committed, nil, nil
				}
				if v.Error == nil {
					return committed, held, errors.New("provider emitted an empty error")
				}
				return committed, held, v.Error
			case types.RouteDelta:
				routed = true
				if v.Experiment == "" {
					v.Experiment, v.Variant = cfg.Experiment, label
					if v.Reason == "" {
						v.Reason = reason
					}
				}
				d = v
			default:
				if !routed && !route() {
					go drain(src)
					return committed, nil, nil
				}
				if _, usage := d.(types.UsageDelta); usage && !committed {
					held = append(held, d)
					continue
				}
				if isOutput(d) {
					committed = true
					if !flush() {
						go drain(src)
						return committed, nil, nil
					}
				}
			}
			if !send(d) {
				go drain(src)
				return committed, nil, nil
			}
		}
	}
}

// isOutput reports whether forwarding d commits the request to this arm.
// Usage, terminal markers, routes and conversion reports do not: adapters
// such as Anthropic's report usage when the message starts, before any
// output, and the most common transient failure follows it. Usage that
// precedes output is held back instead (see attempt).
func isOutput(d types.Delta) bool {
	switch d.(type) {
	case types.UsageDelta, types.DoneDelta, types.ErrorDelta, types.RouteDelta, types.ConversionDelta:
		return false
	default:
		return true
	}
}

func drain(src <-chan types.Delta) {
	for range src {
	}
}
