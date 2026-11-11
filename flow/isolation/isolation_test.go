package isolation_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/isolation"
	"github.com/urmzd/saige/flow/isolation/isolationtest"
)

// files is an in-memory Files source.
type files struct {
	mu    sync.Mutex
	data  map[string]string
	saved int
}

func (f *files) Load(ctx context.Context, inst isolation.Instance) error {
	dst := inst.Files()
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.data {
		if err := dst.Write(ctx, k, strings.NewReader(v)); err != nil {
			return err
		}
	}
	return nil
}

func (f *files) Save(ctx context.Context, inst isolation.Instance) error {
	src := inst.Files()
	keys, err := src.List(ctx, "")
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved++
	f.data = map[string]string{}
	for _, k := range keys {
		r, err := src.Read(ctx, k)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(r)
		_ = r.Close()
		f.data[k] = string(b)
	}
	return nil
}

func TestARunLeasesOnceOnFirstUseAndReleasesOnce(t *testing.T) {
	ctx := context.Background()
	l := &isolationtest.Leaser{Dir: t.TempDir()}
	src := &files{data: map[string]string{"notes.md": "kept"}}
	run := isolation.NewRun(l, isolation.Request{Tenant: "acme", Run: "r1"}, src)
	if run.Leased() || len(l.Instances()) != 0 {
		t.Fatal("a run leased before any tool asked")
	}

	var wg sync.WaitGroup
	got := make([]isolation.Instance, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inst, err := run.Instance(ctx)
			if err != nil {
				t.Error(err)
			}
			got[i] = inst
		}()
	}
	wg.Wait()
	if len(l.Instances()) != 1 {
		t.Fatalf("%d leases for one run, want 1", len(l.Instances()))
	}
	for _, inst := range got {
		if inst != got[0] {
			t.Fatal("two callers of one run got two instances")
		}
	}

	// The work directory starts from the source and goes back to it.
	inst := got[0]
	r, err := inst.Files().Read(ctx, "notes.md")
	if err != nil {
		t.Fatalf("the source's file was not loaded: %v", err)
	}
	_ = r.Close()
	if _, err := inst.Exec(ctx, isolation.Command{Argv: []string{"sh", "-c", "echo new > made.txt"}, Env: map[string]string{"PATH": "/bin:/usr/bin"}}, isolation.Stdio{}); err != nil {
		t.Fatal(err)
	}
	var hooked int
	if err := run.OnRelease(ctx, func(context.Context) error { hooked++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := run.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := run.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if hooked != 1 || src.saved != 1 || strings.TrimSpace(src.data["made.txt"]) != "new" {
		t.Fatalf("release ran hooks %d times, saved %d times, kept %q", hooked, src.saved, src.data)
	}
	if !l.Instances()[0].Closed() {
		t.Fatal("the instance outlived its release")
	}
	if _, err := run.Instance(ctx); !errors.Is(err, isolation.ErrReleased) {
		t.Fatalf("Instance after release = %v, want ErrReleased", err)
	}
	// A hook registered after release runs at once.
	if err := run.OnRelease(ctx, func(context.Context) error { hooked++; return nil }); err != nil || hooked != 2 {
		t.Fatalf("a late hook ran %d, %v", hooked, err)
	}
}

func TestARunThatNeverLeasedReleasesNothing(t *testing.T) {
	l := &isolationtest.Leaser{Dir: t.TempDir()}
	src := &files{}
	run := isolation.NewRun(l, isolation.Request{Run: "r"}, src)
	if err := run.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.Instances()) != 0 || src.saved != 0 {
		t.Fatal("releasing an unleased run leased or saved")
	}
}

func TestAFailedLeaseFailsTheRunAndAnUnisolatedRunNeverLeases(t *testing.T) {
	boom := errors.New("no docker")
	run := isolation.NewRun(&isolationtest.Leaser{Fail: boom}, isolation.Request{Run: "r"}, nil)
	for range 2 {
		if _, err := run.Instance(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("Instance = %v, want the lease's failure", err)
		}
	}
	u := isolation.Unisolated(isolation.Request{Run: "r"})
	if u.Isolated() {
		t.Fatal("an unisolated run says it is isolated")
	}
	if _, err := u.Instance(context.Background()); !errors.Is(err, isolation.ErrUnisolated) {
		t.Fatalf("Instance of an unisolated run = %v", err)
	}
}

// A lease the caller's context cancelled is not the run's failure.
func TestACancelledLeaseCanBeTriedAgain(t *testing.T) {
	l := &isolationtest.Leaser{Dir: t.TempDir()}
	run := isolation.NewRun(l, isolation.Request{Run: "r"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := run.Instance(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Instance on a cancelled context = %v", err)
	}
	if _, err := run.Instance(context.Background()); err != nil {
		t.Fatalf("a second try = %v", err)
	}
	_ = run.Release(context.Background())
	if len(l.Open()) != 0 {
		t.Fatal("an instance outlived the release")
	}
}

func TestAProcessSpeaksOverItsStdio(t *testing.T) {
	l := &isolationtest.Leaser{Dir: t.TempDir()}
	inst, err := l.Lease(context.Background(), isolation.Request{Connectors: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(context.Background()) }()
	p, err := isolation.Start(context.Background(), inst, isolation.Command{Argv: []string{"cat"}, In: isolation.Connectors})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(p, "hello"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(p).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if p.Err() != nil || p.ExitCode() != 0 {
		t.Fatalf("cat ended with %v, exit %d", p.Err(), p.ExitCode())
	}
}

// failing is a Files source whose Save fails.
type failing struct{ saved bool }

func (f *failing) Load(context.Context, isolation.Instance) error { return nil }
func (f *failing) Save(context.Context, isolation.Instance) error {
	f.saved = true
	return errors.New("save failed")
}

// AllFiles loads every source into the instance and saves every one at the
// end, even after another fails.
func TestAllFilesSavesEverySource(t *testing.T) {
	ctx := context.Background()
	l := &isolationtest.Leaser{Dir: t.TempDir()}
	first, second := &failing{}, &files{data: map[string]string{"notes.md": "kept"}}
	r := isolation.NewRun(l, isolation.Request{Tenant: "acme", Run: "r1"}, isolation.AllFiles(first, second))
	inst, err := r.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := inst.Files().Read(ctx, "notes.md"); err != nil {
		t.Fatalf("the second source was not loaded: %v", err)
	} else {
		_ = rc.Close()
	}
	if err := r.Release(ctx); err == nil || !strings.Contains(err.Error(), "save failed") {
		t.Fatalf("release = %v, want the first source's failure", err)
	}
	if !first.saved || second.saved != 1 {
		t.Fatalf("saved: first %v, second %d", first.saved, second.saved)
	}
	if len(l.Open()) != 0 {
		t.Fatal("the instance outlived the run")
	}
}
