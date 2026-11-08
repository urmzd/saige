package connectors_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

type flushingSink interface {
	connector.Sink
	Flush(context.Context) error
}

func TestEveryChatConnectorRecoversPendingTextAndIsolatesTenants(t *testing.T) {
	for _, kind := range []string{"slack", "github"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := deliverystate.NewMemory()
			sf, gf := slack.NewFake(), github.NewFake()
			var target address.Address
			switch kind {
			case "slack":
				target = address.MustParse("slack://T01/C02?thread=1.0")
			case "github":
				target = address.MustParse("github://owner/repo/issues/1")
			}
			replica := func(tenant string) flushingSink {
				var c flushingSink
				var err error
				switch kind {
				case "slack":
					c, err = slack.New(slack.Config{API: sf, Tenant: tenant, DeliveryState: store, DeliveryScope: "same-client", Interval: time.Hour})
				case "github":
					c, err = github.New(github.Config{API: gf, Tenant: tenant, DeliveryState: store, DeliveryScope: "same-client", Interval: time.Hour})
				}
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			bodies := func() []string {
				var out []string
				switch kind {
				case "slack":
					for _, m := range sf.Messages() {
						out = append(out, m.Text)
					}
				case "github":
					for _, m := range gf.Comments() {
						out = append(out, m.Body)
					}
				}
				return out
			}
			text := func(tenant string, seq uint64, body string) event.Envelope {
				return event.Envelope{Tenant: tenant, Run: "same-run", Channel: target.String(), Agent: "agent", Seq: seq, Type: event.TypeText, Data: event.Text{Text: body}}
			}
			for _, tenant := range []string{"one", "two"} {
				c := replica(tenant)
				for _, e := range []event.Envelope{text(tenant, 1, tenant+" "), text(tenant, 2, "tail")} {
					if err := c.Deliver(ctx, target, e); err != nil {
						t.Fatal(err)
					}
				}
			}
			// A restarted tenant-one connector must not flush tenant-two's pending edit.
			c := replica("one")
			if err := c.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			got := bodies()
			if len(got) != 2 || !strings.Contains(got[0], "one tail") || strings.Contains(got[1], "tail") {
				t.Fatalf("tenant recovery leaked or lost text: %q", got)
			}
			if err := c.Deliver(ctx, target, text("two", 3, "wrong")); !errors.Is(err, connector.ErrUndeliverable) {
				t.Fatalf("cross-tenant delivery error=%v", err)
			}
			// Redelivering the last delta after recovery must not append it twice.
			if err := c.Deliver(ctx, target, text("one", 2, "tail")); err != nil {
				t.Fatal(err)
			}
			if err := replica("two").Flush(ctx); err != nil {
				t.Fatal(err)
			}
			got = bodies()
			if len(got) != 2 || strings.Count(got[0], "tail") != 1 || !strings.Contains(got[1], "two tail") {
				t.Fatalf("handoff state=%q", got)
			}
		})
	}
}
