package ngac

import "fmt"

// Spec is the declarative, JSON-friendly form of a policy graph. It is what
// deployments carry in their service spec: the definition of who may do
// what to which parts of the shared workspace, and which agents may spawn
// which. Build compiles it into a Graph.
type Spec struct {
	// PolicyClasses are the top-level policy containers. At least one is
	// required; objects outside every policy class are unreachable.
	PolicyClasses []string `json:"policy_classes"`
	// UserAttrs group users (and other user attributes) for policy
	// purposes, e.g. "trusted-agents". Parents are user attributes or
	// policy classes.
	UserAttrs []Node `json:"user_attrs,omitempty"`
	// Users are the subjects: agent and tool names. Parents are user
	// attributes.
	Users []Node `json:"users"`
	// ObjectAttrs describe what can be accessed. An attribute with a
	// Prefix contains every workspace key under it. Parents are object
	// attributes or policy classes.
	ObjectAttrs []ObjectAttr `json:"object_attrs"`
	// Objects are concrete named resources, such as spawn targets
	// ("tool:planner"). Parents are object attributes.
	Objects []Node `json:"objects,omitempty"`
	// Associations grant operations: users under UserAttr may perform
	// Ops on everything under Target.
	Associations []Association `json:"associations"`
	// Prohibitions deny operations, overriding associations.
	Prohibitions []Association `json:"prohibitions,omitempty"`
}

// Node declares a named node and its containment edges.
type Node struct {
	Name    string   `json:"name"`
	Parents []string `json:"parents,omitempty"`
}

// ObjectAttr declares an object attribute, optionally matching a workspace
// key prefix.
type ObjectAttr struct {
	Name    string   `json:"name"`
	Parents []string `json:"parents,omitempty"`
	Prefix  string   `json:"prefix,omitempty"`
}

// Association grants (or, as a prohibition, denies) Ops to users contained
// in UserAttr over objects contained in Target.
type Association struct {
	UserAttr string   `json:"user_attr"`
	Ops      []string `json:"ops"`
	Target   string   `json:"target"`
}

// Build compiles the spec into a policy graph. All nodes are created before
// any edge, so declaration order never matters.
func Build(spec Spec) (*Graph, error) {
	if len(spec.PolicyClasses) == 0 {
		return nil, fmt.Errorf("ngac: at least one policy class is required")
	}
	g := New()

	for _, pc := range spec.PolicyClasses {
		if err := g.AddPolicyClass(pc); err != nil {
			return nil, err
		}
	}
	for _, ua := range spec.UserAttrs {
		if err := g.AddUserAttribute(ua.Name); err != nil {
			return nil, err
		}
	}
	for _, u := range spec.Users {
		if err := g.AddUser(u.Name); err != nil {
			return nil, err
		}
	}
	for _, oa := range spec.ObjectAttrs {
		if err := g.AddObjectAttribute(oa.Name, oa.Prefix); err != nil {
			return nil, err
		}
	}
	for _, o := range spec.Objects {
		if err := g.AddObject(o.Name); err != nil {
			return nil, err
		}
	}

	assign := func(child string, parents []string) error {
		for _, p := range parents {
			if err := g.Assign(child, p); err != nil {
				return err
			}
		}
		return nil
	}
	for _, ua := range spec.UserAttrs {
		if err := assign(ua.Name, ua.Parents); err != nil {
			return nil, err
		}
	}
	for _, u := range spec.Users {
		if err := assign(u.Name, u.Parents); err != nil {
			return nil, err
		}
	}
	for _, oa := range spec.ObjectAttrs {
		if err := assign(oa.Name, oa.Parents); err != nil {
			return nil, err
		}
	}
	for _, o := range spec.Objects {
		if err := assign(o.Name, o.Parents); err != nil {
			return nil, err
		}
	}

	for _, a := range spec.Associations {
		if err := g.Associate(a.UserAttr, a.Ops, a.Target); err != nil {
			return nil, err
		}
	}
	for _, p := range spec.Prohibitions {
		if err := g.Prohibit(p.UserAttr, p.Ops, p.Target); err != nil {
			return nil, err
		}
	}
	return g, nil
}
