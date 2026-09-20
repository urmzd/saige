// Package router selects complete provider configurations and keeps session
// affinity. Create one Session per conversation owner, not one per deployment.
package router

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

var ErrSessionBusy = errors.New("routing session already has an active request")

// Profile binds an ID to a fully configured provider. Two profiles may target
// the same model with different reasoning, sampling, cache, or endpoint options.
// Providers and their configuration must remain immutable after New.
type Profile struct {
	ID       string
	Provider types.Provider
}

// Candidate is the detached capability record visible to a routing policy.
type Candidate struct {
	ID           string
	Capabilities types.ModelCapabilities
}

// Request is evaluated once before a provider call. Candidates already satisfy
// required capabilities. PreviousFailed includes failures after partial output.
type Request struct {
	Candidates     []Candidate
	Previous       string
	PreviousFailed bool
}

// Policy returns the ordered profile IDs to try. Each is attempted at most once.
// An empty order, duplicate, or unknown ID fails before provider execution.
// Implementations shared across sessions must support concurrent calls.
type Policy interface {
	Order(context.Context, Request) ([]string, error)
}

type PolicyFunc func(context.Context, Request) ([]string, error)

func (f PolicyFunc) Order(ctx context.Context, r Request) ([]string, error) { return f(ctx, r) }

// Sticky keeps the selected profile first until it fails. After a failure it
// rotates to the next eligible profile. It never probes the primary on recovery.
type Sticky struct{}

func (Sticky) Order(_ context.Context, r Request) ([]string, error) {
	start := 0
	for i, candidate := range r.Candidates {
		if candidate.ID == r.Previous {
			start = i
			if r.PreviousFailed {
				start++
			}
			break
		}
	}
	order := make([]string, len(r.Candidates))
	for i := range order {
		order[i] = r.Candidates[(start+i)%len(r.Candidates)].ID
	}
	return order, nil
}

type Config struct {
	Profiles []Profile
	Policy   Policy // nil uses Sticky
	// FailoverOn defaults to types.IsTransient. Cancellation never fails over.
	FailoverOn func(error) bool
	// Required applies to every request, in addition to tools/schema needs.
	Required []types.Capability
}

type Router struct{ cfg Config }

func New(cfg Config) (*Router, error) {
	if len(cfg.Profiles) == 0 {
		return nil, errors.New("router requires at least one profile")
	}
	names := map[string]bool{}
	for _, p := range cfg.Profiles {
		if p.ID == "" || p.Provider == nil || names[p.ID] {
			return nil, fmt.Errorf("invalid or duplicate routing profile %q", p.ID)
		}
		names[p.ID] = true
	}
	cfg.Profiles = append([]Profile(nil), cfg.Profiles...)
	cfg.Required = append([]types.Capability(nil), cfg.Required...)
	if cfg.Policy == nil {
		cfg.Policy = Sticky{}
	}
	if cfg.FailoverOn == nil {
		cfg.FailoverOn = types.IsTransient
	}
	return &Router{cfg: cfg}, nil
}

// Session owns routing state. A session rejects overlapping requests rather
// than letting completion order decide its sticky route. Separate sessions can
// call the same immutable profiles in parallel.
type Session struct {
	router   *Router
	mu       sync.Mutex
	busy     bool
	previous string
	failed   bool
	initErr  error
}

func (r *Router) Session() *Session { return &Session{router: r} }

// NewSession implements types.SessionProvider for independently routed children.
func (s *Session) NewSession() types.Provider { return &Session{router: s.router, initErr: s.initErr} }

func (s *Session) Name() string { return "router" }

// Model is the profile ID, not a model name with settings copied across vendors.
func (s *Session) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previous
}

// WithModel selects a complete named profile. Unknown IDs fail on use. It never
// copies one model's temperature, reasoning, or cache handle onto another model.
func (s *Session) WithModel(id string) types.Provider {
	for _, p := range s.router.cfg.Profiles {
		if p.ID == id {
			cfg := s.router.cfg
			cfg.Profiles = []Profile{p}
			cfg.Policy = Sticky{}
			r, _ := New(cfg)
			return r.Session()
		}
	}
	return &Session{router: s.router, initErr: fmt.Errorf("unknown routing profile %q", id)}
}

// Candidates reports each profile independently, without exposing credentials.
func (r *Router) Candidates() []Candidate {
	out := make([]Candidate, 0, len(r.cfg.Profiles))
	for _, p := range r.cfg.Profiles {
		caps, _ := types.ProviderCapabilities(p.Provider)
		out = append(out, Candidate{ID: p.ID, Capabilities: caps.With()})
	}
	return out
}

// Capabilities is the safe common contract. Per-profile records are available
// through Candidates. Pricing remains conservative across possible failovers.
func (s *Session) Capabilities() types.ModelCapabilities {
	candidates := s.router.Candidates()
	out := candidates[0].Capabilities
	for _, candidate := range candidates[1:] {
		out = out.Intersect(candidate.Capabilities)
	}
	return out
}

func (s *Session) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return s.stream(ctx, messages, tools, nil)
}

func (s *Session) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	return s.stream(ctx, messages, tools, schema)
}

func (s *Session) stream(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return nil, ErrSessionBusy
	}
	s.busy = true
	req := Request{Previous: s.previous, PreviousFailed: s.failed}
	s.mu.Unlock()
	unlock := func() { s.mu.Lock(); s.busy = false; s.mu.Unlock() }
	want := append([]types.Capability(nil), s.router.cfg.Required...)
	if len(tools) > 0 {
		want = append(want, types.CapTools)
	}
	if schema != nil {
		want = append(want, types.CapStructuredOutput)
	}
	eligible := map[string]types.Provider{}
	for i, candidate := range s.router.Candidates() {
		p := s.router.cfg.Profiles[i].Provider
		if !candidate.Capabilities.SupportsAll(want...) {
			continue
		}
		if schema != nil {
			if _, ok := p.(types.StructuredOutputProvider); !ok {
				continue
			}
		}
		req.Candidates = append(req.Candidates, candidate)
		eligible[candidate.ID] = p
	}
	order, err := s.router.cfg.Policy.Order(ctx, req)
	if err == nil && len(order) == 0 {
		err = errors.New("no eligible routing profile")
	}
	seen := map[string]bool{}
	for _, id := range order {
		if eligible[id] == nil || seen[id] {
			err = fmt.Errorf("routing policy returned invalid profile %q", id)
			break
		}
		seen[id] = true
	}
	if err != nil {
		unlock()
		return nil, err
	}
	out := make(chan types.Delta)
	go func() {
		// Release before close so a consumer can immediately start the next turn.
		defer close(out)
		defer unlock()
		s.relay(ctx, out, order, eligible, messages, tools, schema)
	}()
	return out, nil
}

func (s *Session) setOutcome(id string, failed bool) {
	s.mu.Lock()
	s.previous, s.failed = id, failed
	s.mu.Unlock()
}

func (s *Session) relay(ctx context.Context, out chan<- types.Delta, order []string, eligible map[string]types.Provider, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) {
	send := func(d types.Delta) bool {
		select {
		case out <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var failures []error
	for _, id := range order {
		if ctx.Err() != nil {
			return
		}
		p := eligible[id]
		if !send(types.RouteDelta{Profile: id, Provider: types.ProviderName(p), Model: types.ProviderModel(p)}) {
			return
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		var src <-chan types.Delta
		var err error
		if schema != nil {
			src, err = p.(types.StructuredOutputProvider).ChatStreamWithSchema(attemptCtx, messages, tools, schema)
		} else {
			src, err = p.ChatStream(attemptCtx, messages, tools)
		}
		if err == nil && src == nil {
			err = errors.New("provider returned a nil stream")
		}
		committed := false
		if err == nil {
		read:
			for {
				select {
				case <-ctx.Done():
					cancel()
					go drain(src)
					return
				case d, ok := <-src:
					if !ok {
						cancel()
						s.setOutcome(id, false)
						return
					}
					if failure, ok := d.(types.ErrorDelta); ok {
						err = failure.Error
						if err == nil {
							err = errors.New("provider emitted an empty error")
						}
						cancel()
						go drain(src)
						break read
					}
					if _, terminal := d.(types.DoneDelta); terminal {
						continue // channel closure is authoritative
					}
					// Even usage commits an attempt. Mixing usage from different
					// rate cards would corrupt the agent's single-call aggregation.
					committed = true
					if !send(d) {
						cancel()
						go drain(src)
						return
					}
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		s.setOutcome(id, s.router.cfg.FailoverOn(err))
		failures = append(failures, fmt.Errorf("profile %s: %w", id, err))
		if committed || !s.router.cfg.FailoverOn(err) {
			send(types.ErrorDelta{Error: err})
			return
		}
	}
	send(types.ErrorDelta{Error: &types.FallbackError{Errors: failures}})
}

func drain(src <-chan types.Delta) {
	for range src {
	}
}
