package bus_test

import (
	"context"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
	"os"
	"testing"
	"time"
)

func TestFeedbackDeduplicatesByIdentityWithoutAdvancingExecution(t *testing.T) {
	t.Run("memory", func(t *testing.T) { feedbackIdentity(t, bus.NewMemory()) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv(postgresEnv)
		if dsn == "" {
			t.Skip("set " + postgresEnv)
		}
		b := bus.NewPostgres(newPostgresPool(t, dsn))
		if err := b.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		feedbackIdentity(t, b)
	})
}
func feedbackIdentity(t *testing.T, b bus.Bus) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := b.Subscribe(ctx, bus.Subscription{Tenant: "t", Channels: []string{"slack://T/C"}, Types: []string{"run", event.TypeFeedback}, Run: "r"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	env := event.Envelope{Tenant: "t", ID: "start", Source: "fiber://run/r", Channel: "slack://T/C", Run: "r", Seq: 1, Type: event.TypeStarted, Data: event.Started{}}
	if err := b.Publish(ctx, env); err != nil {
		t.Fatal(err)
	}
	feedback := env
	feedback.ID = "feedback1"
	feedback.Seq = 0
	feedback.Type = event.TypeFeedback
	feedback.Data = event.Feedback{RunID: "r", Rating: "up", Actor: event.Actor{ID: "u"}}
	for range 2 {
		if err := b.Publish(ctx, feedback); err != nil {
			t.Fatal(err)
		}
	}
	feedback.ID = "feedback2"
	if err := b.Publish(ctx, feedback); err != nil {
		t.Fatal(err)
	}
	env.ID = "done"
	env.Seq = 2
	env.Type = event.TypeCompleted
	env.Data = event.Completed{Text: "done"}
	if err := b.Publish(ctx, env); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"start", "feedback1", "feedback2", "done"} {
		e, err := sub.Recv(ctx)
		if err != nil || e.ID != id {
			t.Fatalf("got %s %v want %s", e.ID, err, id)
		}
	}
}
