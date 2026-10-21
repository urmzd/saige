package fallback

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func kindError(kind types.ErrorKind) error {
	return &types.ProviderError{Provider: "primary", Model: "m", Kind: kind, Err: errors.New(kind.String())}
}

func TestConditionsAreTheStableNames(t *testing.T) {
	want := []string{"any", "auth", "content_filter", "context_length", "rate_limit", "transient", "unavailable"}
	got := Conditions()
	if !slices.Equal(got, want) {
		t.Fatalf("Conditions() = %v, want %v", got, want)
	}
	got[0] = "edited"
	if Conditions()[0] != OnAny {
		t.Fatal("Conditions returned a shared slice")
	}
	for _, c := range want {
		if test, ok := ConditionTest(c); !ok || test == nil {
			t.Errorf("ConditionTest(%q) has no test", c)
		}
	}
	if _, ok := ConditionTest("timeout"); ok {
		t.Error("an unknown condition has a test")
	}
}

func TestRuleDecidesWhichFailuresFallThrough(t *testing.T) {
	tests := []struct {
		name string
		on   []string
		err  error
		want bool
	}{
		{"named: a rate limit falls through", []string{OnRateLimit}, kindError(types.ErrorKindRateLimit), true},
		{"named: an outage does not", []string{OnRateLimit}, kindError(types.ErrorKindUnavailable), false},
		{"unset: an outage falls through", nil, kindError(types.ErrorKindUnavailable), true},
		{"any: an auth failure falls through", []string{OnAny}, kindError(types.ErrorKindAuth), true},
		{"transient: covers an outage", []string{OnTransient}, kindError(types.ErrorKindUnavailable), true},
		{"transient: covers a rate limit", []string{OnTransient}, kindError(types.ErrorKindRateLimit), true},
		{"transient: not an auth failure", []string{OnTransient}, kindError(types.ErrorKindAuth), false},
		{"auth", []string{OnAuth}, kindError(types.ErrorKindAuth), true},
		{"context length", []string{OnContextLength}, kindError(types.ErrorKindContextLength), true},
		{"content filter", []string{OnContentFilter}, kindError(types.ErrorKindContentFilter), true},
		{"unavailable", []string{OnUnavailable}, kindError(types.ErrorKindUnavailable), true},
		{"several, a duplicate among them", []string{OnAuth, OnRateLimit, OnAuth}, kindError(types.ErrorKindRateLimit), true},
		{"named: an invalid request never does", []string{OnRateLimit, OnTransient}, kindError(types.ErrorKindInvalidRequest), false},
		{"named: a cancelled call never does", []string{OnTransient}, context.Canceled, false},
		{"named: an exhausted budget never does", []string{OnRateLimit}, types.ErrBudgetExceeded, false},
		{"named: no error never does", []string{OnAuth}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := Rule(tc.on...)
			if err != nil {
				t.Fatal(err)
			}
			if got := rule(tc.err); got != tc.want {
				t.Errorf("rule(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestARuleThatCannotBeHonouredIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		on   []string
		want string
	}{
		"unknown":   {[]string{OnAuth, "timeout"}, `unknown condition "timeout"; the conditions are any, auth,`},
		"any mixed": {[]string{OnAny, OnAuth}, `"any" stands alone, and already covers auth`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Rule(tc.on...)
			if !errors.Is(err, types.ErrInvalidConfig) {
				t.Fatalf("Rule(%v) = %v, want ErrInvalidConfig", tc.on, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
	if _, err := Rule(OnAny, OnAny); err != nil {
		t.Errorf("any named twice: %v", err)
	}
}

func TestARuleDrivesTheChain(t *testing.T) {
	rule, err := Rule(OnRateLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind types.ErrorKind
		want string
	}{
		{types.ErrorKindRateLimit, "from-backup"},
		{types.ErrorKindUnavailable, ""},
	} {
		chain, err := New(Config{Providers: []types.Provider{
			&errorProviderSimple{err: kindError(tc.kind)}, &mockProvider{response: "from-backup"},
		}}, WithFallbackOn(rule))
		if err != nil {
			t.Fatal(err)
		}
		ch, err := chain.Stream(context.Background(), types.Request{})
		if tc.want == "" {
			if err == nil || types.KindOf(err) != tc.kind {
				t.Errorf("%v: err = %v, want the primary's failure", tc.kind, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", tc.kind, err)
		}
		var text strings.Builder
		for d := range ch {
			if pd, ok := d.(types.PartDelta); ok {
				text.WriteString(pd.Text)
			}
		}
		if text.String() != tc.want {
			t.Errorf("%v: got %q, want %q", tc.kind, text.String(), tc.want)
		}
	}
}
