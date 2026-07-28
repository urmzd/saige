package retry

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

type capableProvider struct {
	media map[types.MediaType]bool
}

func (c *capableProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}
func (c *capableProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{
		Caps:  map[types.Capability]bool{types.CapReasoning: true},
		Known: true,
	}
}
func (c *capableProvider) ContentSupport() types.ContentSupport {
	return types.ContentSupport{NativeTypes: c.media}
}

func TestRetryForwardsCapabilitiesAndContentSupport(t *testing.T) {
	inner := &capableProvider{media: map[types.MediaType]bool{types.MediaPNG: true}}
	p := New(inner, DefaultConfig())

	caps, ok := types.ProviderCapabilities(p)
	if !ok || !caps.Supports(types.CapReasoning) {
		t.Errorf("capabilities must survive the retry decorator, got %v (reported=%v)", caps.List(), ok)
	}
	if !types.ProviderContentSupport(p).Supports(types.MediaPNG) {
		t.Error("native media support must survive the retry decorator")
	}
}

// An inner provider that reports nothing must not be laundered into a
// confident "supports nothing": Known stays false so callers can fail closed.
func TestRetryOverAnUnreportingProviderStaysUnknown(t *testing.T) {
	p := New(&mockProvider{}, DefaultConfig())
	caps, _ := types.ProviderCapabilities(p)
	if caps.Known {
		t.Error("Known must be false when the inner provider does not report")
	}
	if got := types.MissingCapabilities(p, types.CapTools); len(got) != 1 {
		t.Errorf("MissingCapabilities = %v, want tools reported missing", got)
	}
}
