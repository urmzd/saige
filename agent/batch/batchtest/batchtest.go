// Package batchtest holds test helpers for agent/batch implementations.
package batchtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
)

// StoreConformance checks a batch.Store implementation against the
// contract: create-once, versioned updates, lookups and ordered listing.
func StoreConformance(t *testing.T, s batch.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j := batch.Job{ID: "j1", Provider: "p", Manifest: "m", CustomIDs: []string{"a", "b"}, Prefix: "sbx",
		State: batch.JobSubmitting, CreatedAt: now, UpdatedAt: now}
	got, created, err := s.Create(ctx, j)
	if err != nil || !created || got.Version != 1 {
		t.Fatalf("create = %+v, %v, %v", got, created, err)
	}
	if again, created, err := s.Create(ctx, batch.Job{ID: "j1", Manifest: "other"}); err != nil || created || again.Manifest != "m" {
		t.Fatalf("second create = %+v, %v, %v", again, created, err)
	}
	got.State, got.Handle = batch.JobSubmitted, &types.BatchHandle{Provider: "p", ID: "b1", Meta: map[string]string{"k": "v"}}
	updated, err := s.Update(ctx, got)
	if err != nil || updated.Version != 2 {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := s.Update(ctx, got); !errors.Is(err, batch.ErrJobConflict) {
		t.Fatalf("stale update err = %v, want batch.ErrJobConflict", err)
	}
	read, err := s.Get(ctx, "j1")
	if err != nil || read.Handle == nil || read.Handle.Meta["k"] != "v" || len(read.CustomIDs) != 2 {
		t.Fatalf("get = %+v, %v", read, err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, batch.ErrJobNotFound) {
		t.Fatalf("missing err = %v", err)
	}
	j2 := j
	j2.ID, j2.CreatedAt = "j2", now.Add(time.Second)
	if _, _, err := s.Create(ctx, j2); err != nil {
		t.Fatal(err)
	}
	open, err := s.List(ctx, batch.JobSubmitted)
	if err != nil || len(open) != 1 || open[0].ID != "j1" {
		t.Fatalf("list submitted = %+v, %v", open, err)
	}
	all, err := s.List(ctx)
	if err != nil || len(all) != 2 || all[0].ID != "j1" {
		t.Fatalf("list all = %+v, %v", all, err)
	}
}
