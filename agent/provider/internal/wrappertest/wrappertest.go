// Package wrappertest is the conformance suite for provider decorators: it
// proves that a decorator forwards every optional provider interface the
// agent loop finds by a type assertion, and that each one reaches the
// provider it wraps.
package wrappertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/provider/wrapper"
	"github.com/urmzd/saige/agent/types"
)

// LockReporter is a member-specific interface: decorators must not forward
// it, but wrapper.As must find it through them.
type LockReporter interface{ RouteLocks() []string }

// Counters are shared by a Full provider and every variant it returns.
type Counters struct {
	Closed, Options, Schemas, Sessions atomic.Int32
}

// Full implements every optional provider interface.
type Full struct {
	C      *Counters
	model  string
	Finish []string
}

// NewFull returns a Full provider serving "full-model".
func NewFull() *Full { return &Full{C: &Counters{}, model: "full-model"} }

func (p *Full) stream() <-chan types.Delta {
	ch := make(chan types.Delta, 4)
	ch <- types.PartDelta{Index: 0, Text: "full"}
	ch <- types.UsageDelta{PromptTokens: 3, CompletionTokens: 2, FinishReasons: p.Finish}
	close(ch)
	return ch
}

// Stream implements types.Provider and counts schemas and options.
func (p *Full) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Options != nil {
		p.C.Options.Add(1)
	}
	if req.Schema != nil {
		p.C.Schemas.Add(1)
	}
	return p.stream(), nil
}

// SupportsOptions implements types.OptionsProvider.
func (p *Full) SupportsOptions() bool { return true }

// SupportsSchema implements types.StructuredOutputProvider.
func (p *Full) SupportsSchema() bool { return true }

// Name implements types.NamedProvider.
func (p *Full) Name() string { return "full" }

// Model implements types.ModelProvider.
func (p *Full) Model() string { return p.model }

// WithTarget implements types.TargetSwitcher for model targets.
func (p *Full) WithTarget(t types.Target) (types.Provider, error) {
	m, err := types.TargetModel(t, p.Name())
	if err != nil {
		return nil, err
	}
	c := *p
	c.model = string(m)
	return &c, nil
}

// NewSession implements types.SessionProvider and counts sessions.
func (p *Full) NewSession() types.Provider {
	p.C.Sessions.Add(1)
	c := *p
	return &c
}

// Close implements types.Closer and counts calls.
func (p *Full) Close(context.Context) error { p.C.Closed.Add(1); return nil }

// RouteLocks implements LockReporter.
func (p *Full) RouteLocks() []string { return []string{"context_cache"} }

// EffectiveOptions implements types.OptionsReporter: a seed of 5.
func (p *Full) EffectiveOptions() types.RequestOptions {
	seed := int64(5)
	return types.RequestOptions{Seed: &seed}
}

// Capabilities implements types.CapabilityReporter.
func (p *Full) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "full", Model: p.model, Known: true, ContextWindow: 1000,
		Caps: map[types.Capability]bool{types.CapTools: true, types.CapStructuredOutput: true, types.CapToolChoice: true, types.CapSeed: true}}
}

// Case is one decorator under test.
type Case struct {
	Name  string
	Build func(inner types.Provider) types.Provider
	// Multi marks a decorator over several providers (fallback, router,
	// split): it reports no single member's effective options.
	Multi bool
	// SharedClose marks a decorator whose Close does not reach the inner
	// provider because others share it (a NoClose view, a router session).
	SharedClose bool
	// SharedSessions marks a decorator whose sessions share the inner
	// provider rather than isolate it (a router session: its profiles are
	// the router's).
	SharedSessions bool
	// Profiles marks a decorator that defines profiles and presets (a
	// router): a model target it does not define fails, and RetargetTo
	// is a target it does define.
	Profiles   bool
	RetargetTo types.Target
}

// forwarded lists the optional interfaces the agent loop finds by a direct
// type assertion. Adding an optional interface means adding it here.
var forwarded = []struct {
	name   string
	single bool // only single-provider decorators must implement it
	has    func(types.Provider) bool
}{
	{"NamedProvider", false, func(p types.Provider) bool { _, ok := p.(types.NamedProvider); return ok }},
	{"ModelProvider", false, func(p types.Provider) bool { _, ok := p.(types.ModelProvider); return ok }},
	{"TargetSwitcher", false, func(p types.Provider) bool { _, ok := p.(types.TargetSwitcher); return ok }},
	{"CapabilityReporter", false, func(p types.Provider) bool { _, ok := p.(types.CapabilityReporter); return ok }},
	{"StructuredOutputProvider", false, func(p types.Provider) bool { _, ok := p.(types.StructuredOutputProvider); return ok }},
	{"OptionsProvider", false, func(p types.Provider) bool { _, ok := p.(types.OptionsProvider); return ok }},
	{"SessionProvider", false, func(p types.Provider) bool { _, ok := p.(types.SessionProvider); return ok }},
	{"OptionsReporter", true, func(p types.Provider) bool { _, ok := p.(types.OptionsReporter); return ok }},
	{"Closer", true, func(p types.Provider) bool { _, ok := p.(types.Closer); return ok }},
	{"Wrapper", false, func(p types.Provider) bool {
		_, one := p.(wrapper.Wrapper)
		_, many := p.(wrapper.MultiWrapper)
		return one || many
	}},
}

func drain(t *testing.T, ch <-chan types.Delta, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatalf("stream error: %v", e.Error)
		}
	}
}

// Run checks every case: each optional interface is present and reaches
// the Full provider it wraps.
func Run(t *testing.T, cases []Case) {
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) { check(t, c) })
	}
}

func check(t *testing.T, c Case) {
	inner := NewFull()
	p := c.Build(inner)
	for _, iface := range forwarded {
		if iface.single && c.Multi {
			continue
		}
		if !iface.has(p) {
			t.Errorf("hides %s", iface.name)
		}
	}

	if lr, ok := wrapper.As[LockReporter](p); !ok || len(lr.RouteLocks()) != 1 {
		t.Error("hides a member-specific interface from wrapper.As")
	}
	if found, ok := wrapper.As[*Full](p); !ok || found != inner {
		t.Error("wrapper.As did not reach the inner provider")
	}
	if !strings.Contains(types.NameOf(p), "full") && !c.Multi {
		t.Errorf("Name %q does not name the inner provider", types.NameOf(p))
	}
	if !c.Profiles && types.ProviderModel(p) != "full-model" {
		t.Errorf("Model = %q, want the inner model", types.ProviderModel(p))
	}
	info := wrapper.Describe(p)
	if info.Vendor != "full" && !c.Multi || !info.CapabilitiesKnown || !info.AcceptsSchema || !info.AcceptsOptions || !info.Retargetable {
		t.Errorf("Describe = %+v", info)
	}

	checkForwarding(t, c, p, inner)
	checkRetarget(t, c, p)
	checkLifecycle(t, c, p, inner)
}

// checkForwarding proves capabilities, options and schemas reach inner.
func checkForwarding(t *testing.T, c Case, p types.Provider, inner *Full) {
	t.Helper()
	caps, _ := types.ProviderCapabilities(p)
	if !caps.SupportsAll(types.CapTools, types.CapStructuredOutput, types.CapToolChoice) {
		t.Errorf("capabilities = %v", caps.List())
	}
	if !c.Multi {
		o, ok := types.ProviderEffectiveOptions(p)
		if !ok || o.Seed == nil || *o.Seed != 5 {
			t.Errorf("effective options %+v, %v", o, ok)
		}
	}

	choice := types.ToolChoice{Mode: types.ToolChoiceRequired}
	msgs := []types.Message{types.UserMsg(types.Text("hi"))}
	ch, err := p.Stream(context.Background(), types.Request{Messages: msgs, Tools: []types.ToolDef{{Name: "t"}}, Options: &types.RequestOptions{ToolChoice: &choice}})
	drain(t, ch, err)
	if inner.C.Options.Load() != 1 {
		t.Error("did not forward request options")
	}
	ch, err = p.Stream(context.Background(), types.Request{Messages: msgs, Schema: &types.ParameterSchema{Type: "object"}})
	drain(t, ch, err)
	if inner.C.Schemas.Load() != 1 {
		t.Error("did not forward the schema")
	}

}

// checkRetarget proves a re-targeted provider keeps the decorator and
// reaches the inner provider.
func checkRetarget(t *testing.T, c Case, p types.Provider) {
	t.Helper()
	target := types.ModelTarget("other-model")
	if c.Profiles {
		target = c.RetargetTo
	}
	switched, err := types.ProviderWithTarget(p, target)
	if err != nil {
		t.Fatalf("WithTarget(%s): %v", target, err)
	}
	if typeName(switched) != typeName(p) {
		t.Errorf("WithTarget returned %T, want the decorator %T", switched, p)
	}
	if !c.Profiles {
		if f, ok := wrapper.As[*Full](switched); !ok || f.Model() != "other-model" {
			t.Error("WithTarget did not re-target the inner provider")
		}
		if _, err := types.ProviderWithTarget(p, types.ProfileTarget("nope")); !errors.Is(err, types.ErrUnknownTarget) {
			t.Errorf("an undefined profile target: %v", err)
		}
	} else if _, err := types.ProviderWithTarget(p, types.ModelTarget("undefined-model")); !errors.Is(err, types.ErrUnknownTarget) {
		t.Errorf("an undefined model target: %v", err)
	}

}

// checkLifecycle proves sessions and Close reach the inner provider.
func checkLifecycle(t *testing.T, c Case, p types.Provider, inner *Full) {
	t.Helper()
	// A session keeps the decorator and isolates the inner provider.
	sess := types.NewProviderSession(p)
	if typeName(sess) != typeName(p) {
		t.Errorf("NewSession returned %T, want the decorator %T", sess, p)
	}
	if !c.SharedSessions && inner.C.Sessions.Load() != 1 {
		t.Errorf("NewSession reached the inner provider %d times", inner.C.Sessions.Load())
	}

	if !c.SharedClose {
		if err := types.CloseProvider(context.Background(), p); err != nil || inner.C.Closed.Load() != 1 {
			t.Errorf("Close reached the inner provider %d times, err %v", inner.C.Closed.Load(), err)
		}
	}
}

func typeName(p types.Provider) string { return fmt.Sprintf("%T", p) }
