// Package isolate is the isolation.Leaser over sandbox's pkg/isolate: a
// Pool of instances started by a sandbox Driver (docker with runsc or
// runc, or local). It converts values and nothing more; what an instance
// is, how it is confined and how instances are kept warm are sandbox's.
//
// With Config.Egress, every instance's only way out is sandbox's egress
// gate (isolate/egress). Each lease gets a grant of its own for the hosts
// its request names, default deny, and every command it runs gets the
// gate's proxy variables. Releasing the lease revokes the grant.
//
// Every lease is for one run and is destroyed when the run releases it. The
// spec a lease asks for is the same for every run with the same needs, so
// the pool's warm instances serve them all: a run's identity is never
// written into the spec (that would make every run a spec of its own, and
// the pool would warm one instance per run).
package isolate

import (
	"context"
	"errors"
	"fmt"

	"maps"
	"strings"
	"sync"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/isolate/egress"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/isolation"
)

// Config is what every instance a Leaser leases is started with.
type Config struct {
	// Image is the workload's root filesystem. Required.
	Image string
	// ConnectorsImage is the connectors compartment's; empty is Image.
	ConnectorsImage string
	// Resources bound the workload, and ConnectorResources the connectors
	// compartment. Zero fields are sandbox's small defaults.
	Resources          isolate.Resources
	ConnectorResources isolate.Resources
	// Labels mark every instance for the operator. Keys may not start
	// with "sandbox.".
	Labels map[string]string
	// Pool configures the pool: how many instances are kept warm per
	// spec, and how many may exist at once.
	Pool isolate.PoolConfig
	// Egress, when set, sends both compartments out through an egress
	// gate. Nil is no network at all.
	Egress *Egress
}

// Egress is the gate a Leaser's instances reach the outside through.
type Egress struct {
	// Gate grants each lease its hosts. *egress.Gate is one.
	Gate Gate
	// Address is the gate as "host:port", as an instance reaches it.
	Address string
}

// Gate is what a Leaser needs of sandbox's egress gate.
type Gate interface {
	Grant(id string, p egress.Policy) (token string, err error)
	Revoke(id string)
}

var _ Gate = (*egress.Gate)(nil)

// Leaser leases instances from a sandbox pool.
type Leaser struct {
	pool *isolate.Pool
	cfg  Config

	grants sync.Map // grant id to the isolation.Request it was made for
}

var _ isolation.Leaser = (*Leaser)(nil)

// New returns a Leaser starting instances with d. It checks the spec it
// will lease with, so a bad image fails here and not on a run's first tool.
func New(d isolate.Driver, cfg Config) (*Leaser, error) {
	if d == nil {
		return nil, errors.New("isolation/isolate: no driver")
	}
	if e := cfg.Egress; e != nil && (e.Gate == nil || e.Address == "") {
		return nil, errors.New("isolation/isolate: egress needs a gate and its address")
	}
	l := &Leaser{cfg: cfg}
	for _, connectors := range []bool{false, true} {
		if err := l.spec(isolation.Request{Connectors: connectors}).Validate(); err != nil {
			return nil, fmt.Errorf("isolation/isolate: %w", err)
		}
	}
	l.pool = isolate.NewPool(d, cfg.Pool)
	return l, nil
}

// spec is the sandbox spec for a run's request.
func (l *Leaser) spec(req isolation.Request) isolate.Spec {
	s := isolate.Spec{
		Image:     l.cfg.Image,
		Resources: l.cfg.Resources,
		Labels:    l.cfg.Labels,
	}
	var network isolate.Network
	if l.cfg.Egress != nil {
		network.Proxy = l.cfg.Egress.Address
	}
	s.Network = network
	if req.Connectors {
		s.Connectors = &isolate.ConnectorSpec{Image: l.cfg.ConnectorsImage, Resources: l.cfg.ConnectorResources, Network: network}
	}
	return s
}

// Lease implements isolation.Leaser. With egress, the lease is granted the
// hosts req names; a lease whose grant fails is released, never handed out
// without it.
func (l *Leaser) Lease(ctx context.Context, req isolation.Request) (isolation.Instance, error) {
	lease, err := l.pool.Lease(ctx, l.spec(req))
	if err != nil {
		return nil, err
	}
	inst := &instance{lease: lease}
	if e := l.cfg.Egress; e != nil {
		id := lease.ID()
		token, err := e.Gate.Grant(id, Policy(req.Egress))
		if err != nil {
			_ = lease.Release(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("isolation/isolate: egress grant: %w", err)
		}
		l.grants.Store(id, req)
		inst.env = egress.ProxyEnv(e.Address, token)
		inst.revoke = func() {
			e.Gate.Revoke(id)
			l.grants.Delete(id)
		}
	}
	return inst, nil
}

// Granted returns the request the grant id (an egress decision's Grant)
// was made for, while its lease is out.
func (l *Leaser) Granted(id string) (isolation.Request, bool) {
	v, ok := l.grants.Load(id)
	if !ok {
		return isolation.Request{}, false
	}
	return v.(isolation.Request), true
}

// Policy is the egress policy for hosts: one rule per host, on the gate's
// default ports (80 and 443). No hosts is a policy that allows nothing.
func Policy(hosts []string) egress.Policy {
	p := egress.Policy{Allow: []egress.Rule{}}
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			p.Allow = append(p.Allow, egress.Rule{Host: h})
		}
	}
	return p
}

// Stats is a snapshot of the pool.
func (l *Leaser) Stats() isolate.PoolStats { return l.pool.Stats() }

// Close stops warming and destroys every warm instance. Leases still out
// are destroyed when their runs release them.
func (l *Leaser) Close(ctx context.Context) error { return l.pool.Close(ctx) }

// instance is a pool lease as an isolation.Instance.
type instance struct {
	lease  *isolate.Lease
	env    map[string]string // the gate's proxy variables, or nil
	revoke func()            // ends the lease's egress grant, or nil
	once   sync.Once
}

func (i *instance) ID() string                   { return i.lease.ID() }
func (i *instance) Files() workspace.Workspace   { return i.lease.Files() }
func (i *instance) Project() workspace.Workspace { return i.lease.Project() }
func (i *instance) Me() workspace.Workspace      { return i.lease.Me() }

// Close revokes the egress grant, then releases the lease.
func (i *instance) Close(ctx context.Context) error {
	if i.revoke != nil {
		i.once.Do(i.revoke)
	}
	return i.lease.Release(ctx)
}

func (i *instance) Exec(ctx context.Context, c isolation.Command, s isolation.Stdio) (isolation.ExitStatus, error) {
	in := isolate.Workload
	if c.In == isolation.Connectors {
		in = isolate.Connectors
	}
	env := c.Env
	if i.env != nil {
		// The gate's variables win over a command's own: a tool cannot
		// point its clients elsewhere by its environment (the network
		// would refuse it anyway).
		env = maps.Clone(c.Env)
		if env == nil {
			env = map[string]string{}
		}
		maps.Copy(env, i.env)
	}
	st, err := i.lease.Exec(ctx, isolate.Command{Argv: c.Argv, Env: env, Dir: c.Dir, Timeout: c.Timeout, In: in},
		isolate.Stdio{Stdin: s.Stdin, Stdout: s.Stdout, Stderr: s.Stderr})
	return isolation.ExitStatus{Code: st.Code}, err
}
