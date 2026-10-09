package catalog

import (
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestInferProvider(t *testing.T) {
	for _, tt := range []struct {
		model, want string
		ok          bool
	}{
		{"claude-sonnet-4-5-20250514", "anthropic", true},
		{"gpt-4o-mini", "openai", true},
		{"o3-mini", "openai", true},
		{"gemini-2.5-flash", "google", true},
		{"text-embedding-004", "google", true},
		{"text-embedding-3-small", "openai", true},
		{"qwen3:4b", "ollama", true},
		{"hf.co/someone/qwen3:latest", "ollama", true},
		{"openai/my-finetune", "openai", true},
		{"Anthropic/claude-x", "anthropic", true},
		{"totally-unknown-model", "", false},
	} {
		got, ok := InferProvider(tt.model)
		if got != tt.want || ok != tt.ok {
			t.Errorf("InferProvider(%q) = %q, %v; want %q, %v", tt.model, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFits(t *testing.T) {
	caps := types.ModelCapabilities{Provider: "p", Model: "m", ContextWindow: 1000, MaxOutputTokens: 200}
	for _, tt := range []struct {
		name          string
		caps          types.ModelCapabilities
		input, output int
		fits          bool
	}{
		{"within limits", caps, 700, 200, true},
		{"exactly the window", caps, 800, 200, true},
		{"input plus output too large", caps, 900, 200, false},
		{"output over cap", caps, 10, 201, false},
		{"input alone too large", caps, 1001, 0, false},
		{"negative counts", caps, -1, 0, false},
		{"undeclared limits never reject", types.ModelCapabilities{}, 1 << 30, 1 << 20, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Fits(tt.caps, tt.input, tt.output)
			if (err == nil) != tt.fits {
				t.Fatalf("Fits = %v, want fits=%v", err, tt.fits)
			}
			if err != nil && (!errors.Is(err, types.ErrContextLength) || !types.IsContextLength(err) || types.IsTransient(err)) {
				t.Fatalf("error %v must be a permanent context-length error", err)
			}
		})
	}
}

func TestDescribeTierAndSuccessor(t *testing.T) {
	for _, tt := range []struct {
		provider, model string
		tier            Tier
		successor       string
	}{
		{"anthropic", "claude-opus-5-5", TierFrontier, ""},
		{"anthropic", "claude-haiku-5-5", TierEconomy, ""},
		{"anthropic", "claude-opus-4-1", TierFrontier, "claude-opus-5-5"},
		{"anthropic", "claude-haiku-4-5", TierEconomy, "claude-haiku-5-5"},
		{"anthropic", "claude-3-5-sonnet-20241022", TierStandard, "claude-haiku-5-5"},
		{"anthropic", "claude-3-opus-20240229", TierFrontier, "claude-haiku-5-5"},
		{"openai", "gpt-6-luna", TierEconomy, ""},
		{"openai", "gpt-6-sol", TierStandard, "gpt-6.1-sol"},
		{"openai", "gpt-4o-mini", TierEconomy, "gpt-6-luna"},
		{"openai", "gpt-4.1-nano", TierEconomy, "gpt-6-luna"},
		{"openai", "gpt-5-mini", TierEconomy, "gpt-6-luna"},
		{"openai", "o1-preview", TierFrontier, "gpt-6.1-sol"},
		{"google", "gemini-2.0-flash", TierEconomy, "gemini-3.1-flash-lite"},
		{"google", "gemini-3-flash-preview", TierStandard, "gemini-3.1-flash-lite"},
		{"google", "gemini-2.5-pro", TierFrontier, "gemini-3.8-flash"},
		{"google", "gemini-3.1-flash-lite", TierEconomy, ""},
	} {
		e, ok := Describe(tt.provider, tt.model)
		if !ok || e.Tier != tt.tier {
			t.Errorf("Describe(%s, %s) = %+v, %v; want tier %q", tt.provider, tt.model, e.Tier, ok, tt.tier)
		}
		got, ok := Successor(tt.provider, tt.model)
		if got != tt.successor || ok != (tt.successor != "") {
			t.Errorf("Successor(%s, %s) = %q, %v; want %q", tt.provider, tt.model, got, ok, tt.successor)
		}
	}
	if _, ok := Describe("anthropic", "claude-unreleased"); ok {
		t.Error("a baseline-only model must not describe a row")
	}
}

func TestSuccessorFollowsChainAndStopsOnCycle(t *testing.T) {
	Register(Entry{Provider: "chain-test", Prefix: "chain-a", SupersededBy: "chain-b"})
	Register(Entry{Provider: "chain-test", Prefix: "chain-b", SupersededBy: "chain-c"})
	Register(Entry{Provider: "chain-test", Prefix: "chain-c"})
	if got, _ := Successor("chain-test", "chain-a-1"); got != "chain-c" {
		t.Errorf("Successor = %q, want chain-c", got)
	}
	Register(Entry{Provider: "cycle-test", Prefix: "cycle-x", SupersededBy: "cycle-y"})
	Register(Entry{Provider: "cycle-test", Prefix: "cycle-y", SupersededBy: "cycle-x"})
	if got, ok := Successor("cycle-test", "cycle-x"); !ok || got != "cycle-y" {
		t.Errorf("Successor on a cycle = %q, %v; want cycle-y", got, ok)
	}
}

func TestServerToolFee(t *testing.T) {
	fee, ok := ServerToolFee("anthropic", "claude-sonnet-4-5", types.ServerToolWebSearch)
	if !ok || fee.PerUse <= 0 || fee.AsOf == "" {
		t.Fatalf("anthropic web search fee = %+v, %v; want a dated per-use price", fee, ok)
	}
	if total, ok := fee.Reserve(5); !ok || total != fee.PerUse*5 {
		t.Errorf("Reserve(5) = %v, %v", total, ok)
	}
	if _, ok := fee.Reserve(0); ok {
		t.Error("an uncapped tool cannot be reserved")
	}
	if _, ok := ServerToolFee("anthropic", "claude-sonnet-4-5", types.ServerToolCodeExecution); ok {
		t.Error("code execution is billed by container time and must stay unpriced per use")
	}
	if _, ok := ServerToolFee("google", "gemini-2.5-flash", types.ServerToolWebSearch); ok {
		t.Error("an unpriced fee must report false, not zero")
	}

	// Returned maps are copies.
	e, _ := Describe("anthropic", "claude-sonnet-4-5")
	e.ServerToolFees[types.ServerToolWebSearch] = Fee{PerUse: 99}
	if again, _ := ServerToolFee("anthropic", "claude-sonnet-4-5", types.ServerToolWebSearch); again.PerUse == 99 {
		t.Error("mutating a described entry changed the catalog")
	}
}

func TestReconcile(t *testing.T) {
	Register(Entry{Provider: "reconcile-test", Prefix: "alpha", Tier: TierStandard,
		Caps: types.ModelCapabilities{ContextWindow: 1000}})
	Register(Entry{Provider: "reconcile-test", Prefix: "alpha-exact"})
	Register(Entry{Provider: "reconcile-test", Prefix: "beta", SupersededBy: "alpha"})
	Register(Entry{Provider: "reconcile-test", Prefix: "gamma"})

	r := Reconcile("reconcile-test", []RemoteModel{
		{ID: "zeta-1"},
		{ID: "alpha-exact"},
		{ID: "alpha-2", ContextWindow: 2000},
		{ID: "beta-1"},
	})
	got := map[string]ReconciledModel{}
	var order []string
	for _, m := range r.Models {
		got[m.Remote.ID] = m
		order = append(order, m.Remote.ID)
	}
	if !slices.IsSorted(order) {
		t.Errorf("models not sorted: %v", order)
	}
	for _, tt := range []struct {
		id     string
		status Status
		family string
	}{
		{"zeta-1", StatusUndeclared, ""},
		{"alpha-exact", StatusDeclared, "alpha-exact"},
		{"alpha-2", StatusInferred, "alpha"},
		{"beta-1", StatusInferred, "beta"},
	} {
		if m := got[tt.id]; m.Status != tt.status || m.Family != tt.family {
			t.Errorf("%s = %s/%s, want %s/%s", tt.id, m.Status, m.Family, tt.status, tt.family)
		}
	}
	if len(got["alpha-2"].Drift) != 1 || got["alpha-2"].Tier != TierStandard {
		t.Errorf("alpha-2 = %+v, want one drift note and the row tier", got["alpha-2"])
	}
	if got["beta-1"].SupersededBy != "alpha" {
		t.Errorf("beta-1 successor = %q", got["beta-1"].SupersededBy)
	}
	if !slices.Equal(r.Unserved, []string{"gamma"}) {
		t.Errorf("Unserved = %v, want [gamma]", r.Unserved)
	}
	if !slices.Equal(r.Undeclared(), []string{"zeta-1"}) {
		t.Errorf("Undeclared = %v", r.Undeclared())
	}
}
