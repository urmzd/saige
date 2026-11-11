package thread_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/thread/threadtest"
)

// The Store contract, against every backend this package has: Memory always,
// Postgres when $SAIGE_TEST_POSTGRES_DSN names one.
func TestMemoryConformance(t *testing.T) {
	threadtest.Run(t, func(_ *testing.T, opts ...thread.Option) thread.Store {
		return thread.NewMemory(opts...)
	})
}

func TestPostgresConformance(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run the thread suite against Postgres", postgresEnv)
	}
	threadtest.Run(t, func(t *testing.T, opts ...thread.Option) thread.Store {
		return newPostgres(t, url, opts...)
	})
}

const (
	tenantA = "acme"
	tenantB = "globex"
)

// newStore returns a store whose clock advances a second per read, so that a
// stamped At is deterministic and still strictly increasing.
//
// Which store is decided by the environment, not by the test: every property
// in this file is a property of the Store contract, so the suite runs
// unchanged against Postgres when $SAIGE_TEST_POSTGRES_DSN names one (see
// postgres_test.go) and against Memory otherwise.
func newStore(t *testing.T) thread.Store {
	t.Helper()
	var mu sync.Mutex
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	clock := thread.WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(time.Second)
		return at
	})
	if url := os.Getenv(postgresEnv); url != "" {
		return newPostgres(t, url, clock)
	}
	return thread.NewMemory(clock)
}

func slackThread(agent string) thread.Key {
	return thread.Key{Tenant: tenantA, Agent: agent, Surface: "slack://T01/C02?thread=1699.001"}
}

func user(run, text string) thread.Entry {
	return thread.Entry{Role: thread.RoleUser, Text: text, RunID: run, Actor: event.Actor{ID: "U1", Display: "ada"}}
}

func assistant(run, text string) thread.Entry {
	return thread.Entry{Role: thread.RoleAssistant, Text: text, RunID: run, Actor: event.Actor{ID: "docs-bot", Bot: true}}
}

func mustAppend(t *testing.T, s thread.Store, key thread.Key, entries ...thread.Entry) []thread.Entry {
	t.Helper()
	out, err := s.Append(context.Background(), key, entries...)
	if err != nil {
		t.Fatalf("Append(%s): %v", key, err)
	}
	if len(out) != len(entries) {
		t.Fatalf("Append returned %d entries for %d inputs", len(out), len(entries))
	}
	return out
}

func mustHistory(t *testing.T, s thread.Store, key thread.Key) []thread.Entry {
	t.Helper()
	ctx := context.Background()
	head, err := s.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head(%s): %v", key, err)
	}
	out, err := s.History(ctx, key, head)
	if err != nil {
		t.Fatalf("History(%s, %d): %v", key, head, err)
	}
	return out
}

func texts(entries []thread.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Text
	}
	return out
}

// The thread key rule. What identifies a conversation is the surface and its
// thread; everything else in an address is positioning that moves while the
// conversation stays the same one.
func TestKeyOfKeepsWhatIdentifiesAConversationAndDropsWhatMoves(t *testing.T) {
	t.Parallel()
	tag := func(source string) event.Envelope {
		e := event.Envelope{ID: "Ev1", Tenant: tenantA, Source: "test"}
		if source != "" {
			e.Source = source
		}
		return e
	}
	cases := []struct {
		name, source, want string
	}{
		{"slack thread", "slack://T01/C02?thread=1699.001", "slack://T01/C02?thread=1699.001"},
		{"slack channel", "slack://T01/C02", "slack://T01/C02"},
		{"github pull, head sha dropped", "github://acme/handbook/pull/7?head_sha=abc123", "github://acme/handbook/pull/7"},
		{"github issue", "github://acme/handbook/issues/42", "github://acme/handbook/issues/42"},
		{"thread kept, the rest dropped", "slack://T01/C02?anchor=9&thread=1699.001", "slack://T01/C02?thread=1699.001"},
		{"jira", "jira://acme/PROJ-5", "jira://acme/PROJ-5"},
		{"no source", "", "tag:Ev1"},
	}
	for _, tc := range cases {
		got := thread.KeyOf(tag(tc.source), "Docs-Bot")
		if got.Surface != tc.want {
			t.Errorf("%s: surface = %q, want %q", tc.name, got.Surface, tc.want)
		}
		if got.Tenant != tenantA || got.Agent != "docs-bot" {
			t.Errorf("%s: key = %+v, want tenant %q and the normalized agent name", tc.name, got, tenantA)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: key does not validate: %v", tc.name, err)
		}
		if got.OneShot() != (tc.source == "") {
			t.Errorf("%s: OneShot = %v", tc.name, got.OneShot())
		}
	}

	// Two pushes to one pull request are one conversation; two Slack threads
	// in one channel are two; two events with no surface are two.
	same := [][2]event.Envelope{
		{tag("github://acme/handbook/pull/7?head_sha=aaa"), tag("github://acme/handbook/pull/7?head_sha=bbb")},
	}
	for _, p := range same {
		if thread.KeyOf(p[0], "docs-bot") != thread.KeyOf(p[1], "docs-bot") {
			t.Errorf("%s and %s are different threads", p[0].Source, p[1].Source)
		}
	}
	a, b := tag(""), tag("")
	b.ID = "Ev2"
	different := [][2]event.Envelope{
		{tag("slack://T01/C02?thread=1699.001"), tag("slack://T01/C02?thread=1700.002")},
		{tag("github://acme/handbook/issues/7"), tag("github://acme/handbook/pull/7")},
		{a, b},
	}
	for _, p := range different {
		if thread.KeyOf(p[0], "docs-bot") == thread.KeyOf(p[1], "docs-bot") {
			t.Errorf("%q/%s and %q/%s are one thread", p[0].Source, p[0].ID, p[1].Source, p[1].ID)
		}
	}
}
