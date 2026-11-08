package isolate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/workspace"
)

// fakeDriver starts fakeInstances and keeps count of what is alive.
type fakeDriver struct {
	delay time.Duration
	fail  atomic.Int32 // the next n starts fail

	mu      sync.Mutex
	n       int
	alive   int
	maxSeen int
	all     []*fakeInstance
}

func (d *fakeDriver) Start(ctx context.Context, spec Spec) (Instance, error) {
	d.mu.Lock()
	d.alive++
	d.maxSeen = max(d.maxSeen, d.alive)
	d.mu.Unlock()
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
		}
	}
	if d.fail.Add(-1) >= 0 || ctx.Err() != nil {
		d.mu.Lock()
		d.alive--
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: fake", ErrStart)
	}
	d.fail.Store(0)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	inst := &fakeInstance{d: d, id: "f" + strconv.Itoa(d.n), image: spec.Image}
	d.all = append(d.all, inst)
	return inst, nil
}

func (d *fakeDriver) stats() (alive, maxSeen int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alive, d.maxSeen
}

type fakeInstance struct {
	d      *fakeDriver
	id     string
	image  string
	leased atomic.Int32
	closes atomic.Int32
}

func (i *fakeInstance) ID() string                   { return i.id }
func (i *fakeInstance) Files() workspace.Workspace   { return nil }
func (i *fakeInstance) Project() workspace.Workspace { return nil }
func (i *fakeInstance) Me() workspace.Workspace      { return nil }
func (i *fakeInstance) Exec(context.Context, Command, Stdio) (ExitStatus, error) {
	if i.closes.Load() > 0 {
		return ExitStatus{}, ErrClosed
	}
	return ExitStatus{}, nil
}

func (i *fakeInstance) Close(context.Context) error {
	if i.closes.Add(1) == 1 {
		i.d.mu.Lock()
		i.d.alive--
		i.d.mu.Unlock()
	}
	return nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func closePool(t *testing.T, p *Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPoolLeasesWarmInstancesAndRefills(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 2, Max: 10})
	defer closePool(t, p)
	spec := Spec{Image: "img"}
	if err := p.Warm(spec); err != nil {
		t.Fatal(err)
	}
	eventually(t, "two warm", func() bool { return p.Stats().Warm == 2 })
	l, err := p.Lease(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if id := l.ID(); id != "f1" && id != "f2" {
		t.Fatalf("leased %s, not a warm instance", id)
	}
	eventually(t, "the refill", func() bool { return p.Stats().Warm == 2 })
	if s := p.Stats(); s.Live != 3 || s.Leased != 1 || s.Specs != 1 {
		t.Fatalf("stats %+v", s)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPoolReleaseDestroysAndNeverReuses(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 1, Max: 4})
	defer closePool(t, p)
	spec := Spec{Image: "img"}
	ctx := context.Background()
	seen := map[string]bool{}
	for range 20 {
		l, err := p.Lease(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if seen[l.ID()] {
			t.Fatalf("%s leased twice", l.ID())
		}
		seen[l.ID()] = true
		inst := l.Instance.(*fakeInstance)
		if err := l.Close(ctx); err != nil { // Close is Release
			t.Fatal(err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
		if inst.closes.Load() != 1 {
			t.Fatalf("release closed the instance %d times", inst.closes.Load())
		}
		if _, err := l.Exec(ctx, Command{Argv: []string{"x"}}, Stdio{}); !errors.Is(err, ErrClosed) {
			t.Fatalf("exec after release: %v", err)
		}
	}
	if s := p.Stats(); s.Leased != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestPoolMaxBoundsLeasesAndWaits(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 1, Max: 2})
	defer closePool(t, p)
	spec := Spec{Image: "img"}
	ctx := context.Background()
	a, err := p.Lease(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Lease(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := p.Lease(short, spec); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a lease past Max: %v", err)
	}
	got := make(chan *Lease)
	go func() {
		l, err := p.Lease(ctx, spec)
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	time.Sleep(20 * time.Millisecond)
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c == nil {
		t.Fatal("no lease after a release")
	}
	if _, maxSeen := d.stats(); maxSeen > 2 {
		t.Fatalf("%d instances alive at once, Max is 2", maxSeen)
	}
	_ = b.Release(ctx)
	_ = c.Release(ctx)
}

func TestPoolEvictsAnotherSpecsWarmInstanceWhenFull(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 1, Max: 2})
	defer closePool(t, p)
	ctx := context.Background()
	if err := p.Warm(Spec{Image: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Warm(Spec{Image: "b"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a and b warm", func() bool { return p.Stats().Warm == 2 })
	l, err := p.Lease(ctx, Spec{Image: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Instance.(*fakeInstance).image != "c" {
		t.Fatal("leased the wrong image")
	}
	if s := p.Stats(); s.Live != 2 || s.Warm != 1 {
		t.Fatalf("stats %+v", s)
	}
	eventually(t, "the evicted instance closed", func() bool { alive, _ := d.stats(); return alive == 2 })
	_ = l.Release(ctx)
}

func TestPoolStartFailures(t *testing.T) {
	d := &fakeDriver{}
	d.fail.Store(3)
	var reported atomic.Int32
	p := NewPool(d, PoolConfig{Warm: 1, Max: 4, OnError: func(error) { reported.Add(1) }})
	defer closePool(t, p)
	spec := Spec{Image: "img"}
	if err := p.Warm(spec); err != nil {
		t.Fatal(err)
	}
	// The background start fails, is reported, and is retried with backoff
	// until it succeeds.
	eventually(t, "a warm instance after retries", func() bool { return p.Stats().Warm == 1 })
	if reported.Load() == 0 {
		t.Fatal("background failures were not reported")
	}
	if _, err := p.Lease(context.Background(), Spec{}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("invalid spec: %v", err)
	}
}

func TestPoolColdStartFailureReturnsTheSlot(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 1, Max: 1})
	defer closePool(t, p)
	d.fail.Store(1)
	if _, err := p.Lease(context.Background(), Spec{Image: "img"}); !errors.Is(err, ErrStart) {
		t.Fatalf("err = %v, want ErrStart", err)
	}
	eventually(t, "the slot back and refilled", func() bool { s := p.Stats(); return s.Leased == 0 && s.Live <= 1 })
	l, err := p.Lease(context.Background(), Spec{Image: "img"})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Release(context.Background())
}

func TestPoolClose(t *testing.T) {
	d := &fakeDriver{}
	p := NewPool(d, PoolConfig{Warm: 3, Max: 10})
	ctx := context.Background()
	spec := Spec{Image: "img"}
	l, err := p.Lease(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "three warm", func() bool { return p.Stats().Warm == 3 })
	closePool(t, p)
	if _, err := p.Lease(ctx, spec); !errors.Is(err, ErrClosed) {
		t.Fatalf("lease after close: %v", err)
	}
	if err := p.Warm(spec); !errors.Is(err, ErrClosed) {
		t.Fatalf("warm after close: %v", err)
	}
	if alive, _ := d.stats(); alive != 1 {
		t.Fatalf("%d alive after close, want only the lease", alive)
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if alive, _ := d.stats(); alive != 0 {
		t.Fatalf("%d alive after the last release", alive)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// Many runs leasing at once never share an instance, never exceed Max, and
// leave nothing alive once released and closed.
func TestPoolConcurrentLeases(t *testing.T) {
	d := &fakeDriver{delay: time.Millisecond}
	const maxLive = 8
	p := NewPool(d, PoolConfig{Warm: 2, Max: maxLive})
	specs := []Spec{{Image: "a"}, {Image: "b"}, {Image: "c"}}
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 25 {
				l, err := p.Lease(ctx, specs[(g+n)%len(specs)])
				if err != nil {
					t.Error(err)
					return
				}
				inst := l.Instance.(*fakeInstance)
				if inst.leased.Add(1) != 1 {
					t.Errorf("%s leased twice", inst.id)
				}
				if s := p.Stats(); s.Live > maxLive {
					t.Errorf("live %d > Max", s.Live)
				}
				if err := l.Release(ctx); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	closePool(t, p)
	if _, maxSeen := d.stats(); maxSeen > maxLive {
		t.Fatalf("%d instances alive at once, Max is %d", maxSeen, maxLive)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.alive != 0 {
		t.Fatalf("%d instances alive after close", d.alive)
	}
	for _, inst := range d.all {
		if inst.closes.Load() != 1 {
			t.Fatalf("%s closed %d times", inst.id, inst.closes.Load())
		}
	}
}
