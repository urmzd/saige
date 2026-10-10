package provider

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/otel"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/provider/fallback"
	"github.com/urmzd/saige/agent/provider/internal/wrappertest"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/provider/split"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func cases(t *testing.T) []wrappertest.Case {
	mem := func() types.Cache[cache.CachedResponse] { return memcache.New[cache.CachedResponse]() }
	return []wrappertest.Case{
		{Name: "retry", Build: func(p types.Provider) types.Provider { return must.Get(retry.New(p, retry.DefaultConfig())) }},
		{Name: "convert", Build: func(p types.Provider) types.Provider { return must.Get(convert.New(p, convert.Config{})) }},
		{Name: "fallback", Multi: true, Build: func(p types.Provider) types.Provider { return must.Get(fallback.Of(p)) }},
		{Name: "cache", Build: func(p types.Provider) types.Provider {
			return must.Get(cache.New(p, cache.Config{Cache: mem()}))
		}},
		{Name: "router session", Multi: true, SharedClose: true, SharedSessions: true, Profiles: true, RetargetTo: types.ProfileTarget("only"),
			Build: func(p types.Provider) types.Provider {
				r, err := router.New(router.Config{Profiles: []router.Profile{{ID: "only", Provider: p}}})
				if err != nil {
					t.Fatal(err)
				}
				return r.Session()
			}},
		{Name: "split", Multi: true, Build: func(p types.Provider) types.Provider {
			s, err := split.New(split.Config{Experiment: "exp", Arms: []split.Arm{{Label: "only", Weight: 1, Provider: p}}})
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
		{Name: "privacy", Build: func(p types.Provider) types.Provider {
			return must.Get(privacy.New(p, privacy.Config{Vault: privacy.NewVault(nil)}))
		}},
		{Name: "tracing", Build: func(p types.Provider) types.Provider {
			return must.Get(otel.NewTracedProvider(p, noop.NewTracerProvider().Tracer("test")))
		}},
		{Name: "no close", SharedClose: true, Build: wrapper.NoClose},
		{Name: "stacked", Build: func(p types.Provider) types.Provider {
			return must.Get(retry.New(must.Get(convert.New(must.Get(cache.New(p, cache.Config{Cache: mem()})), convert.Config{})), retry.DefaultConfig()))
		}},
	}
}

// Every built-in decorator forwards every optional interface the agent loop
// finds by a type assertion, and each reaches the provider it wraps.
func TestWrappersForwardEveryOptionalInterface(t *testing.T) {
	wrappertest.Run(t, cases(t))
}

// A NoClose view does not close the shared provider.
func TestNoCloseKeepsTheProviderOpen(t *testing.T) {
	inner := wrappertest.NewFull()
	if err := types.CloseProvider(context.Background(), wrapper.NoClose(inner)); err != nil || inner.C.Closed.Load() != 0 {
		t.Fatalf("NoClose closed the provider: %d, %v", inner.C.Closed.Load(), err)
	}
}

type wrapped struct {
	name  string
	build func(inner types.Provider) types.Provider
}

func wrappers(t *testing.T) []wrapped {
	var out []wrapped
	for _, c := range cases(t) {
		out = append(out, wrapped{c.Name, c.Build})
	}
	return out
}

// bare implements only types.Provider.
type bare struct{}

func (bare) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func TestWrappersRejectOptionsTheInnerProviderCannotReceive(t *testing.T) {
	choice := types.ToolChoice{Mode: types.ToolChoiceRequired}
	opts := types.RequestOptions{ToolChoice: &choice}
	for _, w := range wrappers(t) {
		if w.name == "router session" {
			continue // a router rejects ineligible profiles before calling; see its tests
		}
		t.Run(w.name, func(t *testing.T) {
			ch, err := w.build(bare{}).(types.OptionsProvider).Stream(context.Background(), types.Request{Tools: []types.ToolDef{{Name: "t"}}, Options: &opts})
			if err == nil {
				for d := range ch {
					if e, ok := d.(types.ErrorDelta); ok {
						err = e.Error
					}
				}
			}
			if !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("options were dropped: err = %v", err)
			}
		})
	}
}

func TestInnermostFollowsSingleWrappers(t *testing.T) {
	inner := wrappertest.NewFull()
	stack := must.Get(retry.New(must.Get(cache.New(inner, cache.Config{Cache: memcache.New[cache.CachedResponse]()})), retry.DefaultConfig()))
	if wrapper.Innermost(stack) != types.Provider(inner) {
		t.Fatal("Innermost did not reach the adapter")
	}
	if got := wrapper.Members(must.Get(fallback.Of(inner, bare{}))); len(got) != 2 {
		t.Fatalf("Members = %d", len(got))
	}
}

func TestGenerateKeepsUsageAndReportsTruncation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		finish        []string
		wantTruncated bool
	}{
		{"complete", []string{"end_turn"}, false},
		{"cut off at max tokens", []string{"max_tokens"}, true},
		{"cut off at length", []string{"length"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := wrappertest.NewFull()
			inner.Finish = tc.finish
			g := AsGenerator(must.Get(retry.New(inner, retry.DefaultConfig())))
			res, err := g.GenerateWithUsage(context.Background(), "prompt")
			if errors.Is(err, types.ErrResponseTruncated) != tc.wantTruncated {
				t.Fatalf("err = %v", err)
			}
			if res.Text != "full" || res.Usage.PromptTokens != 3 || res.Usage.CompletionTokens != 2 {
				t.Fatalf("result = %+v", res)
			}
			text, err := g.Generate(context.Background(), "prompt")
			if tc.wantTruncated {
				var te *types.ResponseTruncatedError
				if !errors.As(err, &te) || te.OutputTokens != 2 || text != "" {
					t.Fatalf("Generate = %q, %v", text, err)
				}
			} else if err != nil || text != "full" {
				t.Fatalf("Generate = %q, %v", text, err)
			}
		})
	}
}

// declaresOnly reports tool choice in its capabilities, as the built-in
// adapters do, but does not implement types.OptionsProvider.
type declaresOnly struct{ bare }

func (declaresOnly) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Known: true, Caps: map[types.Capability]bool{
		types.CapTools: true, types.CapToolChoice: true, types.CapParallelToolControl: true,
	}}
}

// noneRequest mirrors how the agent loop forbids tool use for one turn: it
// sends a ToolChoiceNone option only to a provider that accepts options and
// declares tool choice, and otherwise withholds the tools.
func noneRequest(p types.Provider, tools []types.ToolDef) ([]types.ToolDef, *types.RequestOptions) {
	caps, known := types.ProviderCapabilities(p)
	if _, accepts := p.(types.OptionsProvider); accepts && known && caps.Supports(types.CapToolChoice) {
		return tools, &types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNone}}
	}
	return nil, nil
}

func TestWrappersDropOptionCapabilitiesTheInnerProviderCannotReceive(t *testing.T) {
	tools := []types.ToolDef{{Name: "t"}}
	for _, w := range wrappers(t) {
		t.Run(w.name, func(t *testing.T) {
			p := w.build(declaresOnly{})
			caps, _ := types.ProviderCapabilities(p)
			if caps.Supports(types.CapToolChoice) || caps.Supports(types.CapParallelToolControl) {
				t.Fatalf("%s declares option-only capabilities it cannot deliver: %v", w.name, caps.List())
			}
			if !caps.Supports(types.CapTools) {
				t.Fatalf("%s dropped tools: %v", w.name, caps.List())
			}
			if err := caps.ValidateToolChoice(&types.ToolChoice{Mode: types.ToolChoiceRequired}, tools); !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("a required tool choice validated: %v", err)
			}

			sent, opts := noneRequest(p, tools)
			if opts != nil || sent != nil {
				t.Fatalf("forbidding tools sent options %+v and tools %v", opts, sent)
			}
			ch, err := p.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Tools: sent})
			if err != nil {
				t.Fatal(err)
			}
			for d := range ch {
				if e, ok := d.(types.ErrorDelta); ok {
					t.Fatalf("tool-free call failed: %v", e.Error)
				}
			}
		})
	}
	// A member that can receive options keeps the capability for the whole
	// chain, because an options call is routed to that member.
	mixed := must.Get(fallback.Of(declaresOnly{}, wrappertest.NewFull()))
	if caps, _ := types.ProviderCapabilities(mixed); !caps.Supports(types.CapToolChoice) {
		t.Fatalf("mixed chain lost tool choice: %v", caps.List())
	}
}
