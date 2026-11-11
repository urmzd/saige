package bus_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/bus"
)

type heard struct {
	tenant  string
	payload string
}

func listenNotices(t *testing.T, n *bus.Notices, topic string) <-chan heard {
	t.Helper()
	ch := make(chan heard, 16)
	stop := n.Listen(topic, func(tenant string, payload []byte) {
		select {
		case ch <- heard{tenant, string(payload)}:
		default:
		}
	})
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for !n.Listening() {
		if time.Now().After(deadline) {
			t.Fatal("never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ch
}

// samePool opens a second pool on the schema of first: another process on
// the same database.
func samePool(t *testing.T, first *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), first.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A notice sent by one process reaches a listener on another process on the
// same schema, on its topic only, and no listener on another schema.
func TestNoticesCrossProcessesOnOneSchema(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run against Postgres", postgresEnv)
	}
	first := newPostgresPool(t, url)
	a, b := bus.NewNotices(first), bus.NewNotices(samePool(t, first))
	elsewhere := bus.NewNotices(newPostgresPool(t, url))

	onB := listenNotices(t, b, "inbox")
	otherTopic := listenNotices(t, b, "threads")
	otherSchema := listenNotices(t, elsewhere, "inbox")

	if err := a.Send(t.Context(), "inbox", "acme", []byte(`{"id":"m-1"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-onB:
		if got.tenant != "acme" || got.payload != `{"id": "m-1"}` && got.payload != `{"id":"m-1"}` {
			t.Fatalf("heard %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice on the other process")
	}
	select {
	case got := <-otherTopic:
		t.Fatalf("another topic heard %+v", got)
	case got := <-otherSchema:
		t.Fatalf("another schema heard %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// A notice is bounded and must be JSON with a topic and a tenant.
func TestNoticesRefuseWhatIsNotANotice(t *testing.T) {
	url := os.Getenv(postgresEnv)
	if url == "" {
		t.Skipf("set %s to run against Postgres", postgresEnv)
	}
	n := bus.NewNotices(newPostgresPool(t, url))
	for name, c := range map[string]struct {
		topic, tenant, payload string
	}{
		"too big":   {"inbox", "acme", `"` + strings.Repeat("x", bus.MaxNotice) + `"`},
		"not json":  {"inbox", "acme", `{`},
		"no tenant": {"inbox", "", `{}`},
		"no topic":  {"", "acme", `{}`},
	} {
		if err := n.Send(t.Context(), c.topic, c.tenant, []byte(c.payload)); !errors.Is(err, bus.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}
