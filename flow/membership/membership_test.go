package membership_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/membership"
	"github.com/urmzd/saige/flow/membership/membershiptest"
)

const postgresEnv = "SAIGE_TEST_POSTGRES_DSN"

var schemas atomic.Uint64

func TestMemory(t *testing.T) {
	membershiptest.Run(t, func(*testing.T) membership.Store { return membership.NewMemory() })
}

func TestZeroMemoryHasNoMembers(t *testing.T) {
	var m membership.Memory
	got, err := m.Members(context.Background(), "acme", "slack://T1/C1")
	if err != nil || len(got) != 0 {
		t.Fatalf("Members = %v, %v", got, err)
	}
}

func TestPostgres(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run against Postgres", postgresEnv)
	}
	membershiptest.Run(t, func(t *testing.T) membership.Store {
		ctx := context.Background()
		schema := fmt.Sprintf("fibertest_membership_%d_%d", time.Now().UnixNano(), schemas.Add(1))
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = 4
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		ident := pgx.Identifier{schema}.Sanitize()
		if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
			pool.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
			pool.Close()
		})
		s := membership.NewPostgres(pool)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func posted(text string, mentions ...string) event.Envelope {
	return event.Envelope{
		ID: "e1", Source: "slack://T1/C1", Type: event.TypeMessagePosted, Channel: "slack://T1/C1",
		Time: time.Unix(1, 0), Data: event.MessagePosted{Request: event.Request{Text: text, Mentions: mentions}},
	}
}

func TestRuleMatches(t *testing.T) {
	rule := membership.Rule{Types: []string{"message", "github", "jira"}}
	dm := posted("hi")
	dm.Data = event.MessagePosted{Request: event.Request{Text: "hi"}, Slack: &event.SlackRef{ChannelType: "im"}}
	review := event.Envelope{ID: "e2", Source: "github://o/r", Type: event.TypeGitHubReviewRequested, Channel: "github://o/r",
		Time: time.Unix(1, 0), Data: event.GitHubReviewRequested{Reviewer: "docs-bot"}}
	assigned := event.Envelope{ID: "e3", Source: "jira://s/P-1", Type: event.TypeJiraIssueAssigned, Channel: "jira://s/P-1",
		Time: time.Unix(1, 0), Data: event.JiraIssueAssigned{Agent: "docs-bot"}}
	cases := []struct {
		name string
		rule membership.Rule
		e    event.Envelope
		want bool
	}{
		{"mentioned", rule, posted("hi", "docs-bot"), true},
		{"someone else mentioned", rule, posted("hi", "other"), false},
		{"nobody mentioned", rule, posted("hi"), false},
		{"direct message", rule, dm, true},
		{"review requested", rule, review, true},
		{"assigned", rule, assigned, true},
		{"ambient", rule.Ambient(), posted("hi"), true},
		{"type not selected", membership.Rule{Types: []string{"cron"}}.Ambient(), posted("hi", "docs-bot"), false},
		{"no types", membership.Rule{}, posted("hi", "docs-bot"), false},
	}
	for _, c := range cases {
		if got := c.rule.Matches("docs-bot", c.e); got != c.want {
			t.Errorf("%s: Matches = %v; want %v", c.name, got, c.want)
		}
	}
	if !(membership.Rule{}).IsAddressed() {
		t.Error("a zero rule is not addressed; Addressed must default to true")
	}
}
