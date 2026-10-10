package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unsafe"

	"golang.org/x/sync/singleflight"

	"github.com/urmzd/saige/agent/types"
)

// Pool holds connections to several MCP servers and closes them together.
//
// It exists because the lifetime problem is per-deployment, not per-server: an
// agent wired to four servers has four child processes and sessions to unwind,
// and a partial shutdown leaves orphaned processes behind. Close unwinds all of
// them and reports every failure rather than stopping at the first.
//
// Connections are keyed by ServerSpec.Identity, so agents that share a Pool
// share one session per server instead of repeating the handshake.
type Pool struct {
	mu      sync.RWMutex
	clients []*Client
	byKey   map[string]*Client // spec identity -> pooled client
	keyOf   map[*Client]string
	byTool  map[string]*Client // exposed tool name -> owning client
	// retired holds exposed names the server stopped offering that are still
	// in a registry which cannot remove them. They stay in byTool so Gate keeps
	// applying the owning server's policy to them.
	retired map[string]bool
	group   singleflight.Group
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{byKey: map[string]*Client{}, keyOf: map[*Client]string{}, byTool: map[string]*Client{}}
}

// initLocked makes a zero Pool usable. Callers hold p.mu.
func (p *Pool) initLocked() {
	if p.byKey == nil {
		p.byKey = map[string]*Client{}
	}
	if p.keyOf == nil {
		p.keyOf = map[*Client]string{}
	}
	if p.byTool == nil {
		p.byTool = map[string]*Client{}
	}
	if p.retired == nil {
		p.retired = map[string]bool{}
	}
}

// Identity returns a stable key for the connection a spec describes: SHA-256
// over the transport fields, the header names with a hash of their values,
// the allowlist and the prefix. Two specs with the same identity share one
// session in a Pool.
//
// The allowlist is part of the key on purpose: two agents restricted to
// different tool subsets must never share a session and its tool list.
//
// Policy fields (Gate, credentials, HTTPClient, retries, limits, timeouts and
// hooks) are not part of the key. A Pool refuses to share a client between two
// specs with the same identity whose policy fields differ, because the second
// spec's policy would otherwise be silently dropped; give each a distinct Name
// or use a separate Pool.
func (s ServerSpec) Identity() string {
	headers := make([]string, 0, len(s.Headers))
	for k, v := range s.Headers {
		sum := sha256.Sum256([]byte(v))
		headers = append(headers, k+"="+hex.EncodeToString(sum[:8]))
	}
	sort.Strings(headers)
	allowed := append([]string(nil), s.AllowedTools...)
	sort.Strings(allowed)
	raw, _ := json.Marshal(struct {
		Name, Command, WorkDir, URL, Prefix string
		Args, Env, Headers, Allowed         []string
		TrustHints                          bool
	}{s.Name, s.Command, s.WorkDir, s.URL, s.prefix(), s.Args, s.Env, headers, allowed, s.TrustHints})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Acquire returns the pooled client for spec, connecting first when there is
// none. Concurrent callers with the same identity wait on one handshake. The
// handshake is bounded by the spec's ConnectTimeout rather than by any one
// caller's context, so a caller that gives up does not fail the others.
func (p *Pool) Acquire(ctx context.Context, spec ServerSpec) (*Client, error) {
	c, _, err := p.acquire(ctx, spec)
	return c, err
}

// acquire reports whether this call created the connection.
func (p *Pool) acquire(ctx context.Context, spec ServerSpec) (*Client, bool, error) {
	if err := spec.Validate(); err != nil {
		return nil, false, err
	}
	key := spec.Identity()
	p.mu.RLock()
	c := p.byKey[key]
	p.mu.RUnlock()
	if c != nil {
		if !c.isClosed() {
			if err := samePolicy(c.spec, spec); err != nil {
				return nil, false, err
			}
			return c, false, nil
		}
		// Closed outside the pool: drop it so this call reconnects.
		_ = p.Evict(c)
	}

	// opened is set only by this call's own closure, and only when it dials.
	// A caller that joined another caller's flight, or whose flight found a
	// client pooled in the meantime, did not create the connection. The
	// channel receive below orders the write before the read.
	opened := false
	ch := p.group.DoChan(key, func() (any, error) {
		p.mu.RLock()
		existing := p.byKey[key]
		p.mu.RUnlock()
		if existing != nil && !existing.isClosed() {
			return existing, nil
		}
		c, err := Connect(context.WithoutCancel(ctx), spec)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.initLocked()
		p.byKey[key] = c
		p.keyOf[c] = key
		p.clients = append(p.clients, c)
		p.mu.Unlock()
		opened = true
		return c, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, false, res.Err
		}
		c := res.Val.(*Client)
		// A caller that joined another caller's handshake may carry a
		// different policy under the same identity.
		if err := samePolicy(c.spec, spec); err != nil {
			return nil, false, err
		}
		return c, opened, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// Add connects to a server and keeps the client, or returns the pooled client
// when one with the same identity exists. On failure nothing is added, so a
// pool never holds a half-open connection.
func (p *Pool) Add(ctx context.Context, spec ServerSpec) (*Client, error) {
	return p.Acquire(ctx, spec)
}

// AddAll connects to every spec. On the first failure it closes the clients
// this call opened and returns the error: a partially-connected pool would let
// an agent start with a silently incomplete tool set, which surfaces later as
// the model claiming a capability it does not have. Clients that were already
// pooled before the call are left alone.
func (p *Pool) AddAll(ctx context.Context, specs ...ServerSpec) error {
	var opened []*Client
	for _, s := range specs {
		c, created, err := p.acquire(ctx, s)
		if err != nil {
			for _, o := range opened {
				_ = p.Evict(o)
			}
			return err
		}
		if created {
			opened = append(opened, c)
		}
	}
	return nil
}

// Evict removes a client from the pool and closes it, but only while it is
// still the pooled client for its identity; evicting a client that was
// already replaced or removed does nothing. The next Acquire reconnects.
func (p *Pool) Evict(c *Client) error {
	p.mu.Lock()
	key, ok := p.keyOf[c]
	if !ok || p.byKey[key] != c {
		p.mu.Unlock()
		return nil
	}
	delete(p.byKey, key)
	delete(p.keyOf, c)
	for i, x := range p.clients {
		if x == c {
			p.clients = append(p.clients[:i:i], p.clients[i+1:]...)
			break
		}
	}
	for name, owner := range p.byTool {
		if owner == c {
			delete(p.byTool, name)
			delete(p.retired, name)
		}
	}
	p.mu.Unlock()
	if err := c.Close(context.Background()); err != nil {
		return fmt.Errorf("mcp: close %q: %w", c.spec.Name, err)
	}
	return nil
}

// Clients returns the connected clients.
func (p *Pool) Clients() []*Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Client, len(p.clients))
	copy(out, p.clients)
	return out
}

// RegisterAll imports every server's tools into a registry and returns the
// total count. It also records which server owns each exposed tool name, which
// is what Gate needs to route policy.
//
// A name collision is an error rather than a silent overwrite, whether the
// other tool comes from another server or is a local tool already in the
// registry: the model would call one tool and reach the other.
func (p *Pool) RegisterAll(ctx context.Context, registry *types.ToolRegistry) (int, error) {
	total := 0
	for _, c := range p.Clients() {
		tools, err := c.Tools(ctx)
		if err != nil {
			return total, err
		}
		for _, t := range tools {
			name := t.Definition().Name
			known := p.owns(name, c)
			if err := p.claim(name, c); err != nil {
				return total, err
			}
			if err := registerChecked(registry, t, c); err != nil {
				if !known {
					p.release(name, c)
				}
				return total, err
			}
			total++
		}
	}
	return total, nil
}

// ToolChanges reports what Refresh changed in a registry.
type ToolChanges struct {
	Added   []string
	Removed []string
}

// Refresh re-lists every server's tools and brings a registry up to date: new
// tools are registered under the same collision rules as RegisterAll, and
// tools a server stopped offering are removed from the registry when it
// supports removal. A removed tool that stays registered keeps its route to
// the owning server, so Gate still applies that server's policy to it, and a
// call to it returns an error result instead of reaching the server until a
// later listing offers it again.
//
// Call it from ServerSpec.OnToolsChanged, or before each run.
func (p *Pool) Refresh(ctx context.Context, registry *types.ToolRegistry) (ToolChanges, error) {
	var changes ToolChanges
	for _, c := range p.Clients() {
		c.InvalidateCatalog()
		tools, err := c.Tools(ctx)
		if err != nil {
			return changes, err
		}
		current := make(map[string]bool, len(tools))
		for _, t := range tools {
			current[t.Definition().Name] = true
		}

		p.mu.RLock()
		var removed []string
		for name, owner := range p.byTool {
			if owner == c && !current[name] && !p.retired[name] {
				removed = append(removed, name)
			}
		}
		p.mu.RUnlock()
		sort.Strings(removed)
		for _, name := range removed {
			gone := unregisterOwned(registry, name, c)
			p.mu.Lock()
			if p.byTool[name] == c {
				if gone {
					delete(p.byTool, name)
				} else {
					p.initLocked()
					p.retired[name] = true
				}
			}
			p.mu.Unlock()
		}
		changes.Removed = append(changes.Removed, removed...)

		for _, t := range tools {
			name := t.Definition().Name
			p.mu.RLock()
			known := p.byTool[name] == c && !p.retired[name]
			p.mu.RUnlock()
			if err := p.claim(name, c); err != nil {
				return changes, err
			}
			if err := registerChecked(registry, t, c); err != nil {
				if !known {
					p.release(name, c)
				}
				return changes, err
			}
			if !known {
				changes.Added = append(changes.Added, name)
			}
		}
	}
	return changes, nil
}

func (p *Pool) owns(name string, c *Client) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byTool[name] == c
}

// claim records c as the owner of an exposed tool name, refusing a name
// another server already owns.
func (p *Pool) claim(name string, c *Client) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	if owner, taken := p.byTool[name]; taken && owner != c {
		return fmt.Errorf("mcp: tool name %q is exposed by both %q and %q; set ToolPrefix to disambiguate",
			name, owner.spec.Name, c.spec.Name)
	}
	p.byTool[name] = c
	delete(p.retired, name)
	return nil
}

func (p *Pool) release(name string, c *Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byTool[name] == c {
		delete(p.byTool, name)
		delete(p.retired, name)
	}
}

// registerChecked adds t to the registry unless the name is held by a tool
// owner did not produce. Re-registering owner's own tool replaces it, which is
// how a refreshed schema takes effect.
func registerChecked(registry *types.ToolRegistry, t types.Tool, owner *Client) error {
	name := t.Definition().Name
	if existing, ok := registry.Get(name); ok {
		if ownedBy(existing, owner) {
			registry.Register(t)
			return nil
		}
		return fmt.Errorf("mcp: server %q exposes tool %q, which is already registered by another source; set ToolPrefix to disambiguate",
			owner.spec.Name, name)
	}
	// Prefer an atomic insert when the registry offers one, which closes the
	// window between the lookup above and the write.
	if u, ok := any(registry).(interface{ RegisterUnique(types.Tool) error }); ok {
		if err := u.RegisterUnique(t); err != nil {
			return fmt.Errorf("mcp: server %q: %w", owner.spec.Name, err)
		}
		return nil
	}
	registry.Register(t)
	return nil
}

// unregisterOwned removes a tool from the registry when the registry supports
// removal and the registered tool still belongs to owner. It reports whether
// the name no longer reaches owner's tool: true when it was removed or is held
// by something else, false when owner's tool is still registered.
func unregisterOwned(registry *types.ToolRegistry, name string, owner *Client) bool {
	existing, ok := registry.Get(name)
	if !ok || !ownedBy(existing, owner) {
		return true
	}
	u, ok := any(registry).(interface{ Unregister(string) bool })
	if !ok {
		return false
	}
	return u.Unregister(name)
}

// ownedBy reports whether a registered tool is one of owner's server tools,
// looking through approval markers.
func ownedBy(t types.Tool, owner *Client) bool {
	for {
		switch v := t.(type) {
		case *serverTool:
			return v.client == owner
		case *types.MarkedTool:
			t = v.Inner
		default:
			return false
		}
	}
}

// Gate returns a types.ToolGate that applies each server's own Gate to that
// server's tools, and allows everything else through.
//
// Compose it with the deployment's own policy:
//
//	agent.WithToolGate(types.Gates(pool.Gate(), myPolicy))
//
// Per-server policy is the useful unit because trust is per-server: a local
// server you wrote needs no gate, while a remote third-party one should
// probably have every write-shaped tool behind approval, and both can be
// registered into the same agent.
//
// Tool annotations are advisory. CapabilityGate can use them, but they never
// replace a per-server gate for a server you do not control.
func (p *Pool) Gate() types.ToolGate {
	return types.GateFunc(func(ctx context.Context, def types.ToolDef, args map[string]any) types.GateDecision {
		p.mu.RLock()
		c := p.byTool[def.Name]
		p.mu.RUnlock()
		if c == nil || c.spec.Gate == nil {
			return types.Allow()
		}
		return c.spec.Gate.Check(ctx, def, args)
	})
}

// Owner returns the client that exposed a tool name, if any. Useful for
// attributing a failure to the server that caused it.
func (p *Pool) Owner(toolName string) (*Client, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.byTool[toolName]
	return c, ok
}

// Health pings every pooled server and returns each failure by server name.
// An empty map means every server answered.
func (p *Pool) Health(ctx context.Context) map[string]error {
	out := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range p.Clients() {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			if err := c.Ping(ctx); err != nil {
				mu.Lock()
				out[c.spec.Name] = err
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	return out
}

// Preflight returns an error naming every server that did not answer a ping,
// for a check that must pass before a run spends anything.
func (p *Pool) Preflight(ctx context.Context) error {
	failures := p.Health(ctx)
	names := make([]string, 0, len(failures))
	for n := range failures {
		names = append(names, n)
	}
	sort.Strings(names)
	errs := make([]error, 0, len(names))
	for _, n := range names {
		errs = append(errs, failures[n])
	}
	return errors.Join(errs...)
}

// Close closes every client, joining the failures. Local servers are child
// processes, so this is required for cleanup, not merely polite.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	clients := p.clients
	p.clients = nil
	p.byKey = map[string]*Client{}
	p.keyOf = map[*Client]string{}
	p.byTool = map[string]*Client{}
	p.retired = map[string]bool{}
	p.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("mcp: close %q: %w", c.spec.Name, err))
		}
	}
	return errors.Join(errs...)
}

// samePolicy reports an error when two specs with one identity differ in a
// field the identity leaves out. Sharing a client between them would apply
// the first spec's gate, credentials and limits to the second spec's calls.
//
// Values with identity (gates, functions, clients, handlers) are compared by
// instance: the same closure or pointer matches, while two closures built
// from one function literal with different captured state do not.
func samePolicy(pooled, next ServerSpec) error {
	checks := []struct {
		field string
		same  bool
	}{
		{"Gate", sameInstance(pooled.Gate, next.Gate)},
		{"HTTPClient", pooled.HTTPClient == next.HTTPClient},
		{"TokenFunc", sameInstance(pooled.TokenFunc, next.TokenFunc)},
		{"OAuthHandler", sameInstance(pooled.OAuthHandler, next.OAuthHandler)},
		{"Retry", sameRetry(pooled.Retry, next.Retry)},
		{"ConnectRetry", sameRetry(pooled.ConnectRetry, next.ConnectRetry)},
		{"MaxReconnects", pooled.MaxReconnects == next.MaxReconnects},
		{"IdempotentTools", sameSet(pooled.IdempotentTools, next.IdempotentTools)},
		{"ConnectTimeout", pooled.ConnectTimeout == next.ConnectTimeout},
		{"CallTimeout", pooled.CallTimeout == next.CallTimeout},
		{"KeepAlive", pooled.KeepAlive == next.KeepAlive},
		{"MaxConcurrent", pooled.MaxConcurrent == next.MaxConcurrent},
		{"MaxResultBytes", pooled.MaxResultBytes == next.MaxResultBytes},
		{"MaxBinaryBytes", pooled.MaxBinaryBytes == next.MaxBinaryBytes},
		{"ArgTransform", sameInstance(pooled.ArgTransform, next.ArgTransform)},
		{"OnToolsChanged", sameInstance(pooled.OnToolsChanged, next.OnToolsChanged)},
		{"OnProgress", sameInstance(pooled.OnProgress, next.OnProgress)},
	}
	for _, c := range checks {
		if !c.same {
			return fmt.Errorf("mcp: server %q is already pooled with a different %s; give this spec a distinct Name or use a separate Pool",
				next.Name, c.field)
		}
	}
	return nil
}

// sameInstance reports whether a and b hold the same value instance. It
// compares the two words of each interface: the dynamic type and the data
// word. For pointer-shaped values (pointers, funcs, maps, channels) the data
// word is the value itself, so a copied spec matches and a different closure
// does not. Other values are boxed, so only a copy of the same boxed
// interface matches, which errs on the side of not sharing.
func sameInstance(a, b any) bool {
	type eface struct{ typ, data unsafe.Pointer }
	ea := (*eface)(unsafe.Pointer(&a)) //nolint:gosec // reads the interface header only; see above
	eb := (*eface)(unsafe.Pointer(&b)) //nolint:gosec // reads the interface header only; see above
	return ea.typ == eb.typ && ea.data == eb.data
}

func sameRetry(a, b *RetryPolicy) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.MaxAttempts == b.MaxAttempts && a.InitialBackoff == b.InitialBackoff &&
		a.MaxBackoff == b.MaxBackoff && a.MaxTotalDelay == b.MaxTotalDelay &&
		sameInstance(a.OnRetry, b.OnRetry)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
