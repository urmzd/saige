package fallback

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// stubProvider reports a fixed identity, capability set and media support.
type stubProvider struct {
	name  string
	model string
	caps  *types.ModelCapabilities // nil = does not report
	media map[types.MediaType]bool
}

func (s *stubProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}
func (s *stubProvider) Name() string  { return s.name }
func (s *stubProvider) Model() string { return s.model }
func (s *stubProvider) ContentSupport() types.ContentSupport {
	return types.ContentSupport{NativeTypes: s.media}
}
func (s *stubProvider) Capabilities() types.ModelCapabilities {
	if s.caps == nil {
		return types.ModelCapabilities{}
	}
	return *s.caps
}

// bareStub reports neither capabilities nor content support.
type bareStub struct{ stubProvider }

func (b *bareStub) Capabilities() types.ModelCapabilities { return types.ModelCapabilities{} }

func mk(caps ...types.Capability) *types.ModelCapabilities {
	m := map[types.Capability]bool{}
	for _, c := range caps {
		m[c] = true
	}
	return &types.ModelCapabilities{Caps: m, Known: true}
}

func TestModelReportsThePrimary(t *testing.T) {
	f := New(
		&stubProvider{name: "a", model: "primary-model"},
		&stubProvider{name: "b", model: "secondary-model"},
	)
	if got := types.ProviderModel(f); got != "primary-model" {
		t.Errorf("Model() = %q, want the primary's model", got)
	}
	if got := types.ProviderModel(New()); got != "" {
		t.Errorf("Model() on an empty chain = %q, want empty", got)
	}
}

func TestCapabilitiesAreTheIntersection(t *testing.T) {
	// The primary reasons, the secondary does not. A caller that trusted the
	// primary's declaration would enable thinking and get a 400 the moment the
	// chain fell through.
	f := New(
		&stubProvider{name: "a", caps: mk(types.CapTools, types.CapReasoning, types.CapTemperature)},
		&stubProvider{name: "b", caps: mk(types.CapTools, types.CapTemperature)},
	)
	caps, ok := types.ProviderCapabilities(f)
	if !ok {
		t.Fatal("fallback must report capabilities")
	}
	if caps.Supports(types.CapReasoning) {
		t.Error("a capability only the primary has must not be advertised by the chain")
	}
	if !caps.Supports(types.CapTools) || !caps.Supports(types.CapTemperature) {
		t.Error("shared capabilities must survive the intersection")
	}
}

func TestCapabilitiesCollapseWhenAMemberDoesNotReport(t *testing.T) {
	f := New(
		&stubProvider{name: "a", caps: mk(types.CapTools, types.CapStructuredOutput)},
		&bareStub{},
	)
	caps, _ := types.ProviderCapabilities(f)
	if len(caps.List()) != 0 {
		t.Errorf("caps = %v, want nothing: an unreporting member makes the chain unknown", caps.List())
	}
	if caps.Known {
		t.Error("Known must be false when a member is unknown")
	}
}

func TestContentSupportIsTheIntersection(t *testing.T) {
	// A PDF the primary reads natively must still be extracted to text, because
	// the secondary cannot read it.
	f := New(
		&stubProvider{name: "a", media: map[types.MediaType]bool{types.MediaPNG: true, types.MediaPDF: true}},
		&stubProvider{name: "b", media: map[types.MediaType]bool{types.MediaPNG: true}},
	)
	got := types.ProviderContentSupport(f)
	if got.Supports(types.MediaPDF) {
		t.Error("media only the primary handles must not be advertised by the chain")
	}
	if !got.Supports(types.MediaPNG) {
		t.Error("shared media must survive")
	}
}

func TestSingleMemberChainPassesCapabilitiesThrough(t *testing.T) {
	f := New(&stubProvider{name: "a", caps: mk(types.CapTools, types.CapReasoning)})
	caps, _ := types.ProviderCapabilities(f)
	if !caps.SupportsAll(types.CapTools, types.CapReasoning) {
		t.Errorf("a one-member chain must report exactly that member, got %v", caps.List())
	}
}
