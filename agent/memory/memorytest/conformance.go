// Package memorytest holds the contract every memory.Store must meet, as a
// suite each store's tests run against a fresh instance.
package memorytest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/memory"
)

// Scopes the suite writes to. Tenant and Other share a subject, so a store
// that ignored the tenant would fail.
var (
	Tenant = memory.Scope{Tenant: "acme", Subject: "user-1"}
	Other  = memory.Scope{Tenant: "globex", Subject: "user-1"}
)

// RunConformance runs the Store contract against stores from newStore,
// which must return an empty store each time it is called.
//
// The suite expects a record to be recalled only when it matches the
// query: a store that ranks by vector similarity alone must drop records
// that are unrelated to the query.
func RunConformance(t *testing.T, newStore func(t *testing.T) memory.Store) {
	t.Run("Recall", func(t *testing.T) { testRecall(t, newStore(t)) })
	t.Run("Idempotency", func(t *testing.T) { testIdempotency(t, newStore(t)) })
	t.Run("ReplayConcurrent", func(t *testing.T) { testReplayConcurrent(t, newStore(t)) })
	t.Run("Budget", func(t *testing.T) { testBudget(t, newStore(t)) })
	t.Run("Retention", func(t *testing.T) { testRetention(t, newStore(t)) })
	t.Run("ReadOnly", func(t *testing.T) { testReadOnly(t, newStore(t)) })
	t.Run("Forget", func(t *testing.T) { testForget(t, newStore(t)) })
}

func remember(t *testing.T, store memory.Store, r memory.Record) string {
	t.Helper()
	id, err := store.Remember(context.Background(), r)
	if err != nil {
		t.Fatalf("Remember(%q): %v", r.Content, err)
	}
	return id
}

func contents(recs []memory.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, strings.TrimSpace(r.Content))
	}
	return out
}

func testRecall(t *testing.T, store memory.Store) {
	ctx := context.Background()
	remember(t, store, memory.Record{Scope: Tenant, Content: "prefers dark mode in the editor", Tags: []string{"ui"}})
	remember(t, store, memory.Record{Scope: Tenant.Narrow("proj"), Kind: memory.KindProcedural, Content: "deploy with make release"})
	remember(t, store, memory.Record{Scope: Other, Content: "prefers light mode"})
	remember(t, store, memory.Record{Scope: Tenant, Content: "expired note about mode", ExpiresAt: time.Now().Add(-time.Hour)})

	for _, tc := range []struct {
		name  string
		scope memory.Scope
		query string
		want  []string
	}{
		{"match by content", Tenant, "dark mode", []string{"prefers dark mode in the editor"}},
		{"match by tag", Tenant, "ui", []string{"prefers dark mode in the editor"}},
		{"sub-namespace visible from parent", Tenant, "deploy", []string{"deploy with make release"}},
		{"parent not visible from child", Tenant.Narrow("proj"), "dark", nil},
		{"tenants isolated", Other, "dark", nil},
		{"expired skipped", Tenant, "expired", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := store.Recall(ctx, tc.scope, tc.query, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := contents(recs); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("Recall = %q, want %q", got, tc.want)
			}
		})
	}

	recs, err := store.Recall(ctx, Tenant, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("empty query returned %q, want the two unexpired records", contents(recs))
	}
	for _, r := range recs {
		if r.ID == "" || r.CreatedAt.IsZero() || !r.Kind.Valid() {
			t.Fatalf("recalled record missing fields: %+v", r)
		}
	}
	if _, err := store.Recall(ctx, memory.Scope{Subject: "user-1"}, "dark", 0); !errors.Is(err, memory.ErrNoScope) {
		t.Fatalf("Recall without a tenant err = %v, want ErrNoScope", err)
	}
}

func testIdempotency(t *testing.T, store memory.Store) {
	ctx := context.Background()
	k := memory.Record{Scope: Tenant, Content: "first version of the note", IdempotencyKey: "step-1"}
	id1 := remember(t, store, k)
	k.Content = "second version of the note"
	id2 := remember(t, store, k)
	if id1 != id2 {
		t.Fatalf("ids differ: %s %s", id1, id2)
	}
	if recs, _ := store.Recall(ctx, Tenant, "second", 0); len(recs) != 0 {
		t.Fatal("a repeated idempotent write replaced the record")
	}
	// The same key in another tenant is a different record.
	k.Scope = Other
	if id3 := remember(t, store, k); id3 == "" {
		t.Fatal("empty id")
	}
	if recs, _ := store.Recall(ctx, Other, "second", 0); len(recs) != 1 {
		t.Fatalf("other tenant's write with the same key = %q", contents(recs))
	}
}

// testReplayConcurrent replays one step many times at once, as a resumed
// durable run can, and expects one stored record.
func testReplayConcurrent(t *testing.T, store memory.Store) {
	ctx := context.Background()
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = store.Remember(ctx, memory.Record{Scope: Tenant, Content: "the release branch is called trunk", IdempotencyKey: "call-7"})
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("replay %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("replay %d id = %s, want %s", i, ids[i], ids[0])
		}
	}
	recs, err := store.Recall(ctx, Tenant, "", 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("after replays Recall = %q, %v; want one record", contents(recs), err)
	}
}

func testBudget(t *testing.T, store memory.Store) {
	ctx := context.Background()
	remember(t, store, memory.Record{Scope: Tenant, Content: "prefers dark mode in the editor"})
	if recs, _ := store.Recall(ctx, Tenant, "mode", 1); len(recs) != 0 {
		t.Fatalf("budget ignored: %q", contents(recs))
	}
	for i := range 5 {
		remember(t, store, memory.Record{Scope: Tenant, Content: fmt.Sprintf("mode note %d %s", i, strings.Repeat("x", 60))})
	}
	recs, err := store.Recall(ctx, Tenant, "mode", 40)
	if err != nil {
		t.Fatal(err)
	}
	used := 0
	for _, r := range recs {
		used += memory.EstimateTokens(r.Content)
	}
	if len(recs) == 0 || used > 40 {
		t.Fatalf("budget 40 returned %d records using %d tokens", len(recs), used)
	}
}

func testRetention(t *testing.T, store memory.Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	p := memory.Policy{Retention: time.Hour, Now: func() time.Time { return now.Add(-2 * time.Hour) }}
	if _, err := p.Remember(ctx, store, memory.Record{Scope: Tenant, Content: "the old office is on floor three"}); err != nil {
		t.Fatal(err)
	}
	p.Now = func() time.Time { return now }
	if _, err := p.Remember(ctx, store, memory.Record{Scope: Tenant, Content: "the new office is on floor nine"}); err != nil {
		t.Fatal(err)
	}
	recs, err := store.Recall(ctx, Tenant, "office floor", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(recs); len(got) != 1 || got[0] != "the new office is on floor nine" {
		t.Fatalf("Recall after retention = %q, want only the unexpired record", got)
	}
	if !recs[0].ExpiresAt.Equal(recs[0].CreatedAt.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, CreatedAt = %v", recs[0].ExpiresAt, recs[0].CreatedAt)
	}
}

func testReadOnly(t *testing.T, store memory.Store) {
	ctx := context.Background()
	id := remember(t, store, memory.Record{Scope: Tenant, Content: "a note"})
	if _, err := store.Remember(ctx, memory.Record{Scope: Tenant.AsReadOnly(), Content: "x"}); !errors.Is(err, memory.ErrReadOnly) {
		t.Fatalf("read-only remember err = %v", err)
	}
	if err := store.Forget(ctx, Tenant.AsReadOnly(), id); !errors.Is(err, memory.ErrReadOnly) {
		t.Fatalf("read-only forget err = %v", err)
	}
}

func testForget(t *testing.T, store memory.Store) {
	ctx := context.Background()
	a := remember(t, store, memory.Record{Scope: Tenant, Content: "prefers dark mode in the editor"})
	remember(t, store, memory.Record{Scope: Tenant.Narrow("proj"), Content: "deploy with make release"})
	if err := store.Forget(ctx, Other, a); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("cross-tenant forget err = %v", err)
	}
	if err := store.Forget(ctx, Tenant, a); err != nil {
		t.Fatal(err)
	}
	if recs, _ := store.Recall(ctx, Tenant, "dark", 0); len(recs) != 0 {
		t.Fatal("forgotten record recalled")
	}
	if err := store.Forget(ctx, Tenant, a); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second forget err = %v", err)
	}

	// A record recalled from a parent scope can be forgotten from it.
	recs, err := store.Recall(ctx, Tenant, "deploy", 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Recall = %v, %v", recs, err)
	}
	if err := store.Forget(ctx, Tenant.Narrow("other"), recs[0].ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("sibling forget err = %v", err)
	}
	if err := store.Forget(ctx, Tenant, recs[0].ID); err != nil {
		t.Fatalf("parent forget err = %v", err)
	}
	if recs, _ := store.Recall(ctx, Tenant.Narrow("proj"), "deploy", 0); len(recs) != 0 {
		t.Fatal("record forgotten from parent still recalled")
	}
}
