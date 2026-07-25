package types

import (
	"context"
	"testing"
)

// capProvider is a Provider that reports a fixed capability set.
type capProvider struct {
	caps ModelCapabilities
}

func (c *capProvider) ChatStream(context.Context, []Message, []ToolDef) (<-chan Delta, error) {
	ch := make(chan Delta)
	close(ch)
	return ch, nil
}
func (c *capProvider) Capabilities() ModelCapabilities { return c.caps }

// bareProvider reports nothing: the "unknown", not "none", case.
type bareProvider struct{}

func (bareProvider) ChatStream(context.Context, []Message, []ToolDef) (<-chan Delta, error) {
	ch := make(chan Delta)
	close(ch)
	return ch, nil
}

// negotiatorProvider implements only the adapter-level ContentNegotiator.
type negotiatorProvider struct{ bareProvider }

func (negotiatorProvider) ContentSupport() ContentSupport {
	return ContentSupport{NativeTypes: map[MediaType]bool{MediaPNG: true}}
}

func mkCaps(list ...Capability) ModelCapabilities {
	m := map[Capability]bool{}
	for _, c := range list {
		m[c] = true
	}
	return ModelCapabilities{Caps: m, Known: true}
}

func TestSupportsAndMissing(t *testing.T) {
	mc := mkCaps(CapTools, CapReasoning, CapReasoningBudget)

	if !mc.Supports(CapReasoning) {
		t.Error("declared capability must be supported")
	}
	if mc.Supports(CapReasoningEffort) {
		t.Error("undeclared capability must not be supported")
	}
	if got := mc.Missing(CapTools, CapTemperature, CapSeed); len(got) != 2 {
		t.Errorf("Missing = %v, want [temperature seed]", got)
	}
	if !mc.SupportsAll(CapTools, CapReasoning) {
		t.Error("SupportsAll must be true when every want is declared")
	}
	if mc.SupportsAll(CapTools, CapTemperature) {
		t.Error("SupportsAll must be false when any want is missing")
	}
}

func TestZeroValueSupportsNothing(t *testing.T) {
	var mc ModelCapabilities
	if mc.Supports(CapTools) {
		t.Error("the zero value must declare nothing")
	}
	if mc.Known {
		t.Error("the zero value must not claim to be known")
	}
	if got := mc.Missing(CapTools, CapReasoning); len(got) != 2 {
		t.Errorf("Missing on zero value = %v, want both", got)
	}
}

func TestWithAndWithoutDoNotMutateReceiver(t *testing.T) {
	base := mkCaps(CapTools)

	added := base.With(CapReasoning)
	if base.Supports(CapReasoning) {
		t.Fatal("With must not mutate the receiver: catalog rows are shared")
	}
	if !added.Supports(CapReasoning) || !added.Supports(CapTools) {
		t.Error("With must keep existing capabilities and add the new one")
	}

	removed := added.Without(CapTools)
	if !added.Supports(CapTools) {
		t.Fatal("Without must not mutate the receiver")
	}
	if removed.Supports(CapTools) {
		t.Error("Without must drop the named capability")
	}
}

func TestForModelRetargets(t *testing.T) {
	base := mkCaps(CapTools)
	base.Model = "a"

	out := base.ForModel("b")
	if out.Model != "b" {
		t.Errorf("Model = %q, want b", out.Model)
	}
	if base.Model != "a" {
		t.Error("ForModel must not mutate the receiver")
	}
}

func TestIntersectKeepsOnlySharedCapabilities(t *testing.T) {
	primary := mkCaps(CapTools, CapReasoning, CapStructuredOutput, CapTemperature)
	primary.Provider = "anthropic"
	primary.ContextWindow = 200_000
	primary.StructuredOutput = StructuredOutputToolCall
	primary.Media = ContentSupport{NativeTypes: map[MediaType]bool{MediaPNG: true, MediaPDF: true}}

	secondary := mkCaps(CapTools, CapStructuredOutput, CapTemperature)
	secondary.Provider = "openai"
	secondary.ContextWindow = 128_000
	secondary.StructuredOutput = StructuredOutputNative
	secondary.Media = ContentSupport{NativeTypes: map[MediaType]bool{MediaPNG: true}}

	got := primary.Intersect(secondary)

	if got.Supports(CapReasoning) {
		t.Error("a capability only the primary has must not survive: the secondary may serve the request")
	}
	if !got.Supports(CapTools) || !got.Supports(CapTemperature) {
		t.Error("shared capabilities must survive")
	}
	if got.ContextWindow != 128_000 {
		t.Errorf("ContextWindow = %d, want the smaller 128000", got.ContextWindow)
	}
	if got.StructuredOutput != StructuredOutputToolCall {
		t.Errorf("StructuredOutput = %q, want the weaker tool_call mode", got.StructuredOutput)
	}
	if got.Media.Supports(MediaPDF) {
		t.Error("media only the primary reads natively must not survive")
	}
	if !got.Media.Supports(MediaPNG) {
		t.Error("shared media must survive")
	}
	if got.Provider != "anthropic+openai" {
		t.Errorf("Provider = %q, want the combined name", got.Provider)
	}
}

func TestIntersectWithUnknownCollapses(t *testing.T) {
	known := mkCaps(CapTools, CapStructuredOutput)
	known.StructuredOutput = StructuredOutputNative

	got := known.Intersect(ModelCapabilities{})

	if len(got.List()) != 0 {
		t.Errorf("intersecting with an unreporting provider must declare nothing, got %v", got.List())
	}
	if got.Known {
		t.Error("Known must be false when either side is unknown")
	}
}

func TestIntersectDropsEffortWhenVocabulariesDiffer(t *testing.T) {
	a := mkCaps(CapReasoning, CapReasoningEffort)
	a.ReasoningEfforts = []string{"low", "high"}
	b := mkCaps(CapReasoning, CapReasoningEffort)
	b.ReasoningEfforts = []string{"minimal", "low", "medium", "high"}

	got := a.Intersect(b)
	if got.Supports(CapReasoningEffort) {
		t.Error("effort must be dropped when the two sides accept different enums")
	}
	if !got.Supports(CapReasoning) {
		t.Error("reasoning itself is still shared and must survive")
	}
}

func TestIntersectStructuredOutputWithNoneClearsCapability(t *testing.T) {
	a := mkCaps(CapStructuredOutput)
	a.StructuredOutput = StructuredOutputNative
	b := mkCaps(CapStructuredOutput) // declares the cap but no mode

	got := a.Intersect(b)
	if got.Supports(CapStructuredOutput) {
		t.Error("a schema no member can enforce must not be advertised")
	}
}

func TestProviderCapabilitiesDistinguishesUnknownFromNone(t *testing.T) {
	reporting := &capProvider{caps: mkCaps(CapTools)}
	if _, ok := ProviderCapabilities(reporting); !ok {
		t.Error("a reporting provider must be recognised")
	}

	if _, ok := ProviderCapabilities(bareProvider{}); ok {
		t.Error("a provider that does not report must not be treated as reporting none")
	}
}

func TestMissingCapabilitiesFailsClosedOnUnknownProvider(t *testing.T) {
	got := MissingCapabilities(bareProvider{}, CapTools, CapReasoning)
	if len(got) != 2 {
		t.Errorf("MissingCapabilities on an unreporting provider = %v, want everything", got)
	}
}

func TestProviderContentSupportPrefersModelDeclaration(t *testing.T) {
	// The model-level declaration wins: an adapter knows how to encode a PDF,
	// but only the model decides whether it can read one.
	mc := mkCaps(CapTools)
	mc.Media = ContentSupport{NativeTypes: map[MediaType]bool{MediaPDF: true}}
	got := ProviderContentSupport(&capProvider{caps: mc})
	if !got.Supports(MediaPDF) || got.Supports(MediaPNG) {
		t.Errorf("ContentSupport = %+v, want the model declaration", got.NativeTypes)
	}

	// With no model declaration it falls back to the adapter negotiator.
	got = ProviderContentSupport(negotiatorProvider{})
	if !got.Supports(MediaPNG) {
		t.Error("must fall back to the adapter-level negotiator")
	}

	// With neither, nothing is native.
	if got := ProviderContentSupport(bareProvider{}); len(got.NativeTypes) != 0 {
		t.Errorf("ContentSupport = %+v, want empty", got.NativeTypes)
	}
}
