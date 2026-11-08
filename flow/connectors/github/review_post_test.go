package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

type nonrecursiveDeliveryStore struct {
	deliverystate.Store
	active atomic.Bool
}

func (s *nonrecursiveDeliveryStore) Update(ctx context.Context, key deliverystate.Key, f func([]byte) ([]byte, bool, error)) error {
	if !s.active.CompareAndSwap(false, true) {
		return errors.New("nested delivery transaction")
	}
	defer s.active.Store(false)
	return s.Store.Update(ctx, key, f)
}

func TestReviewEvidenceRetriesAfterDeliveryTransactionAndRestart(t *testing.T) {
	api := NewFake()
	sha := strings.Repeat("a", 40)
	ref := Ref{Owner: "acme", Repo: "repo", Number: 7}
	snapshot := Context{Files: []ContextFile{{Filename: "a.go", Patch: "@@ -1 +1 @@\n-old\n+new"}}}
	snapshot.Subject.Head.SHA = sha
	if err := api.SeedContext(ref, snapshot); err != nil {
		t.Fatal(err)
	}
	st := &nonrecursiveDeliveryStore{Store: deliverystate.NewMemory()}
	fail, indexed := true, 0
	var findingMessage address.Address
	change := "fixed"
	cfg := Config{API: api, Tenant: "t", Login: "humagen[bot]", DeliveryState: st,
		Review: func(View, []ContextFile) (Review, bool, error) {
			return Review{CommitID: sha, Event: "COMMENT", Body: "Checked", Comments: []ReviewComment{{FindingID: "finding", Path: "a.go", Line: 1, Body: "Fix it", Change: &change}}}, true, nil
		},
		ObserveDelivery: func(_ context.Context, d connectors.Delivery) error {
			if d.Finding == "" {
				return nil
			}
			if st.active.Load() {
				t.Fatal("review indexing ran inside delivery transaction")
			}
			findingMessage = d.Message
			if fail {
				fail = false
				return errors.New("index temporarily unavailable")
			}
			indexed++
			return nil
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	target := address.MustParse("github://acme/repo/pull/7").WithParam("head_sha", sha)
	e := event.Envelope{Tenant: "t", Run: "r", Seq: 1, Channel: target.String(), Type: event.TypeCompleted, Data: event.Completed{Text: "review"}}
	if err := c.Deliver(t.Context(), target, e); err == nil || !strings.Contains(err.Error(), "index temporarily unavailable") {
		t.Fatal(err)
	}
	if len(api.SubmittedReviews()) != 1 {
		t.Fatal("native review was not committed")
	}
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 || len(api.SubmittedReviews()) != 1 {
		t.Fatalf("indexed=%d reviews=%d", indexed, len(api.SubmittedReviews()))
	}
	if err := st.Update(t.Context(), c.suggestionKey(findingMessage, "r"), func(raw []byte) ([]byte, bool, error) {
		var got suggestionEvidence
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, false, err
		}
		if got.Commit != sha || got.Finding != "finding" || got.Path != "a.go" || got.Replacement != "fixed" {
			t.Fatalf("lost submitted anchors: %+v", got)
		}
		return raw, false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Fatal("acknowledged indexing stayed pending")
	}
}
