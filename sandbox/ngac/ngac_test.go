package ngac_test

import (
	"testing"

	"github.com/urmzd/saige/sandbox/ngac"
)

// buildSpec compiles a spec or fails the test.
func buildSpec(t *testing.T, spec ngac.Spec) *ngac.Graph {
	t.Helper()
	g, err := ngac.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAttributeInheritance(t *testing.T) {
	// Two agents grouped under one user attribute share a grant; a third
	// agent outside the group gets nothing.
	g := buildSpec(t, ngac.Spec{
		PolicyClasses: []string{"pc"},
		UserAttrs:     []ngac.Node{{Name: "agents"}},
		Users: []ngac.Node{
			{Name: "planner", Parents: []string{"agents"}},
			{Name: "reviewer", Parents: []string{"agents"}},
			{Name: "outsider"},
		},
		ObjectAttrs: []ngac.ObjectAttr{
			{Name: "shared", Prefix: "shared", Parents: []string{"pc"}},
		},
		Associations: []ngac.Association{
			{UserAttr: "agents", Ops: []string{"read", "write"}, Target: "shared"},
		},
	})

	tests := []struct {
		user, op, object string
		want             bool
	}{
		{"planner", "read", "shared/notes", true},
		{"reviewer", "write", "shared/report", true},
		{"outsider", "read", "shared/notes", false},
		{"planner", "delete", "shared/notes", false},     // op not granted
		{"planner", "read", "shared-extra/x", false},     // sibling prefix
		{"planner", "read", "elsewhere/notes", false},    // outside every attribute
		{"unknown-agent", "read", "shared/notes", false}, // unknown user
	}
	for _, tt := range tests {
		if got := g.Can(tt.user, tt.op, tt.object); got != tt.want {
			t.Errorf("Can(%s, %s, %s) = %v, want %v", tt.user, tt.op, tt.object, got, tt.want)
		}
	}
}

func TestMultiplePolicyClassesMustAllBeSatisfied(t *testing.T) {
	// The object attribute sits in two policy classes; NGAC requires every
	// containing class to grant the access.
	spec := ngac.Spec{
		PolicyClasses: []string{"ops-policy", "compliance-policy"},
		Users:         []ngac.Node{{Name: "agent"}},
		ObjectAttrs: []ngac.ObjectAttr{
			{Name: "records", Prefix: "records", Parents: []string{"ops-policy", "compliance-policy"}},
		},
		Associations: []ngac.Association{
			{UserAttr: "agent", Ops: []string{"read"}, Target: "records"},
		},
	}
	g := buildSpec(t, spec)
	// The single association's target is contained in both classes, so it
	// satisfies both.
	if !g.Can("agent", "read", "records/2026/q1") {
		t.Fatal("association covering both classes should grant")
	}

	// Split the containment: one attribute per class, association only on
	// the ops side. The compliance class is unsatisfied, so access is
	// denied even though ops grants it.
	g2 := buildSpec(t, ngac.Spec{
		PolicyClasses: []string{"ops-policy", "compliance-policy"},
		Users:         []ngac.Node{{Name: "agent"}},
		ObjectAttrs: []ngac.ObjectAttr{
			{Name: "ops-view", Prefix: "records", Parents: []string{"ops-policy"}},
			{Name: "compliance-view", Prefix: "records", Parents: []string{"compliance-policy"}},
		},
		Associations: []ngac.Association{
			{UserAttr: "agent", Ops: []string{"read"}, Target: "ops-view"},
		},
	})
	if g2.Can("agent", "read", "records/2026/q1") {
		t.Fatal("unsatisfied policy class must deny")
	}
}

func TestProhibitionOverridesAssociation(t *testing.T) {
	g := buildSpec(t, ngac.Spec{
		PolicyClasses: []string{"pc"},
		UserAttrs:     []ngac.Node{{Name: "agents"}},
		Users:         []ngac.Node{{Name: "planner", Parents: []string{"agents"}}},
		ObjectAttrs: []ngac.ObjectAttr{
			{Name: "workspace", Prefix: "", Parents: []string{"pc"}},
			{Name: "quarantine", Prefix: "workspace/quarantine", Parents: []string{"workspace"}},
			{Name: "all", Prefix: "workspace", Parents: []string{"workspace"}},
		},
		Associations: []ngac.Association{
			{UserAttr: "agents", Ops: []string{"read", "write"}, Target: "all"},
		},
		Prohibitions: []ngac.Association{
			{UserAttr: "agents", Ops: []string{"write"}, Target: "quarantine"},
		},
	})

	if !g.Can("planner", "write", "workspace/notes") {
		t.Fatal("grant outside prohibition should hold")
	}
	if g.Can("planner", "write", "workspace/quarantine/incident") {
		t.Fatal("prohibition must override the broad association")
	}
	if !g.Can("planner", "read", "workspace/quarantine/incident") {
		t.Fatal("prohibition is op-scoped: read is still granted")
	}
}

func TestSpawnObjects(t *testing.T) {
	g := buildSpec(t, ngac.Spec{
		PolicyClasses: []string{"pc"},
		Users:         []ngac.Node{{Name: "planner"}, {Name: "worker"}},
		ObjectAttrs:   []ngac.ObjectAttr{{Name: "spawnable", Parents: []string{"pc"}}},
		Objects:       []ngac.Node{{Name: "tool:worker", Parents: []string{"spawnable"}}},
		Associations: []ngac.Association{
			{UserAttr: "planner", Ops: []string{"spawn"}, Target: "spawnable"},
		},
	})
	if !g.Can("planner", "spawn", "tool:worker") {
		t.Fatal("planner should spawn worker")
	}
	if g.Can("worker", "spawn", "tool:worker") {
		t.Fatal("worker has no spawn grant")
	}
	if g.Can("planner", "spawn", "tool:planner") {
		t.Fatal("undeclared spawn target must be denied")
	}
}

func TestGraphValidation(t *testing.T) {
	if _, err := ngac.Build(ngac.Spec{}); err == nil {
		t.Fatal("spec without a policy class must fail")
	}

	g := ngac.New()
	if err := g.AddPolicyClass("pc"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddPolicyClass("pc"); err == nil {
		t.Fatal("duplicate node must fail")
	}
	if err := g.Assign("pc", "ghost"); err == nil {
		t.Fatal("assignment to unknown node must fail")
	}
	if err := g.AddUserAttribute("a"); err != nil {
		t.Fatal(err)
	}
	if err := g.AddUserAttribute("b"); err != nil {
		t.Fatal(err)
	}
	if err := g.Assign("a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := g.Assign("b", "a"); err == nil {
		t.Fatal("cycle must be rejected")
	}
	if err := g.Associate("a", nil, "pc"); err == nil {
		t.Fatal("association without ops must fail")
	}
}
