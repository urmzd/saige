package router

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/types"
)

// scripted answers each call from fail: a nil error streams "ok" from the
// profile, a non-nil error is sent as an ErrorDelta before any output.
type scripted struct {
	id    string
	caps  types.ModelCapabilities
	fail  func(n int) error
	usage *types.UsageDelta
	calls *atomic.Int32
}

func newScripted(id, vendor string, window int, fail func(int) error) scripted {
	return scripted{
		id:    id,
		caps:  types.ModelCapabilities{Provider: vendor, Model: id, ContextWindow: window, Caps: map[types.Capability]bool{types.CapTools: true}},
		fail:  fail,
		calls: &atomic.Int32{},
	}
}

func (p scripted) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	n := int(p.calls.Add(1))
	if p.fail != nil {
		if err := p.fail(n); err != nil {
			return deltas(types.ErrorDelta{Error: err}), nil
		}
	}
	out := []types.Delta{types.PartDelta{Index: 0, Text: p.id}}
	if p.usage != nil {
		out = append(out, *p.usage)
	}
	return deltas(out...), nil
}
func (p scripted) Name() string                          { return p.caps.Provider }
func (p scripted) Model() string                         { return p.id }
func (p scripted) Capabilities() types.ModelCapabilities { return p.caps }

func always(err error) func(int) error { return func(int) error { return err } }
func failCalls(err error, calls ...int) func(int) error {
	return func(n int) error {
		for _, c := range calls {
			if c == n {
				return err
			}
		}
		return nil
	}
}

func kindErr(k types.ErrorKind) error {
	return &types.ProviderError{Kind: k, Err: errors.New(k.String())}
}

type turn struct {
	text   string
	routes []types.RouteDelta
	err    error
}

func run(t *testing.T, p types.Provider, msgs ...types.Message) turn {
	t.Helper()
	if len(msgs) == 0 {
		msgs = []types.Message{types.UserMsg(types.Text("hello"))}
	}
	ch, err := p.Stream(context.Background(), types.Request{Messages: msgs})
	if err != nil {
		return turn{err: err}
	}
	var out turn
	for d := range ch {
		switch v := d.(type) {
		case types.PartDelta:
			out.text += v.Text
		case types.RouteDelta:
			out.routes = append(out.routes, v)
		case types.ErrorDelta:
			out.err = v.Error
		}
	}
	return out
}

func profiles(ps ...scripted) []Profile {
	out := make([]Profile, len(ps))
	for i, p := range ps {
		out[i] = Profile{ID: types.ProfileID(p.id), Provider: p}
	}
	return out
}

func TestPinnedModelKeepsSessionStateAndFailover(t *testing.T) {
	a := newScripted("a", "x", 0, nil)
	b := newScripted("b", "y", 0, failCalls(kindErr(types.ErrorKindTransient), 1))
	r, err := New(Config{Profiles: profiles(a, b)})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()

	pin := func() types.Provider {
		p, err := types.ProviderWithTarget(s, types.ProfileTarget("b"))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Each turn re-applies the pin, as the agent loop does with a ConfigPart
	// target.
	got := run(t, pin())
	if got.err != nil || got.text != "a" {
		t.Fatalf("pinned profile failure did not fail over: %+v", got)
	}
	if got.routes[0].Profile != "b" || got.routes[0].Reason != ReasonPinned || got.routes[1].Reason != ReasonFailover {
		t.Fatalf("routes = %+v", got.routes)
	}
	for range 2 {
		if got := run(t, pin()); got.text != "b" {
			t.Fatalf("pin lost: %+v", got)
		}
	}
	// The base session is what the agent holds when Model() already matches.
	if got := run(t, s); got.text != "b" {
		t.Fatalf("base session dropped the pin: %+v", got)
	}
	st := s.RouteState()
	if st.Pin != "b" || st.Profile != "b" {
		t.Fatalf("state = %+v", st)
	}
	s.Unpin()
	if st := s.RouteState(); st.Pin != "" {
		t.Fatalf("unpin kept %q", st.Pin)
	}
	if _, err := s.WithTarget(types.ProfileTarget("missing")); !errors.Is(err, ErrUnknownProfile) {
		t.Fatal("unknown profile accepted")
	}
	if pin().(*Session).shared != s.shared {
		t.Fatal("pinned view does not share session state")
	}
}

func TestAffinitySustainedFailureThreshold(t *testing.T) {
	transient := kindErr(types.ErrorKindTransient)
	for _, tc := range []struct {
		name        string
		threshold   int
		primaryFail func(int) error
		wantTexts   []string
		wantProfile types.ProfileID
		wantPrimary int32
	}{
		{"one blip stays", 2, failCalls(transient, 1), []string{"b", "a", "a"}, "a", 3},
		{"sustained moves", 2, failCalls(transient, 1, 2), []string{"b", "b", "b"}, "b", 2},
		{"threshold one moves at once", 1, failCalls(transient, 1), []string{"b", "b", "b"}, "b", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newScripted("a", "x", 0, tc.primaryFail)
			b := newScripted("b", "y", 0, nil)
			r, _ := New(Config{Profiles: profiles(a, b), SessionPolicy: Affinity{FailThreshold: tc.threshold}})
			s := r.Session()
			for i, want := range tc.wantTexts {
				if got := run(t, s); got.err != nil || got.text != want {
					t.Fatalf("turn %d = %+v, want %q", i, got, want)
				}
			}
			st := s.RouteState()
			if st.Profile != tc.wantProfile || a.calls.Load() != tc.wantPrimary {
				t.Fatalf("state %+v, primary calls %d", st, a.calls.Load())
			}
		})
	}
}

func TestAffinityReprobeHonorsSwitchCost(t *testing.T) {
	pricing := types.Pricing{InputPerMTok: 3, CachedInputPerMTok: 0.3, CacheWritePerMTok: 3.75}
	for _, tc := range []struct {
		name      string
		maxCost   types.Cost
		advance   time.Duration
		wantProbe bool
	}{
		{"warm prefix blocks reprobe", 0, 0, false},
		{"budgeted switch cost allows reprobe", types.USD(1), 0, true},
		{"expired prefix is free to leave", 0, 10 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1_000, 0)
			a := newScripted("a", "x", 0, failCalls(kindErr(types.ErrorKindTransient), 1))
			b := newScripted("b", "y", 0, nil)
			b.caps.Pricing = pricing
			b.usage = &types.UsageDelta{PromptTokens: 100_000, CachedPromptTokens: 90_000, CacheWriteTokens: 10_000}
			a.caps.Pricing = pricing
			r, _ := New(Config{
				Profiles:      profiles(a, b),
				SessionPolicy: Affinity{ReprobeAfter: 2, MaxSwitchCost: tc.maxCost},
				Now:           func() time.Time { return now },
			})
			s := r.Session()
			// a fails once, the next turn moves the session to b, and b serves
			// two turns.
			for range 3 {
				run(t, s)
			}
			if st := s.RouteState(); st.Profile != "b" || st.WarmPrefixTokens != 100_000 || st.TurnsSince != 2 {
				t.Fatalf("setup state = %+v", st)
			}
			now = now.Add(tc.advance)
			got := run(t, s)
			probed := got.routes[0].Profile == "a" && got.routes[0].Reason == ReasonReprobe
			if probed != tc.wantProbe {
				t.Fatalf("probe = %v, routes %+v", probed, got.routes)
			}
			if tc.wantProbe && s.RouteState().Profile != "a" {
				t.Fatalf("successful probe did not move: %+v", s.RouteState())
			}
		})
	}
}

func TestFailedReprobeKeepsStickyProfile(t *testing.T) {
	transient := kindErr(types.ErrorKindTransient)
	a := newScripted("a", "x", 0, failCalls(transient, 1, 2))
	b := newScripted("b", "y", 0, nil)
	r, _ := New(Config{Profiles: profiles(a, b), SessionPolicy: Affinity{ReprobeAfter: 1}})
	s := r.Session()
	run(t, s) // a fails and b serves
	run(t, s) // the session moves to b
	got := run(t, s)
	if got.routes[0].Reason != ReasonReprobe || got.text != "b" {
		t.Fatalf("reprobe turn = %+v", got)
	}
	st := s.RouteState()
	if st.Profile != "b" || st.FailStreak != 0 || st.TurnsSince != 0 {
		t.Fatalf("failed probe changed sticky state: %+v", st)
	}
}

func TestRouteLocks(t *testing.T) {
	transient := kindErr(types.ErrorKindTransient)
	toolResult := types.UserToolResults(types.ToolResultPart{CallID: "1", Parts: []types.ToolOutputPart{types.Text("done")}})
	signed := types.AssistantMessage{Parts: []types.AssistantPart{
		types.ThinkingPart{Text: "plan", Signature: "sig"},
		types.ToolCallPart{ID: "1", Name: "lookup"},
	}}
	plain := types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "1", Name: "lookup"}}}
	for _, tc := range []struct {
		name       string
		history    []types.Message
		bFails     bool
		wantText   string
		wantErr    bool
		wantLocks  []string
		wantReason string
	}{
		{"tool loop blocks reprobe", []types.Message{types.UserMsg(types.Text("q")), plain, toolResult}, false, "b", false, []string{LockToolLoop}, ""},
		{"tool loop still fails over", []types.Message{types.UserMsg(types.Text("q")), plain, toolResult}, true, "a", false, []string{LockToolLoop}, ""},
		{"signed reasoning blocks failover", []types.Message{types.UserMsg(types.Text("q")), signed, toolResult}, true, "", true, []string{LockToolLoop, LockSignedReasoning}, ReasonLocked},
		{"new user turn releases locks", []types.Message{types.UserMsg(types.Text("q")), signed, toolResult, types.AssistantMsg(types.Text("a")), types.UserMsg(types.Text("next"))}, false, "a", false, nil, ReasonReprobe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newScripted("a", "x", 0, failCalls(transient, 1))
			bFail := func(n int) error {
				if tc.bFails && n == 3 {
					return transient
				}
				return nil
			}
			b := newScripted("b", "y", 0, bFail)
			r, _ := New(Config{Profiles: profiles(a, b), SessionPolicy: Affinity{ReprobeAfter: 1}})
			s := r.Session()
			run(t, s) // a fails and b serves
			run(t, s) // the session moves to b; the next turn would reprobe a
			got := run(t, s, tc.history...)
			if (got.err != nil) != tc.wantErr || got.text != tc.wantText {
				t.Fatalf("turn = %+v", got)
			}
			if got.routes[0].Reason != tc.wantReason {
				t.Fatalf("reason = %q", got.routes[0].Reason)
			}
			if locks := s.RouteState().Locks; !reflect.DeepEqual(locks, tc.wantLocks) {
				t.Fatalf("locks = %v, want %v", locks, tc.wantLocks)
			}
		})
	}
}

type cacheBound struct{ scripted }

func (cacheBound) RouteLocks() []string { return []string{LockContextCache} }

func TestLockReporterFoundThroughDecorators(t *testing.T) {
	inner := cacheBound{newScripted("g", "google", 0, nil)}
	wrapped := retry.New(inner, retry.DefaultConfig())
	locks := detectLocks([]types.Message{types.UserMsg(types.Text("q"))}, wrapped)
	if !reflect.DeepEqual(locks, []string{LockContextCache}) {
		t.Fatalf("locks = %v", locks)
	}
}

func TestClassifiedFailoverOrder(t *testing.T) {
	contentFilter := func(err error) bool { return DefaultFailoverOn(err) || types.IsContentFilter(err) }
	for _, tc := range []struct {
		name       string
		err        error
		failoverOn func(error) bool
		wantRoutes []string
		wantReason string
		wantText   string
	}{
		{"transient keeps order", kindErr(types.ErrorKindTransient), nil, []string{"small", "same-vendor"}, ReasonFailover, "same-vendor"},
		{"context length goes to largest window", kindErr(types.ErrorKindContextLength), nil, []string{"small", "big"}, ReasonContextLength, "big"},
		{"content filter does not fail over by default", kindErr(types.ErrorKindContentFilter), nil, []string{"small"}, "", ""},
		{"content filter moves to another vendor", kindErr(types.ErrorKindContentFilter), contentFilter, []string{"small", "mid"}, ReasonContentFilter, "mid"},
		{"invalid request stops", kindErr(types.ErrorKindInvalidRequest), nil, []string{"small"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			small := newScripted("small", "x", 8_000, always(tc.err))
			same := newScripted("same-vendor", "x", 4_000, nil)
			mid := newScripted("mid", "y", 16_000, nil)
			big := newScripted("big", "z", 32_000, nil)
			r, _ := New(Config{Profiles: profiles(small, same, mid, big), FailoverOn: tc.failoverOn})
			got := run(t, r.Session())
			var ids []string
			for _, rd := range got.routes {
				ids = append(ids, rd.Profile)
			}
			if !reflect.DeepEqual(ids, tc.wantRoutes) || got.text != tc.wantText {
				t.Fatalf("routes %v text %q err %v", ids, got.text, got.err)
			}
			if tc.wantReason != "" && got.routes[1].Reason != tc.wantReason {
				t.Fatalf("reason = %q", got.routes[1].Reason)
			}
			if tc.wantText == "" && got.err == nil {
				t.Fatal("expected the request to fail")
			}
		})
	}
}

func TestAffinityMovesWhenRequestDoesNotFit(t *testing.T) {
	small := newScripted("small", "x", 100, nil)
	big := newScripted("big", "y", 1_000_000, nil)
	r, _ := New(Config{Profiles: profiles(small, big), SessionPolicy: Affinity{}})
	s := r.Session()
	if got := run(t, s); got.text != "small" {
		t.Fatalf("short prompt = %+v", got)
	}
	long := types.UserMsg(types.Text(string(make([]byte, 4_000))))
	got := run(t, s, long)
	if got.text != "big" || got.routes[0].Reason != ReasonContextWindow {
		t.Fatalf("long prompt = %+v", got)
	}
	if st := s.RouteState(); st.Profile != "big" || st.Switches != 1 {
		t.Fatalf("state = %+v", st)
	}
}

func TestRouteStatePersistence(t *testing.T) {
	a := newScripted("a", "x", 0, nil)
	b := newScripted("b", "y", 0, nil)
	r, _ := New(Config{Profiles: profiles(a, b), Revision: "r2", SessionPolicy: Affinity{}})
	full := RouteState{Profile: "b", Pin: "b", Last: "b", Revision: "r2", Switches: 3, TurnsSince: 4, FailStreak: 1,
		WarmPrefixTokens: 500, WarmAt: time.Unix(10, 0).UTC(), Messages: 6, Locks: []string{LockToolLoop}}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RouteState
	if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, full) {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	for _, tc := range []struct {
		name string
		in   RouteState
		want RouteState
	}{
		{"same revision restores everything", full, full},
		{"other revision keeps profiles only", RouteState{Profile: "b", Last: "a", Revision: "r1", Switches: 9, WarmPrefixTokens: 7},
			RouteState{Profile: "b", Last: "a", Revision: "r2"}},
		{"unknown profiles are dropped", RouteState{Profile: "gone", Pin: "gone", Last: "gone", LastFailed: true, Revision: "r2"},
			RouteState{Revision: "r2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := r.Session()
			if err := s.RestoreRouteState(tc.in); err != nil {
				t.Fatal(err)
			}
			if got := s.RouteState(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("restored %+v, want %+v", got, tc.want)
			}
		})
	}
	s := r.Session()
	if err := s.RestoreRouteState(RouteState{Profile: "b", Revision: "r2"}); err != nil {
		t.Fatal(err)
	}
	if got := run(t, s); got.text != "b" {
		t.Fatalf("restored session did not route to its profile: %+v", got)
	}
}

func TestSwitchCost(t *testing.T) {
	cheap := Candidate{ID: "cheap", Capabilities: types.ModelCapabilities{Pricing: types.Pricing{InputPerMTok: 1, CachedInputPerMTok: 0.1, CacheWritePerMTok: 1.25}}}
	dear := Candidate{ID: "dear", Capabilities: types.ModelCapabilities{Pricing: types.Pricing{InputPerMTok: 10, CachedInputPerMTok: 1, CacheWritePerMTok: 12.5}}}
	for _, tc := range []struct {
		name     string
		warm     int
		from, to Candidate
		want     types.Cost
	}{
		{"cold prefix is free", 0, cheap, dear, 0},
		{"same profile is free", 1_000_000, cheap, cheap, 0},
		{"rewriting the prefix costs the difference", 1_000_000, cheap, dear, types.USD(12.5 - 0.1)},
		{"leaving for a cheaper write is free when it costs less", 1_000_000, dear, cheap, types.USD(1.25 - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SwitchCost(RouteState{WarmPrefixTokens: tc.warm}, tc.from, tc.to); got != tc.want {
				t.Fatalf("SwitchCost = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRouteContextCarriesRequestFacts(t *testing.T) {
	budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(2)})
	var seen RouteContext
	r, _ := New(Config{
		Profiles: profiles(newScripted("a", "x", 0, nil)),
		Budget:   budget,
		SessionPolicy: SessionPolicyFunc(func(_ context.Context, rc RouteContext, _ RouteState) (RouteDecision, error) {
			seen = rc
			return RouteDecision{Order: []types.ProfileID{"a"}}, nil
		}),
	})
	msgs := []types.Message{types.SystemMsg(types.Text("sys")), types.UserMsg(types.Text("hello there"))}
	run(t, r.Session(), msgs...)
	if len(seen.Messages) != 2 || seen.EstimatedTokens != types.EstimateTokens(msgs) {
		t.Fatalf("route context = %+v", seen)
	}
	if !seen.Headroom.Known || seen.Headroom.Remaining != types.USD(2) {
		t.Fatalf("headroom = %+v", seen.Headroom)
	}
}

func TestSessionPolicyValidation(t *testing.T) {
	r, _ := New(Config{
		Profiles: profiles(newScripted("a", "x", 0, nil), newScripted("b", "y", 0, nil)),
		SessionPolicy: SessionPolicyFunc(func(context.Context, RouteContext, RouteState) (RouteDecision, error) {
			return RouteDecision{Order: []types.ProfileID{"a"}, Profile: "b"}, nil
		}),
	})
	if _, err := r.Session().Stream(context.Background(), types.Request{}); err == nil {
		t.Fatal("a sticky profile outside the order was accepted")
	}
}

type optionsScripted struct{ scripted }

func (p optionsScripted) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	return p.scripted.Stream(ctx, req)
}

func (p optionsScripted) SupportsOptions() bool { return true }

func TestOptionsOnlyReachProfilesThatAcceptThem(t *testing.T) {
	plain := newScripted("plain", "x", 0, nil)
	accepting := optionsScripted{newScripted("accepting", "y", 0, nil)}
	accepting.caps.Caps[types.CapToolChoice] = true
	r, _ := New(Config{Profiles: []Profile{{ID: "plain", Provider: plain}, {ID: "accepting", Provider: accepting}}})
	choice := types.ToolChoice{Mode: types.ToolChoiceRequired}
	ch, err := r.Session().Stream(context.Background(), types.Request{Tools: []types.ToolDef{{Name: "t"}}, Options: &types.RequestOptions{ToolChoice: &choice}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for d := range ch {
		if v, ok := d.(types.PartDelta); ok {
			text += v.Text
		}
	}
	if text != "accepting" || plain.calls.Load() != 0 {
		t.Fatalf("text %q, plain calls %d", text, plain.calls.Load())
	}
}

func TestHardLockOutranksPin(t *testing.T) {
	transient := kindErr(types.ErrorKindTransient)
	signed := []types.Message{
		types.UserMsg(types.Text("q")),
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ThinkingPart{Text: "plan", Signature: "sig"},
			types.ToolCallPart{ID: "1", Name: "lookup"},
		}},
		types.UserToolResults(types.ToolResultPart{CallID: "1", Parts: []types.ToolOutputPart{types.Text("done")}}),
	}
	for _, tc := range []struct {
		name string
		// pin is the profile a ConfigPart target selects on the locked turn.
		pin      string
		aFails   bool
		wantText string
		wantErr  bool
	}{
		{"pinned profile fails inside a signed tool loop", "a", true, "", true},
		{"pin to another vendor waits for the loop to end", "b", false, "a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			aFail := func(n int) error {
				if tc.aFails && n == 2 {
					return transient
				}
				return nil
			}
			a := newScripted("a", "x", 0, aFail)
			b := newScripted("b", "y", 0, nil)
			r, err := New(Config{Profiles: profiles(a, b)})
			if err != nil {
				t.Fatal(err)
			}
			s := r.Session()
			pin := func(id string) types.Provider {
				p, err := s.WithTarget(types.ProfileTarget(types.ProfileID(id)))
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			if got := run(t, pin("a")); got.text != "a" {
				t.Fatalf("first turn = %+v", got)
			}

			got := run(t, pin(tc.pin), signed...)
			if (got.err != nil) != tc.wantErr || got.text != tc.wantText {
				t.Fatalf("locked turn = %+v", got)
			}
			if len(got.routes) != 1 || got.routes[0].Profile != "a" || got.routes[0].Reason != ReasonLocked {
				t.Fatalf("locked turn routes = %+v", got.routes)
			}
			if b.calls.Load() != 0 {
				t.Fatalf("signed reasoning reached profile b")
			}
			if st := s.RouteState(); st.Pin != tc.pin {
				t.Fatalf("pin = %q, want %q", st.Pin, tc.pin)
			}

			// The next unlocked turn applies the pin.
			got = run(t, s, append(signed, types.AssistantMsg(types.Text("done")), types.UserMsg(types.Text("next")))...)
			if got.err != nil || got.text != tc.pin || got.routes[0].Reason != ReasonPinned {
				t.Fatalf("unlocked turn = %+v", got)
			}
		})
	}
}

// TestAffinityKeepsStickyProfileForIneligibleRequest checks that a request
// the sticky profile cannot serve is served elsewhere without moving the
// session.
func TestAffinityKeepsStickyProfileForIneligibleRequest(t *testing.T) {
	plain := newScripted("plain", "x", 0, nil)
	delete(plain.caps.Caps, types.CapTools)
	tooled := newScripted("tooled", "y", 0, nil)
	r, err := New(Config{Profiles: profiles(plain, tooled), SessionPolicy: Affinity{}})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Session()
	toolsTurn := func() string {
		ch, err := s.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Tools: []types.ToolDef{{Name: "t"}}})
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for d := range ch {
			if v, ok := d.(types.PartDelta); ok {
				text += v.Text
			}
		}
		return text
	}
	steps := []struct {
		name string
		turn func() string
		want string
	}{
		{"first plain request picks the first profile", func() string { return run(t, s).text }, "plain"},
		{"tool request is served by the capable profile", toolsTurn, "tooled"},
		{"next plain request returns to the sticky profile", func() string { return run(t, s).text }, "plain"},
	}
	for _, step := range steps {
		if got := step.turn(); got != step.want {
			t.Fatalf("%s: served by %q, want %q", step.name, got, step.want)
		}
		if st := s.RouteState(); st.Profile != "plain" || st.Switches != 0 {
			t.Fatalf("%s: state = %+v, want sticky profile plain with no switches", step.name, st)
		}
	}
}
