package session_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/flow/session"
)

func suite(t *testing.T, s session.Store) {
	t.Helper()
	ctx := context.Background()
	tenant := t.Name()
	p, err := s.Open(ctx, tenant, session.Open{Parent: "github://acme/repo/pull/5", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	const calls = 20
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Go(func() {
			r, err := s.Open(ctx, tenant, session.Open{Parent: p.ID, Agent: "security"})
			if err != nil {
				t.Error(err)
			}
			if r.Key.ID() != r.ID {
				t.Error("identity drift")
			}
		})
	}
	wg.Wait()
	children, err := s.List(ctx, tenant, session.Filter{Parent: p.ID})
	if err != nil || len(children) != 1 {
		t.Fatalf("concurrent continuation duplicated children: %v %v", children, err)
	}
	c := children[0]
	if c.Depth != 1 {
		t.Fatal(c.Depth)
	}
	labeled, err := s.Open(ctx, tenant, session.Open{Parent: p.ID, Agent: "security", Label: "a"})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Open(ctx, tenant, session.Open{Parent: p.ID, Agent: "security", Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == labeled.ID || fresh.ID == labeled.ID || fresh.Key.Label == "" {
		t.Fatal("parallel sessions collided")
	}
	if p.ThreadKey().Surface != p.Key.Parent || c.ThreadKey() == p.ThreadKey() || c.ThreadKey() == labeled.ThreadKey() {
		t.Fatal("journal isolation failed")
	}
	if _, err := s.Get(ctx, "another-tenant", p.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("cross tenant lookup: %v", err)
	}
	if _, err := s.Open(ctx, "another-tenant", session.Open{Parent: p.ID, Agent: "security"}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("cross tenant parent: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.Link(ctx, tenant, p.ID, session.Link{Target: c.ID, Kind: "evidence"}); err != nil {
			t.Fatal(err)
		}
		if err := s.BindRun(ctx, tenant, c.ID, "r1"); err != nil {
			t.Fatal(err)
		}
	}
	links, err := s.Links(ctx, tenant, p.ID, 10)
	if err != nil || len(links) != 1 {
		t.Fatalf("link idempotency: %v %v", links, err)
	}
	runs, err := s.Runs(ctx, tenant, c.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run idempotency: %v %v", runs, err)
	}
	if err := s.Link(ctx, tenant, p.ID, session.Link{Target: "missing", Kind: "context"}); !errors.Is(err, session.ErrNotFound) {
		t.Fatal(err)
	}
}
func TestMemoryIdentityAndConfinement(t *testing.T) { suite(t, session.NewMemory()) }
func TestPostgresIdentityAndConfinement(t *testing.T) {
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN is not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := session.NewPostgres(pool)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	suite(t, s)
	budgetSuite(t, s)
	usageSuite(t, s)
	// New adapter on the same pool observes persisted accounts after restart.
	again := session.NewPostgres(pool)
	a, err := again.BudgetAccount(context.Background(), t.Name(), "root")
	if err != nil || !a.Ready() {
		t.Fatal(a, err)
	}
}

// Many callers that open the same new session at once all get it. The
// table holds two unique keys for one identity, the id and the key it is
// derived from, and a concurrent first insert may collide on either: both
// must read as "already open", never as an error. Each round uses a tenant
// no earlier run wrote, so every round races on rows that do not exist yet.
func TestPostgresConcurrentFirstOpenNeverFails(t *testing.T) {
	dsn := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SAIGE_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := session.NewPostgres(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 40; round++ {
		tenant := fmt.Sprintf("%s-%d-%d", t.Name(), time.Now().UnixNano(), round)
		p, err := s.Open(ctx, tenant, session.Open{Parent: "github://acme/repo/pull/5", Agent: "reviewer"})
		if err != nil {
			t.Fatal(err)
		}
		const calls = 16
		ids := make([]string, calls)
		var wg sync.WaitGroup
		for i := 0; i < calls; i++ {
			wg.Go(func() {
				r, err := s.Open(ctx, tenant, session.Open{Parent: p.ID, Agent: "security"})
				if err != nil {
					t.Errorf("round %d: %v", round, err)
					return
				}
				ids[i] = r.ID
			})
		}
		wg.Wait()
		if t.Failed() {
			return
		}
		for _, id := range ids {
			if id != ids[0] {
				t.Fatalf("round %d: callers opened different sessions: %v", round, ids)
			}
		}
		children, err := s.List(ctx, tenant, session.Filter{Parent: p.ID})
		if err != nil || len(children) != 1 {
			t.Fatalf("round %d: concurrent first open duplicated children: %v %v", round, children, err)
		}
	}
}

func TestConcurrentAdmissionNeverExceedsEightAndGrantsDoNotCrossTenants(t *testing.T) {
	ctx := context.Background()
	s := session.NewMemory()
	p, err := s.Open(ctx, "t", session.Open{Parent: "console://t/p", Agent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			label := fmt.Sprint(i)
			c, err := s.Open(ctx, "t", session.Open{Parent: p.ID, Agent: "helper", Label: label})
			if err != nil {
				t.Error(err)
				return
			}
			err = s.Reserve(ctx, "t", p.ID, label, c.ID, 8)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, session.ErrLimit) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 8 {
		t.Fatalf("accepted %d pending children, want 8", accepted.Load())
	}
	rows, err := s.Reservations(ctx, "t", p.ID)
	if err != nil || len(rows) != 8 {
		t.Fatal(rows, err)
	}
	first := rows[0]
	if err := s.Reserve(ctx, "t", p.ID, first.Call, first.Child, 8); err != nil {
		t.Fatal("retry consumed another slot", err)
	}
	if err := s.BindReservation(ctx, "t", p.ID, first.Call, "run-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "t", p.ID, first.Call); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, "t", p.ID, "replacement", first.Child, 8); err != nil {
		t.Fatal(err)
	}
	other, err := s.Open(ctx, "t", session.Open{Parent: "console://t/other", Agent: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MayRead(ctx, "t", p.ID, other.ID); err != nil || ok {
		t.Fatal("read granted by default", err)
	}
	if err := s.GrantRead(ctx, "t", p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MayRead(ctx, "t", p.ID, other.ID); err != nil || !ok {
		t.Fatal("explicit share lost", err)
	}
	if err := s.GrantRead(ctx, "other", p.ID, other.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("cross tenant share", err)
	}
	if err := s.RevokeRead(ctx, "t", p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MayRead(ctx, "t", p.ID, other.ID); ok {
		t.Fatal("revocation lost")
	}
}
