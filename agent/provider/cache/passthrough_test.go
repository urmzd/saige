package cache

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// richProvider reports every optional interface a real adapter does, so the
// tests can assert the decorator forwards each one.
type richProvider struct {
	model string
	caps  types.ModelCapabilities
	media map[types.MediaType]bool
}

func (r *richProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}
func (r *richProvider) Name() string  { return "rich" }
func (r *richProvider) Model() string { return r.model }
func (r *richProvider) WithModel(model string) types.Provider {
	c := *r
	c.model = model
	return &c
}
func (r *richProvider) Capabilities() types.ModelCapabilities { return r.caps }
func (r *richProvider) ContentSupport() types.ContentSupport {
	return types.ContentSupport{NativeTypes: r.media}
}

func newRich() *richProvider {
	return &richProvider{
		model: "base-model",
		caps: types.ModelCapabilities{
			Caps:  map[types.Capability]bool{types.CapTools: true, types.CapReasoning: true},
			Known: true,
		},
		media: map[types.MediaType]bool{types.MediaPDF: true},
	}
}

// A cache changes latency, not what the model accepts: every capability probe
// must see through it. Before this passthrough existed, wrapping an adapter in
// a cache silently disabled model switching and native media support.
func TestCacheForwardsModelSwitching(t *testing.T) {
	p := New(newRich(), Config{})

	if got := types.ProviderModel(p); got != "base-model" {
		t.Errorf("Model() = %q, want the inner model", got)
	}

	switched := types.ProviderWithModel(p, "other-model")
	if got := types.ProviderModel(switched); got != "other-model" {
		t.Errorf("after WithModel, Model() = %q, want other-model", got)
	}
	if _, ok := switched.(*Provider); !ok {
		t.Error("WithModel must return a cache-wrapped provider, not unwrap the decorator")
	}
	if got := types.ProviderModel(p); got != "base-model" {
		t.Errorf("WithModel must not mutate the original, Model() = %q", got)
	}
}

func TestCacheForwardsCapabilities(t *testing.T) {
	p := New(newRich(), Config{})
	caps, ok := types.ProviderCapabilities(p)
	if !ok {
		t.Fatal("a cache-wrapped provider must report capabilities")
	}
	if !caps.SupportsAll(types.CapTools, types.CapReasoning) {
		t.Errorf("caps = %v, want the inner provider's", caps.List())
	}
}

func TestCacheForwardsContentSupport(t *testing.T) {
	p := New(newRich(), Config{})
	if !types.ProviderContentSupport(p).Supports(types.MediaPDF) {
		t.Error("native media support must survive the cache decorator, or the file pipeline extracts PDFs the model could read")
	}
}
