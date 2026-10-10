package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/otel"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/provider/fallback"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/provider/split"
	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// fullProvider implements every optional provider interface, plus one that
// only describes this member (router.LockReporter).
type fullProvider struct {
	closed   *atomic.Int32
	optCalls *atomic.Int32
	schemas  *atomic.Int32
	finish   []string
}

func newFull() *fullProvider {
	return &fullProvider{closed: &atomic.Int32{}, optCalls: &atomic.Int32{}, schemas: &atomic.Int32{}}
}

func (p *fullProvider) stream() <-chan types.Delta {
	ch := make(chan types.Delta, 4)
	ch <- types.PartDelta{Index: 0, Text: "full"}
	ch <- types.UsageDelta{PromptTokens: 3, CompletionTokens: 2, FinishReasons: p.finish}
	close(ch)
	return ch
}

func (p *fullProvider) chatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	return p.stream(), nil
}

// Stream implements types.Provider.
func (p *fullProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		return p.chatStreamWithOptions(ctx, req.Messages, req.Tools, *req.Options)
	}
	if req.Schema != nil {
		return p.chatStreamWithSchema(ctx, req.Messages, req.Tools, req.Schema)
	}
	return p.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsOptions implements types.OptionsProvider.
func (p *fullProvider) SupportsOptions() bool { return true }

// SupportsSchema implements types.StructuredOutputProvider.
func (p *fullProvider) SupportsSchema() bool { return true }
func (p *fullProvider) chatStreamWithSchema(context.Context, []types.Message, []types.ToolDef, *types.ParameterSchema) (<-chan types.Delta, error) {
	p.schemas.Add(1)
	return p.stream(), nil
}
func (p *fullProvider) chatStreamWithOptions(context.Context, []types.Message, []types.ToolDef, types.RequestOptions) (<-chan types.Delta, error) {
	p.optCalls.Add(1)
	return p.stream(), nil
}
func (p *fullProvider) Name() string                    { return "full" }
func (p *fullProvider) Model() string                   { return "full-model" }
func (p *fullProvider) WithModel(string) types.Provider { return p }
func (p *fullProvider) NewSession() types.Provider      { return p }
func (p *fullProvider) Close() error                    { p.closed.Add(1); return nil }
func (p *fullProvider) RouteLocks() []string            { return []string{router.LockContextCache} }
func (p *fullProvider) EffectiveOptions() types.RequestOptions {
	seed := int64(5)
	return types.RequestOptions{Seed: &seed}
}
func (p *fullProvider) ContentSupport() types.ContentSupport {
	return types.ContentSupport{NativeTypes: map[types.MediaType]bool{types.MediaPNG: true}}
}
func (p *fullProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "full", Model: "full-model", Known: true, ContextWindow: 1000,
		Caps:  map[types.Capability]bool{types.CapTools: true, types.CapStructuredOutput: true, types.CapToolChoice: true, types.CapSeed: true},
		Media: types.ContentSupport{NativeTypes: map[types.MediaType]bool{types.MediaPNG: true}}}
}

// forwarded lists the optional interfaces the agent loop finds by a direct
// type assertion. Every built-in decorator must implement each one itself.
// Adding an optional interface means adding it here.
var forwarded = []struct {
	name string
	has  func(types.Provider) bool
}{
	{"NamedProvider", func(p types.Provider) bool { _, ok := p.(types.NamedProvider); return ok }},
	{"ModelProvider", func(p types.Provider) bool { _, ok := p.(types.ModelProvider); return ok }},
	// A model switch reaches the inner provider through ModelSwitcher, or
	// through TargetSwitcher, which the router implements in its place.
	{"ModelSwitcher", func(p types.Provider) bool {
		_, ms := p.(types.ModelSwitcher)
		_, ts := p.(types.TargetSwitcher)
		return ms || ts
	}},
	{"TargetSwitcher", func(p types.Provider) bool { _, ok := p.(types.TargetSwitcher); return ok }},
	{"CapabilityReporter", func(p types.Provider) bool { _, ok := p.(types.CapabilityReporter); return ok }},
	{"StructuredOutputProvider", func(p types.Provider) bool { _, ok := p.(types.StructuredOutputProvider); return ok }},
	{"OptionsProvider", func(p types.Provider) bool { _, ok := p.(types.OptionsProvider); return ok }},
	{"SessionProvider", func(p types.Provider) bool { _, ok := p.(types.SessionProvider); return ok }},
}

type wrapped struct {
	name  string
	build func(inner types.Provider) types.Provider
	// closes reports whether Close on the wrapper reaches the inner provider.
	// A router session shares its profiles with other sessions, so only the
	// Router closes them.
	closes bool
}

func wrappers(t *testing.T) []wrapped {
	return []wrapped{
		{"retry", func(p types.Provider) types.Provider { return retry.New(p, retry.DefaultConfig()) }, true},
		{"convert", func(p types.Provider) types.Provider { return convert.New(p, types.ConversionPolicy{}) }, true},
		{"fallback", func(p types.Provider) types.Provider { return fallback.New(p) }, true},
		{"cache", func(p types.Provider) types.Provider {
			return cache.New(p, cache.Config{Cache: memcache.New[cache.CachedResponse]()})
		}, true},
		{"router session", func(p types.Provider) types.Provider {
			r, err := router.New(router.Config{Profiles: []router.Profile{{ID: "only", Provider: p}}})
			if err != nil {
				t.Fatal(err)
			}
			return r.Session()
		}, false},
		{"split", func(p types.Provider) types.Provider {
			s, err := split.New(split.Config{Experiment: "exp", Arms: []split.Arm{{Label: "only", Weight: 1, Provider: p}}})
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, true},
		{"privacy", func(p types.Provider) types.Provider { return privacy.NewProvider(p, privacy.NewVault(nil)) }, true},
		{"tracing", func(p types.Provider) types.Provider {
			return otel.NewTracedProvider(p, noop.NewTracerProvider().Tracer("test"))
		}, true},
		{"stacked", func(p types.Provider) types.Provider {
			return retry.New(fallback.New(cache.New(p, cache.Config{Cache: memcache.New[cache.CachedResponse]()})), retry.DefaultConfig())
		}, true},
	}
}

func TestWrappersKeepOptionalInterfaces(t *testing.T) {
	for _, w := range wrappers(t) {
		t.Run(w.name, func(t *testing.T) {
			inner := newFull()
			p := w.build(inner)
			for _, iface := range forwarded {
				if !iface.has(p) {
					t.Errorf("%s hides %s", w.name, iface.name)
				}
			}
			if lr, ok := wrapper.As[router.LockReporter](p); !ok || len(lr.RouteLocks()) != 1 {
				t.Errorf("%s hides a member-specific interface from wrapper.As", w.name)
			}
			if found, ok := wrapper.As[*fullProvider](p); !ok || found != inner {
				t.Errorf("%s: wrapper.As did not reach the inner provider", w.name)
			}

			caps, _ := types.ProviderCapabilities(p)
			if !caps.SupportsAll(types.CapTools, types.CapStructuredOutput, types.CapToolChoice) {
				t.Errorf("%s capabilities = %v", w.name, caps.List())
			}

			choice := types.ToolChoice{Mode: types.ToolChoiceRequired}
			ch, err := p.(types.OptionsProvider).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Tools: []types.ToolDef{{Name: "t"}}, Options: &types.RequestOptions{ToolChoice: &choice}})
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			if inner.optCalls.Load() != 1 {
				t.Errorf("%s did not forward request options", w.name)
			}

			ch, err = p.(types.StructuredOutputProvider).Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}, Schema: &types.ParameterSchema{Type: "object"}})
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			if inner.schemas.Load() != 1 {
				t.Errorf("%s did not forward the schema", w.name)
			}

			if w.closes {
				if err := types.CloseProvider(p); err != nil || inner.closed.Load() != 1 {
					t.Errorf("%s Close reached the inner provider %d times, err %v", w.name, inner.closed.Load(), err)
				}
			}
		})
	}
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

// Single-provider decorators report the options their inner provider sends,
// so a router validates and records what reaches the wire.
func TestSingleWrappersForwardEffectiveOptions(t *testing.T) {
	for _, w := range wrappers(t) {
		switch w.name {
		case "retry", "convert", "cache", "privacy", "tracing":
		default:
			continue
		}
		o, ok := types.ProviderEffectiveOptions(w.build(newFull()))
		if !ok || o.Seed == nil || *o.Seed != 5 {
			t.Errorf("%s: effective options %+v, %v", w.name, o, ok)
		}
	}
}

func TestInnermostFollowsSingleWrappers(t *testing.T) {
	inner := newFull()
	stack := retry.New(cache.New(inner, cache.Config{}), retry.DefaultConfig())
	if wrapper.Innermost(stack) != types.Provider(inner) {
		t.Fatal("Innermost did not reach the adapter")
	}
	if got := wrapper.Members(fallback.New(inner, bare{})); len(got) != 2 {
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
			inner := newFull()
			inner.finish = tc.finish
			g := AsGenerator(retry.New(inner, retry.DefaultConfig()))
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
	mixed := fallback.New(declaresOnly{}, newFull())
	if caps, _ := types.ProviderCapabilities(mixed); !caps.Supports(types.CapToolChoice) {
		t.Fatalf("mixed chain lost tool choice: %v", caps.List())
	}
}
