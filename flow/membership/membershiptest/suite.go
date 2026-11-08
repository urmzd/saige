// Package membershiptest holds every membership.Store to one contract.
package membershiptest

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/flow/membership"
)

// Run runs the contract against stores made by newStore, one per subtest.
func Run(t *testing.T, newStore func(t *testing.T) membership.Store) {
	ctx := context.Background()
	msg := membership.Rule{Types: []string{"message"}}

	t.Run("EmptyHasNoMembers", func(t *testing.T) {
		s := newStore(t)
		got, err := s.Members(ctx, "acme", "slack://T1/C1")
		if err != nil || len(got) != 0 {
			t.Fatalf("Members = %v, %v; want none", got, err)
		}
	})

	t.Run("PrefixSelectsAndTenantScopes", func(t *testing.T) {
		s := newStore(t)
		must(t, s.Join(ctx, "acme", "slack://T1/C1", "docs-bot", msg))
		must(t, s.Join(ctx, "globex", "slack://T1/C1", "other", msg))
		must(t, s.Join(ctx, "acme", "slack://T1/C12", "near-miss", msg))
		got, err := s.Members(ctx, "acme", "slack://T1/C1?thread=1.2")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Agent != "docs-bot" || got[0].Tenant != "acme" {
			t.Fatalf("Members = %+v; want docs-bot of acme only", got)
		}
	})

	t.Run("MostSpecificWins", func(t *testing.T) {
		s := newStore(t)
		must(t, s.Join(ctx, "acme", "slack://T1/", "docs-bot", membership.Rule{Types: []string{"slack"}}))
		must(t, s.Join(ctx, "acme", "slack://T1/C1", "docs-bot", msg.Ambient()))
		got, err := s.Members(ctx, "acme", "slack://T1/C1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Channel != "slack://T1/C1" || got[0].Rule.IsAddressed() {
			t.Fatalf("Members = %+v; want the channel's own ambient membership", got)
		}
	})

	t.Run("JoinReplacesLeaveRemoves", func(t *testing.T) {
		s := newStore(t)
		must(t, s.Join(ctx, "acme", "cron://acme/", "docs-bot", msg))
		must(t, s.Join(ctx, "acme", "cron://acme/", "docs-bot", membership.Rule{Types: []string{"cron"}}))
		got, _ := s.Members(ctx, "acme", "cron://acme/nightly")
		if len(got) != 1 || len(got[0].Rule.Types) != 1 || got[0].Rule.Types[0] != "cron" {
			t.Fatalf("Members = %+v; want the replaced rule", got)
		}
		must(t, s.Leave(ctx, "acme", "cron://acme/", "docs-bot"))
		if err := s.Leave(ctx, "acme", "cron://acme/", "docs-bot"); !errors.Is(err, membership.ErrNotFound) {
			t.Fatalf("second Leave = %v; want ErrNotFound", err)
		}
		if got, _ := s.Members(ctx, "acme", "cron://acme/nightly"); len(got) != 0 {
			t.Fatalf("Members after Leave = %+v", got)
		}
	})

	t.Run("RefusesWhatCannotBeAMember", func(t *testing.T) {
		s := newStore(t)
		for _, c := range []struct{ tenant, channel, agent string }{
			{"", "slack://T1/C1", "docs-bot"},
			{"acme", "", "docs-bot"},
			{"acme", "slack://T1/C1", ""},
			{"acme", "slack://T1/C1", "bad name"},
			{"acme", "control://acme/stop/abc", "docs-bot"},
		} {
			if err := s.Join(ctx, c.tenant, c.channel, c.agent, msg); !errors.Is(err, membership.ErrInvalid) {
				t.Errorf("Join(%q, %q, %q) = %v; want ErrInvalid", c.tenant, c.channel, c.agent, err)
			}
		}
		if err := s.Join(ctx, "acme", "slack://T1/C1", "docs-bot", membership.Rule{Types: []string{""}}); !errors.Is(err, membership.ErrInvalid) {
			t.Errorf("Join with an empty type prefix = %v; want ErrInvalid", err)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
