package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/event"
)

// A lineage that would nest a run under nothing, or under itself, is refused.
func TestLineageRules(t *testing.T) {
	const run = "r2"
	for _, tc := range []struct {
		name string
		l    Lineage
		ok   bool
	}{
		{"a root run", Lineage{}, true},
		{"a root run naming itself", Lineage{Root: run}, true},
		{"a called run", Lineage{Root: "r0", ParentRun: "r1", ParentToolCall: "htu_r1_1"}, true},
		{"a run called by the root", Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "c1"}, true},
		{"a called run whose call is not known", Lineage{Root: "r0", ParentRun: "r1"}, true},
		{"a call with no run that made it", Lineage{Root: "r0", ParentToolCall: "c1"}, false},
		{"a call alone", Lineage{ParentToolCall: "c1"}, false},
		{"a parent with no root", Lineage{ParentRun: "r1", ParentToolCall: "c1"}, false},
		{"another run's root with no parent", Lineage{Root: "r0"}, false},
		{"its own parent", Lineage{Root: "r0", ParentRun: run}, false},
		{"a called run rooting its own tree", Lineage{Root: run, ParentRun: "r1"}, false},
	} {
		err := tc.l.validate(run)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: validate = %v, want ErrInvalid", tc.name, err)
		}
	}
}

// A run another run calls is in its caller's tree: the root of a root run's
// child is that root run, and a grandchild keeps the root its parent has.
func TestAChildIsInItsCallersTree(t *testing.T) {
	if got, want := (Lineage{}).Child("r1", "c1"), (Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "c1"}); got != want {
		t.Errorf("a root run's child is %+v, want %+v", got, want)
	}
	mid := Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "c1"}
	if got, want := mid.Child("r2", "c7"), (Lineage{Root: "r1", ParentRun: "r2", ParentToolCall: "c7"}); got != want {
		t.Errorf("a grandchild is %+v, want %+v", got, want)
	}
	if got := (Lineage{}).of("r1"); got != (Lineage{Root: "r1"}) {
		t.Errorf("a root run stamps %+v, want itself as its root", got)
	}
}

// A host admitting a durable child passes the call, or a lineage that agrees
// with the parent the ledger holds; one that names another parent or another
// root is refused rather than corrected.
func TestAHostChildsLineageComesFromItsDurableParent(t *testing.T) {
	root := Input{}
	called := Input{Lineage: Lineage{Root: "r0", ParentRun: "r0", ParentToolCall: "c0"}}
	for _, tc := range []struct {
		name   string
		host   Lineage
		caller Input
		want   Lineage
		ok     bool
	}{
		{"no lineage", Lineage{}, root, Lineage{Root: "r1", ParentRun: "r1"}, true},
		{"the call alone", Lineage{ParentToolCall: "htu_r1_1"}, root, Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "htu_r1_1"}, true},
		{"a whole lineage that agrees", Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "htu_r1_1"}, root, Lineage{Root: "r1", ParentRun: "r1", ParentToolCall: "htu_r1_1"}, true},
		{"under a called parent", Lineage{ParentToolCall: "htu_r1_1"}, called, Lineage{Root: "r0", ParentRun: "r1", ParentToolCall: "htu_r1_1"}, true},
		{"another parent", Lineage{Root: "r1", ParentRun: "r9", ParentToolCall: "c"}, root, Lineage{}, false},
		{"another root", Lineage{Root: "r9", ParentRun: "r1", ParentToolCall: "c"}, called, Lineage{}, false},
	} {
		got, err := childLineage(tc.host, tc.caller, "r1")
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.ok && got != tc.want:
			t.Errorf("%s: lineage %+v, want %+v", tc.name, got, tc.want)
		case !tc.ok && !errors.Is(err, ErrInvalid):
			t.Errorf("%s: childLineage = %v, want ErrInvalid", tc.name, err)
		}
	}
}

// A delegation's child run is called through the parent's tool call, in the
// parent's tree, at any depth.
func TestADelegatedChildIsCalledThroughItsParentsToolCall(t *testing.T) {
	trigger := event.Envelope{
		ID: "evt-1", Source: "github:acme", Type: event.TypeMessagePosted, Channel: "github://acme/github",
		Time: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC), Tenant: "acme",
		Data: event.MessagePosted{Request: event.Request{Text: "ship"}},
	}
	parent := Input{Trigger: trigger, Subscriber: "lead", Run: "r1",
		Lineage: Lineage{Root: "r0", ParentRun: "r0", ParentToolCall: "c0"}}
	in, err := childInput(parent, "ops", "deploy", "q1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Lineage{Root: "r0", ParentRun: "r1", ParentToolCall: "c1"}); in.Lineage != want {
		t.Errorf("child lineage %+v, want %+v", in.Lineage, want)
	}
	if err := in.Lineage.validate(in.runID()); err != nil {
		t.Errorf("the child's lineage is refused: %v", err)
	}
}

// A run with no lineage has an input that names none, so it hashes as an
// input written before lineage existed; one with a lineage reads it back.
func TestLineageIsInTheDurableInputOnlyWhenSet(t *testing.T) {
	trigger := event.Envelope{
		ID: "evt-1", Source: "github:acme", Type: event.TypeMessagePosted, Channel: "github://acme/github",
		Time: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC), Tenant: "acme",
		Data: event.MessagePosted{Request: event.Request{Text: "ship"}},
	}
	root, err := json.Marshal(Input{Trigger: trigger, Subscriber: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(root, []byte("lineage")) {
		t.Errorf("a root run's input names a lineage: %s", root)
	}
	want := Lineage{Root: "r0", ParentRun: "r1", ParentToolCall: "htu_r1_1"}
	raw, err := json.Marshal(Input{Trigger: trigger, Subscriber: "ops", Lineage: want})
	if err != nil {
		t.Fatal(err)
	}
	var back Input
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Lineage != want {
		t.Errorf("lineage read back as %+v, want %+v", back.Lineage, want)
	}
}
