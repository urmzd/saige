package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// Pool holds connections to several MCP servers and closes them together.
//
// It exists because the lifetime problem is per-deployment, not per-server: an
// agent wired to four servers has four child processes and sessions to unwind,
// and a partial shutdown leaves orphaned processes behind. Close unwinds all of
// them and reports every failure rather than stopping at the first.
type Pool struct {
	mu      sync.RWMutex
	clients []*Client
	byTool  map[string]*Client // exposed tool name -> owning client
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{byTool: map[string]*Client{}}
}

// Add connects to a server and keeps the client. On failure nothing is added,
// so a pool never holds a half-open connection.
func (p *Pool) Add(ctx context.Context, spec ServerSpec) (*Client, error) {
	c, err := Connect(ctx, spec)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.clients = append(p.clients, c)
	p.mu.Unlock()
	return c, nil
}

// AddAll connects to every spec. On the first failure it closes whatever it
// already opened and returns the error: a partially-connected pool would let an
// agent start with a silently incomplete tool set, which surfaces later as the
// model claiming a capability it does not have.
func (p *Pool) AddAll(ctx context.Context, specs ...ServerSpec) error {
	for _, s := range specs {
		if _, err := p.Add(ctx, s); err != nil {
			_ = p.Close()
			return err
		}
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
// A name collision between two servers is an error rather than a silent
// overwrite: the model would call one server's tool and reach the other's.
func (p *Pool) RegisterAll(ctx context.Context, registry *types.ToolRegistry) (int, error) {
	total := 0
	for _, c := range p.Clients() {
		tools, err := c.Tools(ctx)
		if err != nil {
			return total, err
		}
		for _, t := range tools {
			name := t.Definition().Name
			p.mu.Lock()
			if owner, taken := p.byTool[name]; taken && owner != c {
				p.mu.Unlock()
				return total, fmt.Errorf("mcp: tool name %q is exposed by both %q and %q; set ToolPrefix to disambiguate",
					name, owner.spec.Name, c.spec.Name)
			}
			p.byTool[name] = c
			p.mu.Unlock()
			registry.Register(t)
			total++
		}
	}
	return total, nil
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

// Close closes every client, joining the failures. Local servers are child
// processes, so this is required for cleanup, not merely polite.
func (p *Pool) Close() error {
	p.mu.Lock()
	clients := p.clients
	p.clients = nil
	p.byTool = map[string]*Client{}
	p.mu.Unlock()

	var errs []error
	for _, c := range clients {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("mcp: close %q: %w", c.spec.Name, err))
		}
	}
	return errors.Join(errs...)
}
