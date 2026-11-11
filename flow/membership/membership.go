// Package membership records which agents are members of which channels, and
// what each member wakes for.
//
// A member is an agent joined to a channel prefix within one tenant, with a
// Rule. The dispatcher (pkg/dispatch) asks Members for the channel an event
// arrived on and wakes each member whose rule matches the event.
//
// Everything is default deny. A store with no rows has no members, a Rule with
// no Types matches nothing, and an Addressed rule (the default) wakes a member
// only when the event names it: a mention, a review requested from it, an
// issue assigned to it, or a direct message.
package membership

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/event"
)

// Errors. Callers match with errors.Is.
var (
	// ErrInvalid reports a membership that cannot be recorded.
	ErrInvalid = errors.New("membership: invalid")
	// ErrNotFound reports a Leave of a membership that does not exist.
	ErrNotFound = errors.New("membership: not found")
)

// MaxMembers bounds how many memberships Members returns for one channel.
const MaxMembers = 1000

// Rule is what a member wakes for.
type Rule struct {
	// Types are type prefixes (event.Selects). Empty matches nothing.
	Types []string `json:"types,omitempty"`
	// Addressed, when true or unset, wakes the member only for an event that
	// names it (event.Addressees) or arrives as a direct message. Set it to
	// false to wake on every matching event in the channel.
	Addressed *bool `json:"addressed,omitempty"`
}

// IsAddressed reports whether the rule needs the event to address the
// member. It is true unless Addressed is set to false.
func (r Rule) IsAddressed() bool { return r.Addressed == nil || *r.Addressed }

// Ambient returns a copy of r that wakes on every matching event, addressed
// or not.
func (r Rule) Ambient() Rule {
	f := false
	r.Addressed = &f
	return r
}

// Matches reports whether an event wakes agent under this rule.
func (r Rule) Matches(agent string, e event.Envelope) bool {
	if e.Data == nil || !slices.ContainsFunc(r.Types, func(p string) bool { return event.Selects(p, e.Type) }) {
		return false
	}
	if !r.IsAddressed() {
		return true
	}
	return event.DirectMessage(e.Data) || slices.Contains(event.Addressees(e.Data), agent)
}

// Validate checks the rule can be stored.
func (r Rule) Validate() error {
	for _, p := range r.Types {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%w: empty type prefix", ErrInvalid)
		}
	}
	return nil
}

// Member is one agent's membership of one channel.
type Member struct {
	Tenant  string    `json:"tenant"`
	Channel string    `json:"channel"`
	Agent   string    `json:"agent"`
	Rule    Rule      `json:"rule"`
	Joined  time.Time `json:"joined"`
}

// Store records memberships. Implementations must pass the suite in
// pkg/membership/membershiptest.
type Store interface {
	// Join makes agent a member of channel (a channel prefix, see
	// event.ChannelHasPrefix) in tenant with rule. Joining again replaces
	// the rule.
	Join(ctx context.Context, tenant, channel, agent string, rule Rule) error
	// Leave ends a membership. It returns ErrNotFound when there is none.
	Leave(ctx context.Context, tenant, channel, agent string) error
	// Members returns the members of the channel an event arrived on: one
	// per agent, the membership with the most specific channel prefix that
	// selects channel, sorted by agent. At most MaxMembers.
	Members(ctx context.Context, tenant, channel string) ([]Member, error)
}

// validate checks the key of a membership.
func validate(tenant, channel, agent string) error {
	switch {
	case tenant == "":
		return fmt.Errorf("%w: no tenant", ErrInvalid)
	case channel == "":
		return fmt.Errorf("%w: no channel", ErrInvalid)
	case event.IsControl(channel):
		return fmt.Errorf("%w: %q is a control channel", ErrInvalid, channel)
	case !event.ValidToken(agent):
		return fmt.Errorf("%w: agent %q is not a valid name", ErrInvalid, agent)
	}
	return nil
}

// prefixes returns every channel prefix that can select channel: the channel
// itself, and the text before each '/' or '?' with and without that '/'.
// Callers still filter with event.ChannelHasPrefix.
func prefixes(channel string) []string {
	out := []string{channel}
	for i := 0; i < len(channel); i++ {
		switch channel[i] {
		case '/':
			out = append(out, channel[:i], channel[:i+1])
		case '?':
			out = append(out, channel[:i])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// pick keeps one membership per agent, the most specific, and sorts by
// agent.
func pick(channel string, all []Member) []Member {
	best := map[string]Member{}
	for _, m := range all {
		if !event.ChannelHasPrefix(channel, m.Channel) {
			continue
		}
		if cur, ok := best[m.Agent]; !ok || len(m.Channel) > len(cur.Channel) {
			best[m.Agent] = m
		}
	}
	out := make([]Member, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(a.Agent, b.Agent) })
	if len(out) > MaxMembers {
		out = out[:MaxMembers]
	}
	return out
}

func cloneRule(r Rule) Rule {
	r.Types = slices.Clone(r.Types)
	if r.Addressed != nil {
		v := *r.Addressed
		r.Addressed = &v
	}
	return r
}
