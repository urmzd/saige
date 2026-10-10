// Package agenthost is the session layer the saige commands share when they
// host an agent for another program: saige serve over HTTP, saige acp over
// the Agent Client Protocol, and saige-mcp's agent tool. A session owns one
// agent, usually bound from a pinned definition, its conversation, the run
// in progress, and the grant cap approvals are checked against.
package agenthost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/definition/bind"
	"github.com/urmzd/saige/agent/types"
)

// Errors a Manager or Session returns.
var (
	// ErrBusy is returned when a session already runs a turn.
	ErrBusy = errors.New("a turn is already running in this session")
	// ErrLimit is returned when the manager holds its maximum of sessions.
	ErrLimit = errors.New("session limit reached")
	// ErrClosed is returned by a session that has ended.
	ErrClosed = errors.New("session closed")
	// ErrNoTurn is returned when a decision arrives and no turn runs.
	ErrNoTurn = errors.New("no turn is running in this session")
)

// Agent is an agent built for one session, with what goes with it.
type Agent struct {
	Agent *agentsdk.Agent
	// Release frees what the agent holds, such as MCP connections. It may
	// be nil.
	Release func()
	// CheckGrant rejects a grant the agent's definition does not allow. It
	// may be nil, which allows every scope.
	CheckGrant func(*types.GrantRequest) error
	// MaxGrant is the widest grant scope the definition allows; empty
	// allows every scope.
	MaxGrant types.GrantScope
	// Tools are the root agent's tools, for hosts that describe a call
	// before it runs. It may be nil.
	Tools *types.ToolRegistry
	// Pin names the bound definition; zero for an agent not bound from
	// one.
	Pin bind.Pin
	// Info describes the agent to clients, such as the pin.
	Info any
}

// FromBound builds a session agent from a bound definition. extra options
// apply after the binding's. An agent whose definition declares no
// approval block still gets an empty approval policy, so the grants a host
// accepts take effect.
func FromBound(b *bind.Bound, extra ...agentsdk.Option) (Agent, error) {
	opts := extra
	if b.Config.ApprovalPolicy == nil {
		opts = append([]agentsdk.Option{agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{})}, extra...)
	}
	a, err := b.NewAgent(opts...)
	if err != nil {
		return Agent{}, err
	}
	return Agent{
		Agent:      a,
		Release:    func() { _ = b.Close(context.Background()) },
		CheckGrant: b.CheckGrant,
		MaxGrant:   b.MaxGrant,
		Tools:      b.Config.Tools,
		Pin:        b.Pin(),
		Info:       b.Pin(),
	}, nil
}

// AllowsGrant reports whether a grant of scope is within the agent's cap.
func (a Agent) AllowsGrant(scope types.GrantScope) bool {
	return a.MaxGrant == "" || definition.GrantRank(scope) <= definition.GrantRank(a.MaxGrant)
}

// ValidateGrant checks the grant a client attached to a decision: only an
// approval can carry one, its scope and matchers must be well formed, and
// its expiry must lie in the future.
func ValidateGrant(g *types.GrantRequest, approved bool, now time.Time) error {
	if g == nil {
		return nil
	}
	if !approved {
		return errors.New(`"grant" requires "approved": true`)
	}
	if err := g.Validate(); err != nil {
		return err
	}
	if !g.ExpiresAt.IsZero() && !g.ExpiresAt.After(now) {
		return fmt.Errorf("%w: expires_at %s is not in the future", types.ErrInvalidGrant, g.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// Session is one hosted conversation. H is the host's own per-session
// state, such as serve's event buffers.
type Session[H any] struct {
	ID string
	// Host is the host's state. The session never reads it.
	Host H
	// Artifacts holds the media the session's client uploaded and the
	// media the host externalized from its runs.
	Artifacts *Artifacts

	mu        sync.Mutex
	agent     Agent
	stream    *agentsdk.EventStream
	running   bool
	idleSince time.Time
	closed    bool
}

// Agent returns the session's agent.
func (s *Session[H]) Agent() Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent
}

// Start runs one turn on the session's agent. The run uses ctx, not a
// request's context, so it outlives the call that started it. A session
// runs one turn at a time.
func (s *Session[H]) Start(ctx context.Context, msgs ...types.Message) (*agentsdk.EventStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.running {
		return nil, ErrBusy
	}
	stream := s.agent.Agent.Invoke(ctx, msgs)
	s.stream, s.running = stream, true
	go func() {
		_ = stream.Wait()
		s.mu.Lock()
		if s.stream == stream {
			s.running = false
			s.idleSince = time.Now()
		}
		s.mu.Unlock()
	}()
	return stream, nil
}

// Running reports whether a turn runs.
func (s *Session[H]) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Stream returns the running turn, or the last one, or nil.
func (s *Session[H]) Stream() *agentsdk.EventStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

// Cancel stops the running turn, if any.
func (s *Session[H]) Cancel() {
	s.mu.Lock()
	stream, running := s.stream, s.running
	s.mu.Unlock()
	if running {
		stream.Cancel()
	}
}

// Replace swaps the session's agent between turns and releases the old
// one, for a host that rebinds a session to another model or definition.
func (s *Session[H]) Replace(a Agent) error {
	s.mu.Lock()
	if s.closed || s.running {
		s.mu.Unlock()
		if a.Release != nil {
			a.Release()
		}
		if s.closed {
			return ErrClosed
		}
		return ErrBusy
	}
	old := s.agent
	s.agent = a
	s.mu.Unlock()
	if old.Release != nil {
		old.Release()
	}
	return nil
}

// CheckDecision validates a decision before it is delivered: its grant must
// be well formed and within the definition's cap.
func (s *Session[H]) CheckDecision(r agentsdk.Resolution, now time.Time) error {
	if err := ValidateGrant(r.Grant, r.Approved, now); err != nil {
		return err
	}
	if check := s.Agent().CheckGrant; check != nil {
		return check(r.Grant)
	}
	return nil
}

// Decide checks a decision and delivers it to the running turn's marker
// for toolCallID. The errors of EventStream.ResolveMarkerErr pass through.
func (s *Session[H]) Decide(toolCallID string, r agentsdk.Resolution) error {
	if err := s.CheckDecision(r, time.Now()); err != nil {
		return err
	}
	stream := s.Stream()
	if stream == nil {
		return ErrNoTurn
	}
	return stream.ResolveMarkerErr(toolCallID, r)
}

// IdleFor reports how long the session has had no running turn, or zero
// while a turn runs.
func (s *Session[H]) IdleFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return 0
	}
	return now.Sub(s.idleSince)
}

// Close cancels the running turn and releases the agent. It is safe to
// call more than once and returns nil.
func (s *Session[H]) Close(context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream, running, release := s.stream, s.running, s.agent.Release
	s.mu.Unlock()
	if running {
		stream.Cancel()
	}
	if release != nil {
		release()
	}
	return nil
}

// Config configures a Manager.
type Config struct {
	// Max caps live sessions; 0 means 64.
	Max int
	// IdleTTL drops a session that had no running turn for this long; 0
	// means an hour.
	IdleTTL time.Duration
	// Prefix starts every session ID, such as "s_".
	Prefix string
	// ArtifactBudget caps the bytes each session's artifacts hold; 0
	// means DefaultArtifactBudget.
	ArtifactBudget int64
}

// Manager holds a host's sessions.
type Manager[H any] struct {
	opts     Config
	mu       sync.Mutex
	sessions map[string]*Session[H]
	// reserved counts slots held by creates still building their agent,
	// so concurrent creates cannot pass the cap.
	reserved int
}

// Option adjusts a Config before New validates it.
type Option func(*Config)

// WithMax sets Config.Max.
func WithMax(n int) Option { return func(c *Config) { c.Max = n } }

// New returns an empty manager. A negative Max, IdleTTL or ArtifactBudget
// is an error wrapping types.ErrInvalidConfig.
func New[H any](cfg Config, opts ...Option) (*Manager[H], error) {
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Max < 0 || cfg.IdleTTL < 0 || cfg.ArtifactBudget < 0 {
		return nil, fmt.Errorf("%w: agenthost: negative Max, IdleTTL or ArtifactBudget", types.ErrInvalidConfig)
	}
	if cfg.Max == 0 {
		cfg.Max = 64
	}
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = time.Hour
	}
	return &Manager[H]{opts: cfg, sessions: map[string]*Session[H]{}}, nil
}

// Create builds an agent with build and adds a session for it. id names the
// session; empty picks a random one. The slot is reserved before build
// runs, so the cap holds however many creates run at once.
func (m *Manager[H]) Create(id string, host H, build func() (Agent, error)) (*Session[H], error) {
	m.mu.Lock()
	if len(m.sessions)+m.reserved >= m.opts.Max {
		m.mu.Unlock()
		return nil, ErrLimit
	}
	if id != "" && m.sessions[id] != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("session %s already exists", id)
	}
	m.reserved++
	m.mu.Unlock()

	a, err := build()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved--
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = m.opts.Prefix + RandomID()
	}
	if m.sessions[id] != nil {
		if a.Release != nil {
			a.Release()
		}
		return nil, fmt.Errorf("session %s already exists", id)
	}
	s := &Session[H]{ID: id, Host: host, Artifacts: NewArtifacts(m.opts.ArtifactBudget), agent: a, idleSince: time.Now()}
	m.sessions[id] = s
	return s, nil
}

// Get returns the session with id, or nil.
func (m *Manager[H]) Get(id string) *Session[H] {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// List returns every session, ordered by ID.
func (m *Manager[H]) List() []*Session[H] {
	m.mu.Lock()
	out := make([]*Session[H], 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Remove drops the session with id and closes it. It returns nil when no
// session has that id.
func (m *Manager[H]) Remove(id string) *Session[H] {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		_ = s.Close(context.Background())
	}
	return s
}

// Evict drops and closes every session that has been idle for at least the
// idle TTL at now. A running turn keeps its session however long it takes.
func (m *Manager[H]) Evict(now time.Time) []*Session[H] {
	m.mu.Lock()
	var dropped []*Session[H]
	for id, s := range m.sessions {
		if s.IdleFor(now) >= m.opts.IdleTTL {
			delete(m.sessions, id)
			dropped = append(dropped, s)
		}
	}
	m.mu.Unlock()
	for _, s := range dropped {
		_ = s.Close(context.Background())
	}
	return dropped
}

// Sweep evicts idle sessions until ctx ends, then closes every session.
func (m *Manager[H]) Sweep(ctx context.Context) {
	tick := time.NewTicker(min(m.opts.IdleTTL/4, time.Minute))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.CloseAll()
			return
		case now := <-tick.C:
			m.Evict(now)
		}
	}
}

// CloseAll drops and closes every session.
func (m *Manager[H]) CloseAll() {
	m.mu.Lock()
	all := m.sessions
	m.sessions = map[string]*Session[H]{}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.Close(context.Background())
	}
}

// RandomID returns 24 random hex characters.
func RandomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
