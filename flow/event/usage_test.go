package event_test

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/event"
)

// Addresses travel as URIs and come back as addresses.
func TestActionAddressesRoundTripThroughTheirURIs(t *testing.T) {
	t.Parallel()

	target := address.MustParse("github://urmzd/fiber/issues/42")
	result := address.MustParse("github://urmzd/fiber/issues/42/comments/7")
	back := event.Action{Name: "github_comment", Target: target.String(), Address: result.String()}
	gotTarget, err := back.TargetAddress()
	if err != nil {
		t.Fatalf("TargetAddress: %v", err)
	}
	if gotTarget.String() != target.String() {
		t.Fatalf("target is %s, want %s", gotTarget, target)
	}
	gotResult, err := back.ResultAddress()
	if err != nil {
		t.Fatalf("ResultAddress: %v", err)
	}
	if gotResult.String() != result.String() {
		t.Fatalf("address is %s, want %s", gotResult, result)
	}
}

// An action with no addresses must not turn the empty string into an error.
func TestAbsentAddressesParseAsUnset(t *testing.T) {
	t.Parallel()

	a := event.Action{Name: "cron_noop"}
	for _, tc := range []struct {
		name string
		get  func() (address.Address, error)
	}{
		{"target", a.TargetAddress},
		{"address", a.ResultAddress},
	} {
		got, err := tc.get()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !got.IsZero() {
			t.Fatalf("%s is %s, want the zero address", tc.name, got)
		}
	}
}

// Cache health is read as one number, and it must not divide by zero for a
// provider that reports no usage at all.
func TestCacheReadRatioIsTheCachedShareOfInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		u    event.Usage
		want float64
	}{
		{"no usage reported", event.Usage{}, 0},
		{"cold cache", event.Usage{PromptTokens: 100, CacheWriteTokens: 100}, 0},
		{"warm cache", event.Usage{PromptTokens: 200, CachedPromptTokens: 150}, 0.75},
	}
	for _, tc := range tests {
		if got := tc.u.CacheReadRatio(); got != tc.want {
			t.Errorf("%s: CacheReadRatio = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func call(id string, depth int, in, out int, micros int64, currency, asOf string) event.UsageCall {
	c := event.UsageCall{AccountingID: id, Depth: depth, Requests: 1, PromptTokens: in, CompletionTokens: out}
	if currency != "" {
		c.Cost = &event.Cost{Micros: micros, Currency: currency, AsOf: asOf}
	}
	return c
}

// Delivery is at-least-once and a call is relayed up every level of a
// delegation, so the same report can arrive more than once. A tally counts an
// AccountingID once, keeps a run's own calls apart from what it delegated, and
// adds money as integers.
func TestATallyCountsEachAccountingIDOnce(t *testing.T) {
	t.Parallel()

	var tally event.Tally
	reports := []event.UsageCall{
		call("r/a1/1", 0, 1000, 50, 4500, "USD", "2026-07-24"),
		call("r/a1/1", 0, 1000, 50, 4500, "USD", "2026-07-24"), // redelivered
		call("r/a1/helper.sub-1/1", 1, 300, 20, 1200, "USD", "2026-06-01"),
		call("r/a1/2", 0, 1100, 60, 5100, "USD", "2026-07-24"),
	}
	fresh := 0
	for _, c := range reports {
		if tally.Call(c) {
			fresh++
		}
	}
	child := event.UsageChild{AccountingID: event.ChildRunAccountingID("r9"), RunID: "r9", Depth: 1,
		Usage: event.Usage{ModelCalls: 2, Requests: 2, PromptTokens: 50, CompletionTokens: 5, Cost: &event.Cost{Micros: 300, Currency: "USD", AsOf: "2026-07-24"}}}
	if !tally.Child(child) || tally.Child(child) {
		t.Fatal("a child run must be counted the first time it is reported and never again")
	}
	if fresh != 3 {
		t.Fatalf("%d calls counted as new, want 3", fresh)
	}

	got := tally.Usage()
	if got.Own.ModelCalls != 2 || got.Own.PromptTokens != 2100 || got.Own.Cost.Micros != 9600 {
		t.Errorf("Own = %+v cost %+v, want the run's two calls", got.Own, got.Own.Cost)
	}
	if got.Children.ModelCalls != 3 || got.Children.Cost.Micros != 1500 || got.Children.Cost.AsOf != "2026-06-01" {
		t.Errorf("Children = %+v cost %+v, want the sub-task's call and the child run, dated by the oldest card", got.Children, got.Children.Cost)
	}
	if got.Total.ModelCalls != 5 || got.Total.Cost.Micros != 11100 || got.Total.Cost.AsOf != "2026-06-01" {
		t.Errorf("Total = %+v cost %+v, want own and children added", got.Total, got.Total.Cost)
	}
	if got.Context == nil || got.Context.Tokens != 1100 {
		t.Errorf("Context = %+v, want the run's own latest call", got.Context)
	}
}

// Money adds only within one currency, and nothing is converted, dropped or
// guessed: Cost is in the first priced call's currency, a call priced in
// another currency is summed in OtherCosts for that currency, and a call with
// no price is counted in UnpricedCalls. Every call's tokens count. A total
// priced from a card with no date has no date.
func TestCostsAddOnlyWithinOneCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		calls        []event.UsageCall
		wantMicros   int64
		wantNoCost   bool
		wantAsOf     string
		wantUnpriced int
		wantOther    []event.Cost
	}{
		{"one currency", []event.UsageCall{call("a", 0, 1, 1, 10, "USD", "2026-07-24"), call("b", 0, 1, 1, 20, "USD", "2026-07-24")}, 30, false, "2026-07-24", 0, nil},
		{"a second currency", []event.UsageCall{call("a", 0, 1, 1, 10, "USD", "2026-07-24"), call("b", 0, 1, 1, 20, "EUR", "2026-07-24"), call("c", 0, 1, 1, 5, "EUR", "")}, 10, false, "2026-07-24", 0, []event.Cost{{Micros: 25, Currency: "EUR"}}},
		{"an unpriced call", []event.UsageCall{call("a", 0, 1, 1, 10, "USD", "2026-07-24"), call("b", 0, 1, 1, 0, "", "")}, 10, false, "2026-07-24", 1, nil},
		{"nothing priced", []event.UsageCall{call("a", 0, 1, 1, 0, "", "")}, 0, true, "", 1, nil},
		{"an undated card", []event.UsageCall{call("a", 0, 1, 1, 10, "USD", "2026-07-24"), call("b", 0, 1, 1, 20, "USD", "")}, 30, false, "", 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var u event.Usage
			for _, c := range tc.calls {
				u = u.AddCall(c)
			}
			if u.PromptTokens != len(tc.calls) {
				t.Errorf("PromptTokens = %d, want every call's tokens counted", u.PromptTokens)
			}
			if !reflect.DeepEqual(u.OtherCosts, tc.wantOther) {
				t.Errorf("OtherCosts = %+v, want %+v", u.OtherCosts, tc.wantOther)
			}
			if u.UnpricedCalls != tc.wantUnpriced {
				t.Errorf("UnpricedCalls = %d, want %d", u.UnpricedCalls, tc.wantUnpriced)
			}
			if tc.wantNoCost {
				if u.Cost != nil {
					t.Errorf("Cost = %+v, want none", *u.Cost)
				}
				return
			}
			if u.Cost == nil || u.Cost.Micros != tc.wantMicros || u.Cost.AsOf != tc.wantAsOf {
				t.Errorf("Cost = %+v, want %d micros as of %q", u.Cost, tc.wantMicros, tc.wantAsOf)
			}
		})
	}
}
