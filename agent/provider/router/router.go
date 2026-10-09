// Package router selects complete provider configurations and keeps session
// affinity. Create one Session per conversation owner, not one per deployment.
//
// A Policy orders profiles per request; the default Sticky keeps the last
// profile until it fails. A SessionRouterPolicy such as Affinity orders them
// from the session's RouteState instead, which adds a sustained-failure
// threshold, recovery probes bounded by the prompt-cache switch cost, and a
// state the host can persist with the conversation (Session.RouteState and
// Session.RestoreRouteState).
//
// Within one request, a failure fails over before any output commits the
// attempt. The remaining order depends on the error class: a context-length
// error moves only to a larger context window, and a content-filter refusal,
// when FailoverOn allows it, moves only to another provider. Route locks keep
// a conversation on the profile that holds its tool loop, signed reasoning, or
// provider-side cache.
//
// Groups name ordered sets of profiles, such as the chain of one catalog
// preset. Pinning a group through ConfigContent.Model restricts a request to
// its members, and DefaultGroup restricts unpinned requests, so the chain a
// caller picked is exactly the failover order.
package router

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/optionscheck"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

var ErrSessionBusy = errors.New("routing session already has an active request")

// ErrUnknownProfile reports a profile ID that the router does not define.
var ErrUnknownProfile = errors.New("unknown routing profile")

// Route reasons reported on types.RouteDelta.Reason. An empty reason means the
// policy's first choice.
const (
	ReasonPinned           = "pinned"            // ConfigContent.Model selected the profile
	ReasonFailover         = "failover"          // the previous attempt failed with a transient error
	ReasonContextLength    = "context_length"    // the previous attempt did not fit its context window
	ReasonContentFilter    = "content_filter"    // the previous attempt was refused by a content filter
	ReasonSustainedFailure = "sustained_failure" // the sticky profile failed too often and moved
	ReasonContextWindow    = "context_window"    // the request is estimated not to fit the sticky profile
	ReasonReprobe          = "reprobe"           // the session is trying its preferred profile again
	ReasonLocked           = "locked"            // a route lock kept the previous profile
	// ReasonOptions means the group's first member could not honor the
	// request's options merged with its own, so a later member serves.
	ReasonOptions = "options"
)

// Profile binds an ID to a fully configured provider. Two profiles may target
// the same model with different reasoning, sampling, cache, or endpoint options.
// Providers and their configuration must remain immutable after New.
type Profile struct {
	ID       string
	Provider types.Provider
	// ConfigHash, Preset and CatalogRevision identify a profile built from a
	// declared catalog preset. They are copied onto every RouteDelta.
	ConfigHash      string
	Preset          string
	CatalogRevision string
}

// Candidate is the detached capability record visible to a routing policy.
type Candidate struct {
	ID           string
	Capabilities types.ModelCapabilities
	// Options are the profile's configured options, when its provider
	// reports them (types.OptionsReporter).
	Options    types.RequestOptions
	ConfigHash string
	Preset     string
}

// Headroom is the budget left when the request is routed. Known is false when
// the router has no budget configured.
type Headroom struct {
	Known bool
	// Remaining is the unreserved allowance, or -1 when the budget has no
	// monetary limit.
	Remaining types.Cost
	Status    types.BudgetStatus
}

// RouteContext describes the request being routed. Messages and Tools are the
// caller's slices and must not be modified.
type RouteContext struct {
	// Candidates are the eligible profiles in configuration order. They
	// already satisfy the required capabilities, tools, schema, and options.
	Candidates []Candidate
	Messages   []types.Message
	Tools      []types.ToolDef
	Schema     bool
	// EstimatedTokens approximates the prompt size with types.EstimateTokens.
	EstimatedTokens int
	Headroom        Headroom
	// Pinned is the profile selected through ConfigContent.Model, if any. The
	// router places it first regardless of the policy's order.
	Pinned string
	// Locks are the route locks in force for this request. See LockToolLoop.
	Locks []string
}

// Request is evaluated once before a provider call. Candidates already satisfy
// required capabilities. PreviousFailed includes failures after partial output.
type Request struct {
	Candidates     []Candidate
	Previous       string
	PreviousFailed bool
	// Route carries the request facts. Route.Candidates equals Candidates.
	Route RouteContext
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
// Use Affinity for a failure threshold and recovery probes.
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
	// Groups name ordered sets of profile IDs, such as the chain of one
	// preset. WithModel accepts a group name and then restricts candidates to
	// its members, in group order. Group names and profile IDs share one
	// namespace; a group may reuse a profile's ID only when that profile is
	// its sole member.
	Groups map[string][]string
	// DefaultGroup, when set, restricts unpinned requests to that group's
	// members, so profiles in other groups are reachable only through a pin.
	DefaultGroup string
	// Policy orders profiles per request. Nil uses Sticky. SessionPolicy, when
	// set, takes precedence.
	Policy Policy
	// SessionPolicy orders profiles from the session's RouteState.
	SessionPolicy SessionRouterPolicy
	// FailoverOn defaults to DefaultFailoverOn. Cancellation never fails over.
	FailoverOn func(error) bool
	// Required applies to every request, in addition to tools/schema needs.
	Required []types.Capability
	// Revision identifies this routing configuration. RouteState records it,
	// and a restored state from another revision keeps only its profiles.
	Revision string
	// Budget, when set, is read for RouteContext.Headroom. The router does not
	// reserve from it.
	Budget *types.Budget
	// PromptCacheTTL is how long a served prefix is assumed to stay warm in the
	// provider's prompt cache. Zero uses five minutes.
	PromptCacheTTL time.Duration
	// Now is the clock used for warm-prefix expiry. Nil uses time.Now.
	Now func() time.Time
}

// DefaultFailoverOn fails over on transient errors and on context-length
// errors. A context-length failure only moves to a profile with a larger
// declared context window. Content-filter refusals do not fail over by
// default: sending a refused prompt to another provider is a policy decision
// the host must make explicitly, for example with
// FailoverOn: func(err error) bool { return DefaultFailoverOn(err) || types.IsContentFilter(err) }.
func DefaultFailoverOn(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	return types.IsTransient(err) || types.IsContextLength(err)
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
	groups := make(map[string][]string, len(cfg.Groups))
	for name, members := range cfg.Groups {
		// A group may share its name with a profile only when it is that
		// one profile, as for a preset built from a single model.
		if name == "" || (names[name] && (len(members) != 1 || members[0] != name)) {
			return nil, fmt.Errorf("routing group %q is empty or collides with a profile ID", name)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("routing group %q has no members", name)
		}
		seen := map[string]bool{}
		for _, id := range members {
			if !names[id] || seen[id] {
				return nil, fmt.Errorf("routing group %q: unknown or duplicate profile %q", name, id)
			}
			seen[id] = true
		}
		groups[name] = append([]string(nil), members...)
	}
	cfg.Groups = groups
	if cfg.DefaultGroup != "" && cfg.Groups[cfg.DefaultGroup] == nil {
		return nil, fmt.Errorf("default routing group %q is not defined", cfg.DefaultGroup)
	}
	cfg.Profiles = append([]Profile(nil), cfg.Profiles...)
	cfg.Required = append([]types.Capability(nil), cfg.Required...)
	if cfg.Policy == nil {
		cfg.Policy = Sticky{}
	}
	if cfg.FailoverOn == nil {
		cfg.FailoverOn = DefaultFailoverOn
	}
	if cfg.PromptCacheTTL <= 0 {
		cfg.PromptCacheTTL = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Router{cfg: cfg}, nil
}

// Close implements types.Closer by closing every profile's provider. Sessions
// share these providers, so close the router only after its last session.
func (r *Router) Close() error {
	var errs []error
	for _, p := range r.cfg.Profiles {
		if err := types.CloseProvider(p.Provider); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Router) hasProfile(id string) bool {
	for _, p := range r.cfg.Profiles {
		if p.ID == id {
			return true
		}
	}
	return false
}

// hasTarget reports whether name is a profile ID or a group.
func (r *Router) hasTarget(name string) bool {
	return r.hasProfile(name) || r.cfg.Groups[name] != nil
}

// Groups returns the group names, sorted.
func (r *Router) Groups() []string {
	out := make([]string, 0, len(r.cfg.Groups))
	for name := range r.cfg.Groups {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Group returns a group's members in order.
func (r *Router) Group(name string) []string { return append([]string(nil), r.cfg.Groups[name]...) }

// sessionState is shared by a session and the pinned views WithModel returns,
// so a pin keeps the session's affinity and failure history.
type sessionState struct {
	mu    sync.Mutex
	busy  bool
	state RouteState
	// sent holds the options last sent to each profile, so a reasoning
	// change that resets a provider's prompt cache can be reported.
	sent map[string]types.RequestOptions
}

// Session owns routing state. A session rejects overlapping requests rather
// than letting completion order decide its sticky route. Separate sessions can
// call the same immutable profiles in parallel.
type Session struct {
	router  *Router
	shared  *sessionState
	pin     string
	initErr error
}

var (
	_ types.Provider                 = (*Session)(nil)
	_ types.NamedProvider            = (*Session)(nil)
	_ types.ModelProvider            = (*Session)(nil)
	_ types.ModelSwitcher            = (*Session)(nil)
	_ types.CapabilityReporter       = (*Session)(nil)
	_ types.ContentNegotiator        = (*Session)(nil)
	_ types.StructuredOutputProvider = (*Session)(nil)
	_ types.OptionsProvider          = (*Session)(nil)
	_ types.SessionProvider          = (*Session)(nil)
	_ wrapper.MultiWrapper           = (*Session)(nil)
)

func (r *Router) Session() *Session {
	return &Session{router: r, shared: &sessionState{state: RouteState{Revision: r.cfg.Revision}}}
}

// NewSession implements types.SessionProvider for independently routed children.
// The child starts with empty routing state and keeps this session's pin.
func (s *Session) NewSession() types.Provider {
	child := s.router.Session()
	child.pin, child.initErr = s.pin, s.initErr
	child.shared.state.Pin = s.currentPin()
	return child
}

func (s *Session) Name() string { return "router" }

// Model is the profile ID, not a model name with settings copied across vendors.
// A pinned view reports its pin; otherwise it is the last profile attempted.
func (s *Session) Model() string {
	if s.pin != "" {
		return s.pin
	}
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	return s.shared.state.Last
}

// WithModel selects a complete named profile, or a group of them. Unknown
// names fail on use with ErrUnknownProfile. It never
// copies one model's temperature, reasoning, or cache handle onto another model.
//
// The returned provider shares this session's state, so a pin keeps the
// session's sticky history, failure counts, and failover to other profiles.
// Using it records the pin in RouteState, and the session then keeps that
// profile first on later requests made through either value. The agent loop
// re-applies ConfigContent.Model on every turn, and the pin must survive the
// turns where the provider already reports the pinned profile and the loop
// skips the switch. Another WithModel or Unpin changes it.
func (s *Session) WithModel(id string) types.Provider {
	if !s.router.hasTarget(id) {
		return &Session{router: s.router, shared: s.shared, initErr: fmt.Errorf("%w %q", ErrUnknownProfile, id)}
	}
	if s.pin == id {
		return s
	}
	return &Session{router: s.router, shared: s.shared, pin: id, initErr: s.initErr}
}

// Unpin clears the pin recorded by a WithModel view, returning the session to
// its policy's order.
func (s *Session) Unpin() {
	s.shared.mu.Lock()
	s.shared.state.Pin = ""
	s.shared.mu.Unlock()
}

func (s *Session) currentPin() string {
	if s.pin != "" {
		return s.pin
	}
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	return s.shared.state.Pin
}

// Unwrap returns every profile's provider in configuration order. See
// package wrapper.
func (s *Session) Unwrap() []types.Provider {
	out := make([]types.Provider, len(s.router.cfg.Profiles))
	for i, p := range s.router.cfg.Profiles {
		out[i] = p.Provider
	}
	return out
}

// Candidates reports each profile independently, without exposing credentials.
func (r *Router) Candidates() []Candidate {
	out := make([]Candidate, 0, len(r.cfg.Profiles))
	for _, p := range r.cfg.Profiles {
		caps, _ := types.ProviderCapabilities(p.Provider)
		opts, _ := types.ProviderEffectiveOptions(p.Provider)
		out = append(out, Candidate{ID: p.ID, Capabilities: caps.With(), Options: opts, ConfigHash: p.ConfigHash, Preset: p.Preset})
	}
	return out
}

// Capabilities is the safe common contract. Per-profile records are available
// through Candidates. Pricing remains conservative across possible failovers.
// Capabilities that need request options are dropped when no profile can
// receive them, since no profile is then eligible for an options call.
func (s *Session) Capabilities() types.ModelCapabilities {
	ids, _ := s.router.members(s.currentPin())
	var candidates []Candidate
	var members []types.Provider
	for i, c := range s.router.Candidates() {
		if ids == nil || slices.Contains(ids, c.ID) {
			candidates = append(candidates, c)
			members = append(members, s.router.cfg.Profiles[i].Provider)
		}
	}
	out := candidates[0].Capabilities
	for _, candidate := range candidates[1:] {
		out = out.Intersect(candidate.Capabilities)
	}
	return optionscheck.Narrow(out, members...)
}

// reachable returns the profiles a request may currently use: the pinned or
// default group, or every profile.
func (s *Session) reachable() []Profile {
	ids, _ := s.router.members(s.currentPin())
	if ids == nil {
		return s.router.cfg.Profiles
	}
	var out []Profile
	for _, p := range s.router.cfg.Profiles {
		if slices.Contains(ids, p.ID) {
			out = append(out, p)
		}
	}
	return out
}

// ContentSupport implements types.ContentNegotiator as the intersection over
// the profiles, because any profile may serve the request.
func (s *Session) ContentSupport() types.ContentSupport {
	profiles := s.reachable()
	out := types.ProviderContentSupport(profiles[0].Provider)
	for _, p := range profiles[1:] {
		next := types.ProviderContentSupport(p.Provider)
		merged := map[types.MediaType]bool{}
		for mt, ok := range out.NativeTypes {
			if ok && next.NativeTypes[mt] {
				merged[mt] = true
			}
		}
		out = types.ContentSupport{NativeTypes: merged}
	}
	return out
}

func (s *Session) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return s.stream(ctx, request{messages: messages, tools: tools})
}

func (s *Session) ChatStreamWithSchema(ctx context.Context, messages []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) (<-chan types.Delta, error) {
	return s.stream(ctx, request{messages: messages, tools: tools, schema: schema})
}

// ChatStreamWithOptions implements types.OptionsProvider. Only profiles that
// accept request options and whose capabilities validate them are eligible,
// so a forced tool choice never reaches a profile that would drop it.
func (s *Session) ChatStreamWithOptions(ctx context.Context, messages []types.Message, tools []types.ToolDef, opts types.RequestOptions) (<-chan types.Delta, error) {
	return s.stream(ctx, request{messages: messages, tools: tools, opts: &opts})
}

// request is one provider call as the caller made it.
type request struct {
	messages []types.Message
	tools    []types.ToolDef
	schema   *types.ParameterSchema
	opts     *types.RequestOptions
}

func (q request) call(ctx context.Context, p types.Provider) (<-chan types.Delta, error) {
	switch {
	case q.opts != nil:
		op, ok := p.(types.OptionsProvider)
		if !ok {
			return nil, optionscheck.Unsupported(p)
		}
		return op.ChatStreamWithOptions(ctx, q.messages, q.tools, *q.opts)
	case q.schema != nil:
		return p.(types.StructuredOutputProvider).ChatStreamWithSchema(ctx, q.messages, q.tools, q.schema)
	default:
		return p.ChatStream(ctx, q.messages, q.tools)
	}
}

// compiled is what a profile sends for one request: its configured options
// with the request's on top and every dial compiled for its model.
type compiled struct {
	opts   types.RequestOptions
	report *types.DialReport
}

// dialSurface names the API the profile's adapter serves, for dials whose
// mapping depends on it.
func dialSurface(p types.Provider) string {
	if r, ok := wrapper.As[types.DialSurfaceReporter](p); ok {
		return r.DialSurface()
	}
	return ""
}

// eligible reports why a profile cannot serve q, or nil, and what it would
// send. Dials compile here, per candidate: a contractual dial the model
// cannot honor filters the profile out, while an advisory dial that is
// mapped or dropped never does.
func (q request) eligible(c Candidate, p types.Provider, want []types.Capability, prev *types.RequestOptions) (compiled, error) {
	if missing := c.Capabilities.Missing(want...); len(missing) > 0 {
		return compiled{}, fmt.Errorf("profile %s: %w: missing %v", c.ID, types.ErrInvalidModelConfig, missing)
	}
	if q.schema != nil {
		if _, ok := p.(types.StructuredOutputProvider); !ok {
			return compiled{}, fmt.Errorf("profile %s: %w", c.ID, types.ErrSchemaUnsupported)
		}
	}
	merged := c.Options
	if q.opts != nil {
		if _, ok := p.(types.OptionsProvider); !ok {
			return compiled{}, fmt.Errorf("profile %s: %w", c.ID, types.ErrOptionsUnsupported)
		}
		merged = merged.Merge(*q.opts)
	}
	ctx := types.DialContext{Tools: len(q.tools) > 0, Schema: q.schema != nil, Surface: dialSurface(p), Previous: prev}
	eff, rep, err := types.CompileOptions(c.Capabilities, merged, ctx)
	if err != nil {
		return compiled{}, fmt.Errorf("profile %s: %w", c.ID, err)
	}
	if q.opts != nil {
		// Validate what the adapter will send: its configured options with
		// this request's overrides on top.
		if err := c.Capabilities.ValidateOptions(eff); err != nil {
			return compiled{}, fmt.Errorf("profile %s: %w", c.ID, err)
		}
	}
	return compiled{opts: eff, report: rep}, nil
}

// plan is the validated attempt order for one request.
type plan struct {
	order      []string
	reason     string
	candidates map[string]Candidate
	providers  map[string]types.Provider
	compiled   map[string]compiled
	// home is the sticky profile once this request's decision applies.
	home string
	// switchTo, when set, makes home a different profile before the request.
	switchTo string
	// probe is tried first; it becomes home only if it serves the request.
	probe string
	// followServed makes home whichever profile serves the request, which is
	// how a plain Policy such as Sticky keeps affinity.
	followServed bool
	hardLocked   bool
	messages     int
	locks        []string
}

func (s *Session) stream(ctx context.Context, q request) (<-chan types.Delta, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.shared.mu.Lock()
	if s.shared.busy {
		s.shared.mu.Unlock()
		return nil, ErrSessionBusy
	}
	s.shared.busy = true
	if s.pin != "" {
		s.shared.state.Pin = s.pin
	}
	st := s.shared.state.clone()
	sent := maps.Clone(s.shared.sent)
	s.shared.mu.Unlock()
	unlock := func() { s.shared.mu.Lock(); s.shared.busy = false; s.shared.mu.Unlock() }

	p, err := s.plan(ctx, q, st, sent)
	if err != nil {
		unlock()
		return nil, err
	}
	s.shared.mu.Lock()
	s.shared.state.applyPlan(p)
	s.shared.mu.Unlock()

	out := make(chan types.Delta)
	go func() {
		// Release before close so a consumer can immediately start the next turn.
		defer close(out)
		defer unlock()
		s.relay(ctx, out, p, q)
	}()
	return out, nil
}

// plan filters the profiles, asks the policy for an order, and applies the
// pin and the route locks.
func (s *Session) plan(ctx context.Context, q request, st RouteState, sent map[string]types.RequestOptions) (plan, error) {
	cfg := s.router.cfg
	want := append([]types.Capability(nil), cfg.Required...)
	if len(q.tools) > 0 {
		want = append(want, types.CapTools)
	}
	if q.schema != nil {
		want = append(want, types.CapStructuredOutput)
	}
	p := plan{candidates: map[string]Candidate{}, providers: map[string]types.Provider{}, compiled: map[string]compiled{}, messages: len(q.messages)}
	rc := RouteContext{
		Messages: q.messages, Tools: q.tools, Schema: q.schema != nil,
		EstimatedTokens: types.EstimateTokens(q.messages),
		Pinned:          st.Pin,
	}
	members, group := s.router.members(st.Pin)
	skippedFirst, reasons := s.collectCandidates(q, want, st, members, sent, &p, &rc)
	if cfg.Budget != nil {
		rc.Headroom = Headroom{Known: true, Remaining: cfg.Budget.Remaining(), Status: cfg.Budget.Status()}
	}
	if last, ok := p.providers[st.Last]; ok {
		rc.Locks = detectLocks(q.messages, last)
	}
	p.locks = rc.Locks
	st = st.effective(cfg, len(q.messages))

	var decision RouteDecision
	var err error
	if cfg.SessionPolicy != nil {
		decision, err = cfg.SessionPolicy.Select(ctx, rc, st)
	} else {
		decision.Order, err = cfg.Policy.Order(ctx, Request{Candidates: rc.Candidates, Previous: st.Last, PreviousFailed: st.LastFailed, Route: rc})
		p.followServed = true
	}
	if err == nil && len(decision.Order) == 0 {
		err = errors.New("no eligible routing profile")
		if len(rc.Candidates) == 0 && len(reasons) > 0 {
			// Each member's own reason, so a raw option every member
			// rejects reads as a configuration error, not an outage.
			err = fmt.Errorf("no eligible routing profile: %w", errors.Join(reasons...))
		}
	}
	seen := map[string]bool{}
	for _, id := range decision.Order {
		if p.providers[id] == nil || seen[id] {
			err = fmt.Errorf("routing policy returned invalid profile %q", id)
			break
		}
		seen[id] = true
	}
	for _, id := range []string{decision.Profile, decision.Probe} {
		if err == nil && id != "" && !seen[id] {
			err = fmt.Errorf("routing policy selected profile %q outside its order", id)
		}
	}
	if err != nil {
		return plan{}, err
	}
	if members != nil {
		decision.Order = groupOrder(decision.Order, members)
	}
	p.order, p.reason = decision.Order, decision.Reason
	if skippedFirst && p.reason == "" && q.opts != nil {
		p.reason = ReasonOptions
	}
	p.home, p.switchTo, p.probe = st.Profile, decision.Profile, decision.Probe
	if p.switchTo == "" && p.home == "" && !p.followServed {
		p.switchTo = p.order[0] // the first decision of a new session
	}

	p.applyPinsAndLocks(st, rc.Locks, group)
	if p.switchTo != "" {
		p.home = p.switchTo
	}
	if p.followServed {
		// A plain Policy's sticky profile follows whichever profile serves.
		p.switchTo = ""
	}
	return p, nil
}

// applyPinsAndLocks puts the pinned profile or group, or the locked previous
// profile, ahead of the policy's order.
func (p *plan) applyPinsAndLocks(st RouteState, locks []string, group string) {
	switch {
	case hasHardLock(locks):
		// The previous profile produced state another model cannot accept.
		// This outranks a pin: the pin stays in the state and takes effect on
		// the next turn that is not locked.
		p.order, p.reason, p.switchTo, p.probe, p.hardLocked = []string{st.Last}, ReasonLocked, st.Last, "", true
	case st.Pin != "" && p.providers[st.Pin] != nil:
		p.order = moveFirst(p.order, st.Pin)
		p.reason, p.switchTo, p.probe = ReasonPinned, st.Pin, ""
	case group != "" && group == st.Pin:
		// A group pin selects the group's first eligible member.
		p.reason, p.switchTo, p.probe = ReasonPinned, p.order[0], ""
	case len(locks) > 0 && !st.LastFailed && p.order[0] != st.Last && p.providers[st.Last] != nil:
		p.order = moveFirst(p.order, st.Last)
		p.reason, p.switchTo, p.probe = ReasonLocked, st.Last, ""
	}
}

// collectCandidates fills the eligible candidates of a request into p and rc,
// restricted to members when set. It reports whether the group's first
// member was skipped because it cannot serve the request, and why each
// skipped member was.
func (s *Session) collectCandidates(q request, want []types.Capability, st RouteState, members []string, sent map[string]types.RequestOptions, p *plan, rc *RouteContext) (bool, []error) {
	skippedFirst := false
	var reasons []error
	for i, candidate := range s.router.Candidates() {
		provider := s.router.cfg.Profiles[i].Provider
		var prev *types.RequestOptions
		if o, ok := sent[candidate.ID]; ok {
			prev = &o
		}
		c, err := q.eligible(candidate, provider, want, prev)
		if members != nil && !slices.Contains(members, candidate.ID) {
			// Outside the group, but the previous profile still matters: it
			// may hold a lock that outranks the group.
			if candidate.ID == st.Last && err == nil {
				p.candidates[candidate.ID] = candidate
				p.providers[candidate.ID] = provider
				p.compiled[candidate.ID] = c
			}
			continue
		}
		if err != nil {
			reasons = append(reasons, err)
			if members != nil && members[0] == candidate.ID {
				skippedFirst = true
			}
			continue
		}
		rc.Candidates = append(rc.Candidates, candidate)
		p.candidates[candidate.ID] = candidate
		p.providers[candidate.ID] = provider
		p.compiled[candidate.ID] = c
	}
	return skippedFirst, reasons
}

// members returns the profile IDs a request may use: the pinned group, or
// the default group when nothing is pinned. Nil means every profile. A pin
// naming a single profile keeps every profile reachable for failover.
func (r *Router) members(pin string) ([]string, string) {
	if g := r.cfg.Groups[pin]; g != nil {
		return g, pin
	}
	if pin == "" && r.cfg.DefaultGroup != "" {
		return r.cfg.Groups[r.cfg.DefaultGroup], r.cfg.DefaultGroup
	}
	return nil, ""
}

// groupOrder keeps a policy's order but restricted to the group, so a
// group's failover order is exactly its declared chain when the policy
// follows configuration order.
func groupOrder(order, members []string) []string {
	out := make([]string, 0, len(order))
	for _, id := range order {
		if slices.Contains(members, id) {
			out = append(out, id)
		}
	}
	return out
}

func moveFirst(order []string, id string) []string {
	out := []string{id}
	for _, o := range order {
		if o != id {
			out = append(out, o)
		}
	}
	return out
}

// outcome is what relay learned about one request.
type outcome struct {
	served     string // profile whose stream completed, empty if none
	last       string // last profile attempted
	lastFailed bool   // the last attempt failed with a failover-eligible error
	homeFailed bool   // the sticky profile was attempted and failed
	usage      types.UsageDelta
}

//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (s *Session) relay(ctx context.Context, out chan<- types.Delta, p plan, q request) {
	send := func(d types.Delta) bool {
		select {
		case out <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var res outcome
	defer func() {
		if res.last == "" || (ctx.Err() != nil && res.served == "") {
			return
		}
		s.shared.mu.Lock()
		s.shared.state.record(s.router.cfg, p, res)
		s.shared.mu.Unlock()
	}()
	failoverOn := s.router.cfg.FailoverOn
	var failures []error
	order, reason := p.order, p.reason
	for len(order) > 0 {
		if ctx.Err() != nil {
			return
		}
		id := order[0]
		order = order[1:]
		provider := p.providers[id]
		res.last = id
		if !send(s.routeDelta(id, provider, p.compiled[id], reason)) {
			return
		}
		s.shared.mu.Lock()
		if s.shared.sent == nil {
			s.shared.sent = map[string]types.RequestOptions{}
		}
		s.shared.sent[id] = p.compiled[id].opts.Clone()
		s.shared.mu.Unlock()
		attemptCtx, cancel := context.WithCancel(ctx)
		src, err := q.call(attemptCtx, provider)
		if err == nil && src == nil {
			err = errors.New("provider returned a nil stream")
		}
		committed := false
		var usage types.UsageDelta
		// held keeps the metadata deltas (usage, nested routes) that arrive
		// before the attempt's first output. They are forwarded once output
		// arrives or the stream completes, and dropped on failover, so the
		// caller never sees usage from two rate cards for one call.
		var held []types.Delta
		flush := func() bool {
			for _, h := range held {
				if !send(h) {
					return false
				}
			}
			held = nil
			return true
		}
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
						res.served, res.lastFailed, res.usage = id, false, usage
						flush()
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
					switch v := d.(type) {
					case types.UsageDelta:
						usage = usage.Merge(v)
						if !committed {
							held = append(held, d)
							continue
						}
					case types.RouteDelta:
						if !committed {
							held = append(held, d)
							continue
						}
					}
					// Output commits the attempt: replaying it on another
					// profile would duplicate what the caller has seen. Usage
					// alone does not, because adapters such as Anthropic's
					// report usage when the message starts, before any output,
					// and the most common transient failure follows it.
					committed = true
					if !flush() || !send(d) {
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
		eligible := failoverOn(err)
		res.lastFailed = eligible
		if id == p.home {
			res.homeFailed = res.homeFailed || eligible
		}
		failures = append(failures, fmt.Errorf("profile %s: %w", id, err))
		if committed || !eligible || p.hardLocked {
			// The request ends here, so usage held from this attempt is
			// real spend on the profile that failed: report it.
			if flush() {
				send(types.ErrorDelta{Error: err})
			}
			return
		}
		order, reason = reroute(order, p.candidates[id], p.candidates, err)
	}
	send(types.ErrorDelta{Error: &types.FallbackError{Errors: failures}})
}

// reroute filters the remaining order by the class of the failure. A
// context-length failure only moves to a larger declared context window,
// largest first, and a content-filter refusal only moves to a different
// provider. Any other failover-eligible error keeps the order.
func reroute(order []string, failed Candidate, candidates map[string]Candidate, err error) ([]string, string) {
	switch {
	case types.IsContextLength(err):
		window := failed.Capabilities.ContextWindow
		var out []string
		for _, id := range order {
			if w := candidates[id].Capabilities.ContextWindow; window > 0 && w > window {
				out = append(out, id)
			}
		}
		slices.SortStableFunc(out, func(a, b string) int {
			return candidates[b].Capabilities.ContextWindow - candidates[a].Capabilities.ContextWindow
		})
		return out, ReasonContextLength
	case types.IsContentFilter(err):
		var out []string
		for _, id := range order {
			if candidates[id].Capabilities.Provider != failed.Capabilities.Provider {
				out = append(out, id)
			}
		}
		return out, ReasonContentFilter
	default:
		return order, ReasonFailover
	}
}

func drain(src <-chan types.Delta) {
	for range src {
	}
}

// routeDelta describes one attempt, including the effective options the
// adapter sends for it and how its dials compiled.
func (s *Session) routeDelta(id string, provider types.Provider, c compiled, reason string) types.RouteDelta {
	// The profile's provider is usually decorated (retry, attempt deadline);
	// the route names the adapter underneath, the vendor that served.
	d := types.RouteDelta{Profile: id, Provider: wrapper.InnermostName(provider), Model: types.ProviderModel(provider), Reason: reason}
	for _, prof := range s.router.cfg.Profiles {
		if prof.ID == id {
			d.Preset, d.ConfigHash, d.CatalogRevision = prof.Preset, prof.ConfigHash, prof.CatalogRevision
			break
		}
	}
	if _, ok := provider.(types.OptionsReporter); ok {
		opts := c.opts.Clone()
		d.Options = &opts
	}
	if c.report != nil {
		r := c.report.Clone()
		d.Dials = &r
	}
	return d
}
