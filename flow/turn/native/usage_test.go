package native_test

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// priced is a scripted model that declares what saige's catalog would: a rate
// card and a context window. Without Capabilities it is a model saige does not
// know.
type priced struct {
	*agenttest.ScriptedProvider
	caps saigetypes.ModelCapabilities
}

func (p *priced) Capabilities() saigetypes.ModelCapabilities { return p.caps }

var sonnetLike = saigetypes.ModelCapabilities{
	Model:         "claude-test",
	ContextWindow: 200_000,
	Known:         true,
	Pricing: saigetypes.Pricing{
		InputPerMTok: 3, OutputPerMTok: 15, CachedInputPerMTok: 0.3, CacheWritePerMTok: 3.75,
		AsOf: "2026-07-24", Source: "test",
	},
}

func withUsage(deltas []saigetypes.Delta, u saigetypes.UsageDelta) []saigetypes.Delta {
	return append(append([]saigetypes.Delta(nil), deltas...), u)
}

func usageCalls(t *testing.T, chunks []event.Body) []event.UsageCall {
	t.Helper()
	var out []event.UsageCall
	for _, c := range chunks {
		if c.Type() != event.TypeUsageCall {
			continue
		}
		call, err := decodeBody[event.UsageCall](c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, call)
	}
	return out
}

// Every model call is published as it completes, carrying saige's figures as
// saige reported them: the call's key, the model that answered, every token
// tier, the cost from the model's rate card with the card's date and currency,
// and the model's context window. The turn's Usage is those calls summed.
func TestEachModelCallIsPublishedWithSaigesFigures(t *testing.T) {
	t.Parallel()

	first := saigetypes.UsageDelta{
		AccountingID: "acct-1", ResponseModel: "claude-test-0",
		PromptTokens: 12_000, CachedPromptTokens: 9_000, CacheWriteTokens: 1_000, CompletionTokens: 400,
	}
	second := saigetypes.UsageDelta{PromptTokens: 13_000, CachedPromptTokens: 12_000, CompletionTokens: 250}
	model := &priced{caps: sonnetLike, ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		withUsage(agenttest.ToolCallResponse("call-1", "lookup", map[string]any{}), first),
		withUsage(agenttest.TextResponse("found it"), second),
	}}}
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Tools = []string{"lookup"}
	r, err := native.New(rev,
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }),
		native.WithTools(native.StaticTools(lookupTool("lookup"))),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	chunks, res := collect(t, r, native.Turn{Text: "find it", Accounting: "run-x/a1"})

	calls := usageCalls(t, chunks)
	if len(calls) != 2 {
		t.Fatalf("published %d usage.call events, want one per model call (2): %v", len(calls), kinds(chunks))
	}
	cost := func(d saigetypes.UsageDelta) *event.Cost {
		return &event.Cost{Micros: int64(sonnetLike.Pricing.Cost(saigetypes.UsageFromDelta(d))), Currency: "USD", AsOf: "2026-07-24"}
	}
	want := []event.UsageCall{
		{
			// Keyed by the turn's own prefix; saige's id rides beside it.
			AccountingID: "run-x/a1/1", SaigeAccountingID: "acct-1", Agent: "docs-bot", Model: "claude-test-0", Requests: 1,
			PromptTokens: 12_000, CachedPromptTokens: 9_000, CacheWriteTokens: 1_000, CompletionTokens: 400,
			Cost: cost(first), ContextWindow: 200_000,
		},
		{
			AccountingID: "run-x/a1/2", Agent: "docs-bot", Model: "claude-test", Requests: 1,
			PromptTokens: 13_000, CachedPromptTokens: 12_000, CompletionTokens: 250,
			Cost: cost(second), ContextWindow: 200_000,
		},
	}
	for i := range want {
		if !equalCall(calls[i], want[i]) {
			t.Errorf("call %d:\n got %+v cost %+v\nwant %+v cost %+v", i+1, calls[i], calls[i].Cost, want[i], want[i].Cost)
		}
	}
	if calls[0].Cost.Micros <= 0 {
		t.Fatalf("a priced call cost %d micros", calls[0].Cost.Micros)
	}

	var sum event.Usage
	for _, c := range calls {
		sum = sum.AddCall(c)
	}
	if !equalUsage(res.Usage, sum) {
		t.Errorf("Result.Usage = %+v, want the calls summed %+v", res.Usage, sum)
	}
	if res.Usage.Cost.Micros != calls[0].Cost.Micros+calls[1].Cost.Micros || res.Usage.UnpricedCalls != 0 {
		t.Errorf("Result.Usage.Cost = %+v (unpriced %d), want the exact sum of the calls", res.Usage.Cost, res.Usage.UnpricedCalls)
	}
}

// A model saige has no rate card for reports its tokens and no cost: nothing
// is guessed, and the call is counted as unpriced. Nor is a window reported
// that the catalog does not declare.
func TestAnUnknownModelReportsTokensAndNoCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model saigetypes.Provider
	}{
		{"a model that declares nothing", &agenttest.ScriptedProvider{}},
		{"a known model with no rate card", &priced{caps: saigetypes.ModelCapabilities{Model: "local", ContextWindow: 8_192, Known: true}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := [][]saigetypes.Delta{withUsage(agenttest.TextResponse("hello"), saigetypes.UsageDelta{PromptTokens: 700, CompletionTokens: 20})}
			switch m := tc.model.(type) {
			case *agenttest.ScriptedProvider:
				m.Responses = script
			case *priced:
				m.ScriptedProvider = &agenttest.ScriptedProvider{Responses: script}
			}
			r, err := native.New(offlineRevision(1, ""),
				native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return tc.model, nil }))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			chunks, res := collect(t, r, native.Turn{Text: "hi"})
			calls := usageCalls(t, chunks)
			if len(calls) != 1 {
				t.Fatalf("published %d usage.call events, want 1", len(calls))
			}
			c := calls[0]
			if c.PromptTokens != 700 || c.CompletionTokens != 20 {
				t.Errorf("tokens = %d in, %d out, want 700 and 20", c.PromptTokens, c.CompletionTokens)
			}
			if c.Cost != nil {
				t.Errorf("an unpriced model reported a cost: %+v", *c.Cost)
			}
			if res.Usage.Cost != nil || res.Usage.UnpricedCalls != 1 {
				t.Errorf("Result.Usage = %+v cost %+v, want no cost and one unpriced call", res.Usage, res.Usage.Cost)
			}
			if p, ok := tc.model.(*priced); ok && c.ContextWindow != p.caps.ContextWindow {
				t.Errorf("ContextWindow = %d, want the catalog's %d", c.ContextWindow, p.caps.ContextWindow)
			}
			if _, ok := tc.model.(*agenttest.ScriptedProvider); ok && c.ContextWindow != 0 {
				t.Errorf("ContextWindow = %d for a model that declares none", c.ContextWindow)
			}
		})
	}
}

// A response served from saige's response cache sent nothing to a provider,
// so it spent nothing: it is reported, flagged, with no tokens and no cost,
// and it is not an unpriced call.
func TestAResponseCacheHitSpendsNothing(t *testing.T) {
	t.Parallel()

	model := &priced{caps: sonnetLike, ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		withUsage(agenttest.TextResponse("cached"), saigetypes.UsageDelta{PromptTokens: 5_000, CompletionTokens: 100, CacheHit: true}),
	}}}
	r, err := native.New(offlineRevision(1, ""),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	chunks, res := collect(t, r, native.Turn{Text: "hi"})
	calls := usageCalls(t, chunks)
	if len(calls) != 1 || !calls[0].ResponseCacheHit || calls[0].PromptTokens != 0 || calls[0].Requests != 0 || calls[0].Cost != nil {
		t.Fatalf("usage.call = %+v, want a flagged call that spent nothing", calls)
	}
	if res.Usage.ModelCalls != 1 || res.Usage.PromptTokens != 0 || res.Usage.UnpricedCalls != 0 {
		t.Errorf("Result.Usage = %+v, want one call, no tokens, nothing unpriced", res.Usage)
	}
}

func equalCall(a, b event.UsageCall) bool {
	return reflect.DeepEqual(a, b)
}

func equalUsage(a, b event.Usage) bool { return reflect.DeepEqual(a, b) }
