package isolate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Pool defaults applied when PoolConfig fields are zero.
const (
	DefaultPoolWarm = 1
	DefaultPoolMax  = 64
)

// Retry delays after a background start fails, doubling from the first to
// the last.
const (
	minRetry = 100 * time.Millisecond
	maxRetry = 30 * time.Second
)

// PoolConfig configures a Pool.
type PoolConfig struct {
	// Warm is how many started, unleased instances the pool keeps for
	// each spec it has seen; zero means DefaultPoolWarm.
	Warm int
	// Max bounds every instance the pool holds at once: warm, leased and
	// starting. Zero means DefaultPoolMax. Lease waits while it is
	// reached and nothing warm can be given up.
	Max int
	// OnError receives the errors of background work: a failed warm
	// start, or a failed Close of an evicted instance. Nil drops them.
	// It must not block or call the pool.
	OnError func(error)
}

// Pool keeps instances warm per spec and leases each one to exactly one
// run. A lease is never reused: Release destroys the instance, and the pool
// starts a fresh one in the background. Pool is safe for concurrent use.
//
// The pool warms the specs it has been asked for, with Lease or Warm. When
// Max is reached, a lease for a spec with no warm instance destroys a warm
// instance of another spec to make room.
type Pool struct {
	driver Driver
	warm   int
	max    int
	onErr  func(error)

	ctx    context.Context // background starts; cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	live    int // warm, leased, starting and closing instances
	waiting int // leases waiting for room; refills yield to them
	leased  int
	specs   map[string]*poolSpec
	wake    chan struct{} // closed and replaced whenever capacity may have freed
}

type poolSpec struct {
	spec     Spec
	warm     []Instance
	starting int
	retry    time.Duration // delay before the next start after a failure
	retryAt  time.Time
	timer    *time.Timer
}

// NewPool returns a pool that starts its instances with d.
func NewPool(d Driver, cfg PoolConfig) *Pool {
	if cfg.Warm <= 0 {
		cfg.Warm = DefaultPoolWarm
	}
	if cfg.Max <= 0 {
		cfg.Max = DefaultPoolMax
	}
	if cfg.OnError == nil {
		cfg.OnError = func(error) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		driver: d, warm: cfg.Warm, max: cfg.Max, onErr: cfg.OnError,
		ctx: ctx, cancel: cancel,
		specs: make(map[string]*poolSpec),
		wake:  make(chan struct{}),
	}
}

// PoolStats is a snapshot of a pool's instances.
type PoolStats struct {
	// Live counts every instance the pool holds: warm, leased, starting.
	Live int
	// Warm counts started instances waiting for a lease.
	Warm int
	// Leased counts instances leased and not yet released.
	Leased int
	// Specs counts the distinct specs the pool warms.
	Specs int
}

// Stats returns a snapshot of the pool.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := PoolStats{Live: p.live, Leased: p.leased, Specs: len(p.specs)}
	for _, ps := range p.specs {
		s.Warm += len(ps.warm)
	}
	return s
}

// Warm validates spec and starts keeping instances of it warm, without
// leasing one. It returns at once; the instances start in the background.
func (p *Pool) Warm(spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.fillLocked(p.specLocked(spec))
	return nil
}

// Lease returns an instance of spec for one run: a warm one when there is
// one, otherwise one started now. It waits while the pool is full and ctx
// allows. The caller must Release the lease; the instance is destroyed
// then and never leased again.
func (p *Pool) Lease(ctx context.Context, spec Spec) (*Lease, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		ps := p.specLocked(spec)
		if n := len(ps.warm); n > 0 {
			inst := ps.warm[n-1]
			ps.warm = ps.warm[:n-1]
			p.leased++
			p.fillLocked(ps)
			p.mu.Unlock()
			return &Lease{Instance: inst, pool: p}, nil
		}
		if p.live >= p.max {
			// Room frees once the evicted instance is gone; wait
			// for it below like any other release.
			p.evictLocked()
		}
		if p.live < p.max {
			p.live++
			p.leased++
			p.mu.Unlock()
			inst, err := p.driver.Start(ctx, spec)
			p.mu.Lock()
			if err != nil {
				p.live--
				p.leased--
				p.notifyLocked()
				p.mu.Unlock()
				return nil, err
			}
			p.fillLocked(ps)
			p.mu.Unlock()
			return &Lease{Instance: inst, pool: p}, nil
		}
		wake := p.wake
		p.waiting++
		p.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
		}
		p.mu.Lock()
		p.waiting--
		if ctx.Err() != nil {
			if p.waiting == 0 {
				p.fillAllLocked() // what waiting held back
			}
			p.mu.Unlock()
			return nil, fmt.Errorf("isolate: lease: waiting for room in the pool: %w", ctx.Err())
		}
		p.mu.Unlock()
	}
}

// specLocked returns the pool's entry for spec, adding it on first sight.
func (p *Pool) specLocked(spec Spec) *poolSpec {
	key := spec.Key()
	ps, ok := p.specs[key]
	if !ok {
		ps = &poolSpec{spec: spec}
		p.specs[key] = ps
	}
	return ps
}

// evictLocked destroys one warm instance, of any spec, to free a slot.
func (p *Pool) evictLocked() {
	for _, ps := range p.specs {
		if n := len(ps.warm); n > 0 {
			inst := ps.warm[0]
			ps.warm = ps.warm[1:]
			p.destroy(inst)
			return
		}
	}
}

// destroy closes inst in the background, reporting a failure to OnError.
// The instance holds its slot until it is gone, so Max bounds instances
// being destroyed too.
func (p *Pool) destroy(inst Instance) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := inst.Close(ctx); err != nil {
			p.onErr(fmt.Errorf("isolate: pool: close %s: %w", inst.ID(), err))
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.live--
		p.notifyLocked()
		p.fillAllLocked()
	}()
}

// fillLocked starts instances of ps in the background until it has Warm of
// them, warm or starting, as far as Max allows. A lease waiting for room
// comes first: nothing is warmed while one waits.
func (p *Pool) fillLocked(ps *poolSpec) {
	if p.closed || p.waiting > 0 || time.Now().Before(ps.retryAt) {
		return
	}
	for len(ps.warm)+ps.starting < p.warm && p.live < p.max {
		ps.starting++
		p.live++
		p.wg.Add(1)
		go p.start(ps)
	}
}

// fillAllLocked tops up every spec, after capacity frees.
func (p *Pool) fillAllLocked() {
	for _, ps := range p.specs {
		p.fillLocked(ps)
	}
}

func (p *Pool) start(ps *poolSpec) {
	defer p.wg.Done()
	inst, err := p.driver.Start(p.ctx, ps.spec)
	p.mu.Lock()
	defer p.mu.Unlock()
	ps.starting--
	switch {
	case err != nil:
		p.live--
		p.notifyLocked()
		if p.closed {
			return
		}
		p.onErr(fmt.Errorf("isolate: pool: warm start: %w", err))
		ps.retry = min(max(2*ps.retry, minRetry), maxRetry)
		ps.retryAt = time.Now().Add(ps.retry)
		if ps.timer == nil {
			ps.timer = time.AfterFunc(ps.retry, func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				ps.timer = nil
				p.fillLocked(ps)
			})
		}
	case p.closed:
		p.destroy(inst)
	default:
		ps.retry, ps.retryAt = 0, time.Time{}
		ps.warm = append(ps.warm, inst)
		p.notifyLocked()
	}
}

// notifyLocked wakes every Lease waiting for room.
func (p *Pool) notifyLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

// release frees a leased instance's slot after it has been destroyed.
func (p *Pool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live--
	p.leased--
	p.notifyLocked()
	p.fillAllLocked()
}

// Close stops warming, destroys every warm instance, and waits for
// background starts and closes to finish. Leases still out stay valid and
// are destroyed when released. Lease and Warm fail with ErrClosed after.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.cancel()
	var warm []Instance
	for _, ps := range p.specs {
		warm = append(warm, ps.warm...)
		p.live -= len(ps.warm)
		ps.warm = nil
		if ps.timer != nil {
			ps.timer.Stop()
		}
	}
	p.notifyLocked()
	p.mu.Unlock()

	var errs []error
	for _, inst := range warm {
		if err := inst.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("isolate: pool: close %s: %w", inst.ID(), err))
		}
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("isolate: pool: waiting for background work: %w", ctx.Err()))
	}
	return errors.Join(errs...)
}

// Lease is an instance leased from a Pool to one run. Close and Release
// are the same: both destroy the instance and return its slot to the pool.
type Lease struct {
	Instance
	pool *Pool
	once sync.Once
	err  error
}

var _ Instance = (*Lease)(nil)

// Release destroys the instance and frees its slot. Calling it again
// returns the first result.
func (l *Lease) Release(ctx context.Context) error {
	l.once.Do(func() {
		l.err = l.Instance.Close(ctx)
		l.pool.release()
	})
	return l.err
}

// Close implements Instance; it is Release.
func (l *Lease) Close(ctx context.Context) error { return l.Release(ctx) }
