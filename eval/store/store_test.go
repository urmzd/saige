package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
)

func TestBaselineFor(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i, r := range []eval.RunRecord{
		{ID: "old", Status: eval.RunSucceeded, Outcome: eval.OutcomePassed},
		{ID: "flaky", Status: eval.RunSucceeded, Outcome: eval.OutcomeInconclusive},
		{ID: "broken", Status: eval.RunErrored},
		{ID: "other-suite", Suite: "other", Status: eval.RunSucceeded},
		{ID: "cand", Status: eval.RunSucceeded, Outcome: eval.OutcomeFailed},
		{ID: "later", Status: eval.RunSucceeded},
	} {
		if r.Suite == "" {
			r.Suite = "s"
		}
		r.StartedAt = at.Add(time.Duration(i) * time.Hour)
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cand, _ := s.GetRun(ctx, "cand")
	got, err := store.BaselineFor(ctx, s, "", cand)
	if err != nil || got.ID != "old" {
		t.Fatalf("BaselineFor(cand) = %s, %v; want old", got.ID, err)
	}
	if got, err := store.BaselineFor(ctx, s, "other", cand); err != nil || got.ID != "other-suite" {
		t.Fatalf("BaselineFor(other, cand) = %s, %v; want other-suite", got.ID, err)
	}
	first, _ := s.GetRun(ctx, "old")
	if _, err := store.BaselineFor(ctx, s, "", first); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("BaselineFor(first) = %v, want ErrNotFound", err)
	}
}
