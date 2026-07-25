package catalog

import (
	"testing"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

func TestLookupMatchesLongestPrefix(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		family   string
	}{
		{"anthropic", "claude-sonnet-4-5-20250514", "claude-sonnet-4"},
		{"anthropic", "claude-3-5-sonnet-20241022", "claude-3-5-sonnet"},
		{"openai", "gpt-4o-mini-2024-07-18", "gpt-4o-mini"},
		{"openai", "gpt-4o-2024-08-06", "gpt-4o"},
		{"openai", "o3-mini", "o3"},
		{"google", "gemini-2.5-flash-preview-05-20", "gemini-2.5-flash"},
		{"ollama", "qwen3:4b", "qwen3"},
		{"ollama", "hf.co/someone/qwen3:latest", "qwen3"},
		{"ollama", "nomic-embed-text", "nomic-embed"},
	}
	for _, tt := range tests {
		caps, found := Lookup(tt.provider, tt.model)
		if !found {
			t.Errorf("Lookup(%q, %q) fell through to the baseline", tt.provider, tt.model)
			continue
		}
		if caps.Family != tt.family {
			t.Errorf("Lookup(%q, %q).Family = %q, want %q", tt.provider, tt.model, caps.Family, tt.family)
		}
		if caps.Model != tt.model {
			t.Errorf("Lookup(%q, %q).Model = %q, want the queried model", tt.provider, tt.model, caps.Model)
		}
		if !caps.Known {
			t.Errorf("Lookup(%q, %q).Known = false, want true for a matched entry", tt.provider, tt.model)
		}
	}
}

func TestUnknownModelFallsBackToBaselineAndIsMarkedUnknown(t *testing.T) {
	caps, found := Lookup("anthropic", "claude-something-nobody-shipped")
	if found {
		t.Fatal("an unlisted model must not report a catalog match")
	}
	if caps.Known {
		t.Error("Known must be false so callers can fail closed on unrecognised models")
	}
	if caps.Supports(types.CapReasoning) {
		t.Error("the baseline must not assume reasoning support")
	}
	if !caps.Supports(types.CapTools) {
		t.Error("the baseline should still declare what is safe to assume for the provider")
	}
}

func TestUnknownProviderDeclaresNothing(t *testing.T) {
	caps, found := Lookup("some-gateway", "mystery-1")
	if found {
		t.Fatal("an unknown provider must not report a match")
	}
	if len(caps.List()) != 0 {
		t.Errorf("caps = %v, want nothing declared for an unknown provider", caps.List())
	}
}

// The point of the table: models inside one provider disagree about which
// flags they accept, and the disagreement is not guessable from the provider
// name alone.
func TestReasoningKnobsDifferWithinAProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		want     []types.Capability
		notWant  []types.Capability
	}{
		{
			name:     "anthropic thinking model sizes reasoning by token budget",
			provider: "anthropic", model: "claude-sonnet-4-5",
			want:    []types.Capability{types.CapReasoning, types.CapReasoningBudget, types.CapReasoningSignature},
			notWant: []types.Capability{types.CapReasoningEffort},
		},
		{
			name:     "anthropic 3.5 has no reasoning at all",
			provider: "anthropic", model: "claude-3-5-sonnet-20241022",
			want:    []types.Capability{types.CapTemperature, types.CapTools},
			notWant: []types.Capability{types.CapReasoning, types.CapReasoningBudget},
		},
		{
			name:     "openai reasoning models take effort and reject temperature",
			provider: "openai", model: "o3",
			want:    []types.Capability{types.CapReasoning, types.CapReasoningEffort},
			notWant: []types.Capability{types.CapTemperature, types.CapTopP, types.CapFrequencyPenalty},
		},
		{
			name:     "openai chat models take temperature and have no reasoning",
			provider: "openai", model: "gpt-4o",
			want:    []types.Capability{types.CapTemperature, types.CapTopP, types.CapSeed},
			notWant: []types.Capability{types.CapReasoning, types.CapReasoningEffort},
		},
		{
			name:     "gemini 3 sizes reasoning by level",
			provider: "google", model: "gemini-3-flash-preview",
			want:    []types.Capability{types.CapReasoning, types.CapReasoningEffort},
			notWant: []types.Capability{types.CapReasoningBudget},
		},
		{
			name:     "gemini 2.5 sizes reasoning by budget",
			provider: "google", model: "gemini-2.5-pro",
			want:    []types.Capability{types.CapReasoning, types.CapReasoningBudget},
			notWant: []types.Capability{types.CapReasoningEffort},
		},
		{
			name:     "ollama reasoning models expose only an on/off toggle",
			provider: "ollama", model: "deepseek-r1:8b",
			want:    []types.Capability{types.CapReasoning, types.CapReasoningToggle, types.CapContextWindowOverride},
			notWant: []types.Capability{types.CapReasoningBudget, types.CapReasoningSignature},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := MustLookup(tt.provider, tt.model)
			if got := caps.Missing(tt.want...); len(got) > 0 {
				t.Errorf("missing expected capabilities %v", got)
			}
			for _, c := range tt.notWant {
				if caps.Supports(c) {
					t.Errorf("must not declare %q", c)
				}
			}
		})
	}
}

func TestStructuredOutputModeIsRecordedPerFamily(t *testing.T) {
	// Anthropic has no response_format: the adapter forces a hidden tool call,
	// which is a weaker guarantee and must not be reported as native.
	if got := MustLookup("anthropic", "claude-sonnet-4-5").StructuredOutput; got != types.StructuredOutputToolCall {
		t.Errorf("anthropic StructuredOutput = %q, want tool_call", got)
	}
	if got := MustLookup("openai", "gpt-4o").StructuredOutput; got != types.StructuredOutputNative {
		t.Errorf("openai StructuredOutput = %q, want native", got)
	}
	if got := MustLookup("google", "gemini-2.5-flash").StructuredOutput; got != types.StructuredOutputNative {
		t.Errorf("google StructuredOutput = %q, want native", got)
	}
}

// A correction appends a revision instead of overwriting, so the previous row
// stays inspectable and a bad correction is one Rollback away.
func TestRegisterAppendsARevision(t *testing.T) {
	t.Cleanup(func() { Unpin("openai", "gpt-4o") })

	before := MustLookup("openai", "gpt-4o")
	revsBefore := Revisions("openai", "gpt-4o")

	Register(Entry{Provider: "openai", Prefix: "gpt-4o", Caps: before.With(types.CapReasoning)},
		registry.WithSource("test"), registry.WithNote("pretend the vendor added reasoning"))

	if got := Revisions("openai", "gpt-4o"); got != revsBefore+1 {
		t.Errorf("revisions = %d, want %d: a correction must append", got, revsBefore+1)
	}
	if !MustLookup("openai", "gpt-4o").Supports(types.CapReasoning) {
		t.Error("resolution must take the newest revision")
	}

	// Exactly one family entry, no matter how many revisions it has.
	count := 0
	for _, f := range Families("openai") {
		if f == "gpt-4o" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("gpt-4o appears %d times in Families, want 1 regardless of revision count", count)
	}

	// The old row is still there, and rolling back restores it.
	rolled, err := Rollback("openai", "gpt-4o")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolled.Caps.Supports(types.CapReasoning) {
		t.Error("rollback must return the previous revision")
	}
	if MustLookup("openai", "gpt-4o").Supports(types.CapReasoning) {
		t.Error("after rollback, resolution must take the pinned older revision")
	}
}

// A pin freezes a row so a later correction cannot move the numbers under a
// running budget.
func TestPinHoldsARowAgainstLaterCorrections(t *testing.T) {
	t.Cleanup(func() { Unpin("anthropic", "claude-3-haiku") })

	original := MustLookup("anthropic", "claude-3-haiku")
	pinnedRev := registry.Revision(Revisions("anthropic", "claude-3-haiku"))
	if err := Pin("anthropic", "claude-3-haiku", pinnedRev); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	dearer := original
	dearer.Pricing.InputPerMTok = original.Pricing.InputPerMTok * 10
	Register(Entry{Provider: "anthropic", Prefix: "claude-3-haiku", Caps: dearer})

	if got := MustLookup("anthropic", "claude-3-haiku").Pricing.InputPerMTok; got != original.Pricing.InputPerMTok {
		t.Errorf("pinned row resolved to %v, want the pinned %v", got, original.Pricing.InputPerMTok)
	}

	Unpin("anthropic", "claude-3-haiku")
	if got := MustLookup("anthropic", "claude-3-haiku").Pricing.InputPerMTok; got != dearer.Pricing.InputPerMTok {
		t.Errorf("after unpin, resolved %v, want the newest %v", got, dearer.Pricing.InputPerMTok)
	}
}

// Pricing must be per-variant. gpt-4o-mini costs a fraction of gpt-4o, so a
// prefix match that priced it as gpt-4o would over-charge a budget by ~16x.
func TestMiniVariantsCarryTheirOwnPricing(t *testing.T) {
	full := MustLookup("openai", "gpt-4o-2024-08-06")
	mini := MustLookup("openai", "gpt-4o-mini")
	if mini.Pricing.InputPerMTok >= full.Pricing.InputPerMTok {
		t.Errorf("gpt-4o-mini input rate %v is not below gpt-4o's %v: the mini row is not matching",
			mini.Pricing.InputPerMTok, full.Pricing.InputPerMTok)
	}
}

// A local model is free, which a budget must be able to distinguish from
// unpriced: unpriced refuses to run, free runs at zero cost.
func TestLocalModelsArePricedFreeNotUnpriced(t *testing.T) {
	caps := MustLookup("ollama", "qwen3:4b")
	if caps.Pricing.IsZero() {
		t.Error("a local model must be priced (at zero), not unpriced")
	}
	cost := caps.Pricing.Cost(types.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if cost != 0 {
		t.Errorf("local cost = %s, want 0", cost)
	}
}

// Server-tool support is per-family and must not be assumed from the provider:
// ollama has none, and the flag and the enumerated kinds must agree.
func TestServerToolDeclarationsAgreeWithFlags(t *testing.T) {
	for _, tt := range []struct {
		provider, model string
		wantWebSearch   bool
	}{
		{"anthropic", "claude-sonnet-4-5", true},
		{"google", "gemini-2.5-flash", true},
		{"ollama", "qwen3", false},
		{"anthropic", "claude-3-haiku", false},
	} {
		caps := MustLookup(tt.provider, tt.model)
		if got := caps.Supports(types.CapWebSearch); got != tt.wantWebSearch {
			t.Errorf("%s/%s CapWebSearch = %v, want %v", tt.provider, tt.model, got, tt.wantWebSearch)
		}
		if got := caps.SupportsServerTool(types.ServerToolWebSearch); got != tt.wantWebSearch {
			t.Errorf("%s/%s ServerTools web_search = %v, want it to agree with the flag", tt.provider, tt.model, got)
		}
	}
}

func TestMinReasoningBudgetIsDeclaredWhereItIsEnforced(t *testing.T) {
	caps := MustLookup("anthropic", "claude-sonnet-4-5")
	if caps.MinReasoningBudget != 1024 {
		t.Errorf("MinReasoningBudget = %d, want 1024: below it Anthropic rejects the request", caps.MinReasoningBudget)
	}
}

func TestProvidersAndFamiliesAreListed(t *testing.T) {
	provs := Providers()
	want := map[string]bool{"anthropic": true, "openai": true, "google": true, "ollama": true}
	for _, p := range provs {
		delete(want, p)
	}
	if len(want) > 0 {
		t.Errorf("Providers() = %v, missing %v", provs, want)
	}
	if len(Families("anthropic")) == 0 {
		t.Error("Families(anthropic) must not be empty")
	}
}
