package router

import (
	"context"
	"slices"
	"time"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// Route locks. A lock keeps the session on the profile that served the
// previous request, because moving would lose or invalidate state that only
// that profile holds.
//
// A soft lock blocks voluntary switches (a policy's reprobe or preference)
// but still allows failover when that profile fails. A hard lock also blocks
// failover: the request fails rather than reach a profile that would reject
// or misread the conversation.
const (
	// LockToolLoop is soft. The last message carries tool results, so the
	// model is in the middle of a tool loop.
	LockToolLoop = "tool_loop"
	// LockSignedReasoning is hard. The tool loop continues a turn whose
	// reasoning carries a provider signature that must be echoed back to the
	// model that produced it.
	LockSignedReasoning = "signed_reasoning"
	// LockContextCache is soft. The previous profile's provider holds a bound
	// provider-side context cache handle for this conversation. Reported
	// through LockReporter.
	LockContextCache = "context_cache"
)

// LockReporter is an optional interface for providers that hold state tied to
// the conversation, such as a bound context cache handle. The router finds it
// through any decorator stack with wrapper.As.
type LockReporter interface {
	RouteLocks() []string
}

func hasHardLock(locks []string) bool {
	return slices.Contains(locks, LockSignedReasoning)
}

// detectLocks returns the locks the previous profile holds for messages.
func detectLocks(messages []types.Message, previous types.Provider) []string {
	var locks []string
	if toolLoopActive(messages) {
		locks = append(locks, LockToolLoop)
		if signedReasoning(messages) {
			locks = append(locks, LockSignedReasoning)
		}
	}
	if reporter, ok := wrapper.As[LockReporter](previous); ok {
		for _, l := range reporter.RouteLocks() {
			if !slices.Contains(locks, l) {
				locks = append(locks, l)
			}
		}
	}
	return locks
}

// toolLoopActive reports whether the last message returns tool results.
func toolLoopActive(messages []types.Message) bool {
	if len(messages) == 0 {
		return false
	}
	switch m := messages[len(messages)-1].(type) {
	case types.UserMessage:
		for _, c := range m.Parts {
			if _, ok := c.(types.ToolResultPart); ok {
				return true
			}
		}
	case types.SystemMessage:
		for _, c := range m.Parts {
			if _, ok := c.(types.ToolResultPart); ok {
				return true
			}
		}
	}
	return false
}

// signedReasoning reports whether the latest assistant turn carries signed
// reasoning.
func signedReasoning(messages []types.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		a, ok := messages[i].(types.AssistantMessage)
		if !ok {
			continue
		}
		for _, c := range a.Parts {
			if t, ok := c.(types.ThinkingPart); ok && t.Signature != "" {
				return true
			}
		}
		return false
	}
	return false
}

// RouteState is a session's routing history. It is plain data so the host
// can persist it with the conversation, for example next to the branch's
// ConfigPart, and restore it with Session.RestoreRouteState after a
// restart. It never holds credentials or message content.
type RouteState struct {
	// Profile is the sticky profile the session prefers.
	Profile types.ProfileID `json:"profile,omitempty"`
	// Pin is the profile ID or group name a ConfigPart target selected, if
	// any.
	Pin string `json:"pin,omitempty"`
	// Last is the profile attempted most recently, and LastFailed reports
	// whether that attempt failed with a failover-eligible error.
	Last       types.ProfileID `json:"last,omitempty"`
	LastFailed bool            `json:"last_failed,omitempty"`
	// Revision is the Config.Revision that wrote this state.
	Revision string `json:"revision,omitempty"`
	// Switches counts changes of Profile.
	Switches int `json:"switches,omitempty"`
	// TurnsSince counts served requests since Profile last changed, or since
	// the last failed reprobe.
	TurnsSince int `json:"turns_since,omitempty"`
	// FailStreak counts consecutive requests on which Profile failed.
	FailStreak int `json:"fail_streak,omitempty"`
	// WarmPrefixTokens estimates the prompt tokens held in Profile's prompt
	// cache, from the cache reads and writes of the last request it served.
	// It drops to zero when the cache TTL passes, when the conversation
	// shrinks (compaction rewrote the prefix), and when Profile changes.
	WarmPrefixTokens int       `json:"warm_prefix_tokens,omitempty"`
	WarmAt           time.Time `json:"warm_at,omitzero"`
	// Messages is the message count of the last request Profile served.
	Messages int `json:"messages,omitempty"`
	// Locks are the route locks that applied to the last request.
	Locks []string `json:"locks,omitempty"`
}

func (st RouteState) clone() RouteState {
	st.Locks = append([]string(nil), st.Locks...)
	return st
}

// effective returns the state a policy sees: an expired or invalidated warm
// prefix reads as cold.
func (st RouteState) effective(cfg Config, messages int) RouteState {
	if st.WarmPrefixTokens > 0 && (st.WarmAt.IsZero() || cfg.Now().Sub(st.WarmAt) > cfg.PromptCacheTTL || messages < st.Messages) {
		st.WarmPrefixTokens = 0
	}
	return st
}

func (st *RouteState) switchTo(id types.ProfileID) {
	if id == "" || id == st.Profile {
		return
	}
	if st.Profile != "" {
		st.Switches++
	}
	st.Profile = id
	st.FailStreak, st.TurnsSince = 0, 0
	st.WarmPrefixTokens, st.WarmAt, st.Messages = 0, time.Time{}, 0
}

// applyPlan records the decision made before the request runs.
func (st *RouteState) applyPlan(p plan) {
	st.switchTo(p.switchTo)
	st.Locks = append([]string(nil), p.locks...)
}

// record folds the request's outcome into the state.
func (st *RouteState) record(cfg Config, p plan, res outcome) {
	st.Last, st.LastFailed = res.last, res.lastFailed
	if res.homeFailed {
		st.FailStreak++
	} else if res.served != "" && res.served == st.Profile {
		st.FailStreak = 0
	}
	if res.served != "" {
		st.TurnsSince++
	}
	switch {
	case p.followServed:
		st.switchTo(res.served)
	case p.probe != "" && res.served == p.probe:
		st.switchTo(p.probe)
	case p.probe != "":
		st.TurnsSince = 0 // a failed reprobe waits a full interval again
	}
	if res.served != "" && res.served == st.Profile && !res.usage.CacheHit {
		st.WarmPrefixTokens = res.usage.CachedPromptTokens + res.usage.CacheWriteTokens
		st.WarmAt = cfg.Now()
		st.Messages = p.messages
	}
}

// RouteState returns a copy of the session's routing state for persistence.
func (s *Session) RouteState() RouteState {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	return s.shared.state.clone()
}

// RestoreRouteState replaces the session's routing state, for example after
// loading a conversation. Profiles the router no longer defines are dropped.
// State written under a different Config.Revision keeps only its profiles:
// counters, warm prefix, and locks start over. It fails with ErrSessionBusy
// while a request is in flight.
func (s *Session) RestoreRouteState(st RouteState) error {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	if s.shared.busy {
		return ErrSessionBusy
	}
	st = st.clone()
	if st.Pin != "" && !s.router.hasTarget(st.Pin) {
		st.Pin = ""
	}
	for _, id := range []*types.ProfileID{&st.Profile, &st.Last} {
		if *id != "" && !s.router.hasProfile(*id) {
			*id = ""
		}
	}
	if st.Last == "" {
		st.LastFailed = false
	}
	if st.Revision != s.router.cfg.Revision {
		st = RouteState{Profile: st.Profile, Pin: st.Pin, Last: st.Last, LastFailed: st.LastFailed}
	}
	st.Revision = s.router.cfg.Revision
	s.shared.state = st
	return nil
}

// RouteDecision is a session policy's answer for one request.
type RouteDecision struct {
	// Order lists the profiles to try, each at most once.
	Order []types.ProfileID
	// Reason is reported on the first attempt's RouteDelta.
	Reason string
	// Profile, when set, becomes the session's sticky profile before the
	// request runs. It must appear in Order.
	Profile types.ProfileID
	// Probe, when set, is tried first and becomes the sticky profile only if
	// it serves the request. A failed probe leaves the sticky profile and its
	// failure count unchanged. It must appear in Order.
	Probe types.ProfileID
}

// SessionRouterPolicy orders profiles from the session's RouteState. The
// state is a copy; the router applies the decision itself. Implementations
// shared across sessions must support concurrent calls.
type SessionRouterPolicy interface {
	Select(ctx context.Context, rc RouteContext, st RouteState) (RouteDecision, error)
}

// SessionPolicyFunc adapts a function to a SessionRouterPolicy.
type SessionPolicyFunc func(context.Context, RouteContext, RouteState) (RouteDecision, error)

// Select calls f.
func (f SessionPolicyFunc) Select(ctx context.Context, rc RouteContext, st RouteState) (RouteDecision, error) {
	return f(ctx, rc, st)
}

// SwitchCost estimates what leaving from for to costs on the next request:
// the warm prefix is written again at to's cache-write rate instead of read at
// from's cached rate. It is zero for a cold prefix or when to is cheaper.
func SwitchCost(st RouteState, from, to Candidate) types.Cost {
	warm := st.WarmPrefixTokens
	if warm <= 0 || from.ID == to.ID {
		return 0
	}
	stay := from.Capabilities.Pricing.Cost(types.TokenUsage{CachedInputTokens: warm})
	move := to.Capabilities.Pricing.Cost(types.TokenUsage{CacheWriteTokens: warm})
	if move <= stay {
		return 0
	}
	return move - stay
}

// Affinity is a SessionRouterPolicy that prefers profiles in configuration
// order and keeps the session on one profile until it fails repeatedly.
type Affinity struct {
	// FailThreshold is the number of consecutive requests on which the sticky
	// profile must fail before the session moves to the next profile. A
	// single failure still fails over within its request. Zero means 1.
	FailThreshold int
	// ReprobeAfter, when positive, tries the preferred (first eligible)
	// profile again after this many served requests away from it. Zero never
	// reprobes.
	ReprobeAfter int
	// MaxSwitchCost bounds a reprobe: it waits while SwitchCost exceeds this.
	// Zero allows a reprobe only when the switch is free, for example once
	// the warm prefix has expired.
	MaxSwitchCost types.Cost
}

// Select implements SessionRouterPolicy.
func (a Affinity) Select(_ context.Context, rc RouteContext, st RouteState) (RouteDecision, error) {
	c := rc.Candidates
	if len(c) == 0 {
		return RouteDecision{}, ErrNoEligibleProfile
	}
	fits := func(i int) bool {
		w := c[i].Capabilities.ContextWindow
		n := c[i].EstimatedTokens
		if n == 0 {
			n = rc.EstimatedTokens // a candidate list a caller built
		}
		return w == 0 || n <= w
	}
	// firstFit returns the first fitting index from start, cycling and
	// skipping skip, or fallback when none fits.
	firstFit := func(start, skip, fallback int) int {
		for k := range len(c) {
			i := (start + k) % len(c)
			if i != skip && fits(i) {
				return i
			}
		}
		return fallback
	}
	pos := slices.IndexFunc(c, func(x Candidate) bool { return x.ID == st.Profile })
	if pos < 0 {
		i := firstFit(0, -1, 0)
		if st.Profile != "" {
			// The sticky profile cannot serve this request (it lacks a
			// capability the request needs, or rejects its options). Serve
			// it elsewhere without moving the session, so the sticky
			// profile and its warm prefix serve the next request.
			return RouteDecision{Order: rotate(c, i)}, nil
		}
		return RouteDecision{Order: rotate(c, i), Profile: c[i].ID}, nil
	}
	if st.FailStreak >= max(1, a.FailThreshold) && len(c) > 1 {
		i := firstFit(pos+1, pos, (pos+1)%len(c))
		return RouteDecision{Order: rotate(c, i), Profile: c[i].ID, Reason: ReasonSustainedFailure}, nil
	}
	if !fits(pos) {
		if i := firstFit(pos+1, pos, -1); i >= 0 {
			return RouteDecision{Order: rotate(c, i), Profile: c[i].ID, Reason: ReasonContextWindow}, nil
		}
	}
	if a.ReprobeAfter > 0 && pos > 0 && st.TurnsSince >= a.ReprobeAfter && len(rc.Locks) == 0 {
		if i := firstFit(0, -1, -1); i >= 0 && i < pos && SwitchCost(st, c[pos], c[i]) <= a.MaxSwitchCost {
			order := []types.ProfileID{c[i].ID, c[pos].ID}
			for _, x := range c {
				if x.ID != c[i].ID && x.ID != c[pos].ID {
					order = append(order, x.ID)
				}
			}
			return RouteDecision{Order: order, Probe: c[i].ID, Reason: ReasonReprobe}, nil
		}
	}
	return RouteDecision{Order: rotate(c, pos)}, nil
}

// rotate lists every candidate starting at index start.
func rotate(c []Candidate, start int) []types.ProfileID {
	out := make([]types.ProfileID, len(c))
	for i := range out {
		out[i] = c[(start+i)%len(c)].ID
	}
	return out
}
