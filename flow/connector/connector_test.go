package connector_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
)

// Three connectors covering the three shapes the mesh actually has: a full
// peer, a trigger with no surface to deliver to, and a sink that raises
// nothing.

type fullPeer struct{ name string }

func (f fullPeer) Name() string { return f.name }
func (f fullPeer) Ingest(ctx context.Context, _ connector.Publish) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f fullPeer) Deliver(context.Context, address.Address, event.Envelope) error { return nil }
func (f fullPeer) Actions() []connector.Action {
	return []connector.Action{{Name: f.name + "_comment", Description: "post a comment"}}
}

type triggerOnly struct{}

func (triggerOnly) Name() string { return "cron" }
func (triggerOnly) Ingest(ctx context.Context, _ connector.Publish) error {
	<-ctx.Done()
	return ctx.Err()
}

type sinkOnly struct{}

func (sinkOnly) Name() string                                                   { return "webhook" }
func (sinkOnly) Deliver(context.Context, address.Address, event.Envelope) error { return nil }

type inert struct{}

func (inert) Name() string { return "inert" }

func TestRolesAreDerivedNotDeclared(t *testing.T) {
	cases := []struct {
		name string
		c    connector.Connector
		want connector.Roles
	}{
		{"full peer", fullPeer{name: "github"}, connector.Roles{Trigger: true, Sink: true, Actor: true}},
		{"cron triggers only", triggerOnly{}, connector.Roles{Trigger: true}},
		{"webhook sinks only", sinkOnly{}, connector.Roles{Sink: true}},
		{"inert", inert{}, connector.Roles{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connector.RolesOf(tc.c); got != tc.want {
				t.Fatalf("RolesOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRegisterRejectsFacelessConnector(t *testing.T) {
	r := connector.NewRegistry()
	if err := r.Register(inert{}); err == nil {
		t.Fatal("registered a connector with no faces")
	} else if !errors.Is(err, connector.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestRegisterRejectsDuplicatesAndBadNames(t *testing.T) {
	r := connector.NewRegistry()
	if err := r.Register(fullPeer{name: "github"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := r.Register(fullPeer{name: "github"}); err == nil {
		t.Error("duplicate registration accepted")
	}
	if err := r.Register(fullPeer{name: ""}); err == nil {
		t.Error("unnamed connector accepted")
	}
	if err := r.Register(fullPeer{name: "not.safe"}); err == nil {
		t.Error("name that is not scheme-safe accepted")
	}
}

func TestSinkResolvesByTargetScheme(t *testing.T) {
	r := connector.NewRegistry()
	for _, c := range []connector.Connector{fullPeer{name: "github"}, triggerOnly{}, sinkOnly{}} {
		if err := r.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	if _, err := r.Sink(address.MustParse("github://urmzd/fiber/issues/42")); err != nil {
		t.Errorf("github sink: %v", err)
	}
	if _, err := r.Sink(address.MustParse("webhook://acme/deploys")); err != nil {
		t.Errorf("webhook sink: %v", err)
	}

	// A schedule has no surface: delivering to it is a design error, and the
	// registry says so rather than silently dropping the event.
	_, err := r.Sink(address.MustParse("cron://acme/nightly"))
	if !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("cron sink error = %v, want ErrUnsupported", err)
	}

	_, err = r.Sink(address.MustParse("linear://acme/ENG-1"))
	if !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unregistered sink error = %v, want ErrNotFound", err)
	}
}

func TestTriggersAndActionsAreDeterministic(t *testing.T) {
	r := connector.NewRegistry()
	for _, c := range []connector.Connector{
		fullPeer{name: "slack"}, fullPeer{name: "github"}, triggerOnly{}, sinkOnly{},
	} {
		if err := r.Register(c); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	// cron, github, slack trigger; webhook does not.
	if got := len(r.Triggers()); got != 3 {
		t.Errorf("Triggers() = %d, want 3", got)
	}

	// Only the full peers contribute actions, in sorted connector order.
	actions := r.Actions()
	if len(actions) != 2 {
		t.Fatalf("Actions() = %d, want 2", len(actions))
	}
	if actions[0].Name != "github_comment" || actions[1].Name != "slack_comment" {
		t.Fatalf("Actions() not in deterministic order: %q, %q", actions[0].Name, actions[1].Name)
	}

	want := []string{"cron", "github", "slack", "webhook"}
	got := r.Names()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestActionInvokeCarriesTarget(t *testing.T) {
	var gotTarget address.Address
	action := connector.Action{
		Name: "jira_transition",
		Invoke: func(_ context.Context, target address.Address, args json.RawMessage) (connector.Result, error) {
			gotTarget = target
			return connector.Result{Summary: "moved to Done", Address: target}, nil
		},
	}

	target := address.MustParse("jira://acme/PROJ-5")
	res, err := action.Invoke(context.Background(), target, json.RawMessage(`{"to":"Done"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotTarget.String() != target.String() {
		t.Fatalf("action received target %q, want %q", gotTarget, target)
	}
	if res.Address.String() != target.String() {
		t.Fatalf("result address = %q, want the acted-on target", res.Address)
	}
}
