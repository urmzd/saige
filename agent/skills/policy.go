package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// AllowList is a tri-state selection of skills: all trusted skills, none,
// or the named ones. The zero value allows none, so a forgotten list fails
// closed.
//
// In JSON it is "*", [] (or null), or a list of names.
type AllowList struct {
	all   bool
	names []string
	// trustedOnly restricts names to trusted skills. Only Intersect sets
	// it, when one side is AllowAll.
	trustedOnly bool
}

// AllowAll permits every trusted skill. Untrusted skills still need to be
// named.
func AllowAll() AllowList { return AllowList{all: true} }

// AllowNone permits no skill.
func AllowNone() AllowList { return AllowList{} }

// AllowNames permits exactly the named skills, trusted or not. Naming a
// skill is the explicit decision that admits an untrusted one.
func AllowNames(names ...string) AllowList {
	return AllowList{names: slices.Compact(slices.Sorted(slices.Values(names)))}
}

// ParseAllowList reads "*" as all, "" as none, and otherwise a list of names
// separated by commas or spaces.
func ParseAllowList(s string) AllowList {
	s = strings.TrimSpace(s)
	switch s {
	case "*":
		return AllowAll()
	case "":
		return AllowNone()
	}
	return AllowNames(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })...)
}

// IsAll reports whether the list permits every trusted skill.
func (l AllowList) IsAll() bool { return l.all }

// Names returns the named skills, sorted. It is empty for AllowAll.
func (l AllowList) Names() []string { return slices.Clone(l.names) }

// Permits reports whether the list admits the skill.
func (l AllowList) Permits(meta SkillMeta) bool {
	if l.all {
		return meta.Trusted
	}
	if l.trustedOnly && !meta.Trusted {
		return false
	}
	_, found := slices.BinarySearch(l.names, meta.Name)
	return found
}

// Intersect returns a list that admits a skill only when both lists do.
func (l AllowList) Intersect(other AllowList) AllowList {
	switch {
	case l.all && other.all:
		return AllowAll()
	case l.all:
		return AllowList{names: other.names, trustedOnly: true}
	case other.all:
		return AllowList{names: l.names, trustedOnly: true}
	}
	var names []string
	for _, n := range l.names {
		if _, ok := slices.BinarySearch(other.names, n); ok {
			names = append(names, n)
		}
	}
	return AllowList{names: names, trustedOnly: l.trustedOnly || other.trustedOnly}
}

// MarshalJSON implements json.Marshaler.
func (l AllowList) MarshalJSON() ([]byte, error) {
	if l.all {
		return []byte(`"*"`), nil
	}
	if l.names == nil {
		return []byte(`[]`), nil
	}
	return json.Marshal(l.names)
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *AllowList) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*l = AllowNone()
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s != "*" {
			return fmt.Errorf(`skills: allow list string must be "*", got %q`, s)
		}
		*l = AllowAll()
		return nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return fmt.Errorf(`skills: allow list must be "*" or a list of names: %w`, err)
	}
	*l = AllowNames(names...)
	return nil
}

// SkillPolicy decides which skills an agent may see and load. owner is the
// agent's name and ownerTools its tool names for the turn. The result must
// be a subset of all.
type SkillPolicy interface {
	Reachable(ctx context.Context, owner string, ownerTools []string, all []SkillMeta) ([]SkillMeta, error)
}

// SkillPolicyFunc adapts a function to SkillPolicy.
type SkillPolicyFunc func(ctx context.Context, owner string, ownerTools []string, all []SkillMeta) ([]SkillMeta, error)

// Reachable implements SkillPolicy.
func (f SkillPolicyFunc) Reachable(ctx context.Context, owner string, ownerTools []string, all []SkillMeta) ([]SkillMeta, error) {
	return f(ctx, owner, ownerTools, all)
}

// AllowListPolicy assigns an AllowList per agent.
//
// Parents makes delegation monotone: an agent listed with a parent can
// reach only skills its parent can also reach, however its own list reads,
// so a sub-agent never gets a skill its delegator lacks.
type AllowListPolicy struct {
	// Default applies to agents without an entry in Owners. Its zero value
	// allows none.
	Default AllowList
	// Owners maps an agent name to its allow list.
	Owners map[string]AllowList
	// Parents maps a sub-agent's name to the agent that delegates to it.
	Parents map[string]string
}

// Reachable implements SkillPolicy.
func (p AllowListPolicy) Reachable(_ context.Context, owner string, _ []string, all []SkillMeta) ([]SkillMeta, error) {
	list, err := p.effective(owner)
	if err != nil {
		return nil, err
	}
	var out []SkillMeta
	for _, m := range all {
		if list.Permits(m) {
			out = append(out, m)
		}
	}
	return out, nil
}

// effective intersects owner's list with every ancestor's.
func (p AllowListPolicy) effective(owner string) (AllowList, error) {
	list := p.listFor(owner)
	seen := map[string]bool{owner: true}
	for cur := owner; ; {
		parent, ok := p.Parents[cur]
		if !ok {
			return list, nil
		}
		if seen[parent] {
			return AllowList{}, fmt.Errorf("skills: parent cycle through %q", parent)
		}
		seen[parent] = true
		list = list.Intersect(p.listFor(parent))
		cur = parent
	}
}

func (p AllowListPolicy) listFor(owner string) AllowList {
	if l, ok := p.Owners[owner]; ok {
		return l
	}
	return p.Default
}
