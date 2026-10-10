package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// modelSwitchProvider is a fake types.ModelSwitcher that records which model
// variant served each Stream call.
type modelSwitchProvider struct {
	model string
	mu    *sync.Mutex
	seen  *[]string
}

func newModelSwitchProvider(model string) *modelSwitchProvider {
	return &modelSwitchProvider{model: model, mu: &sync.Mutex{}, seen: &[]string{}}
}

func (p *modelSwitchProvider) Stream(_ context.Context, _ types.Request) (<-chan types.Delta, error) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.model)
	p.mu.Unlock()
	ch := make(chan types.Delta, 4)
	ch <- types.PartStart{Index: 0, Kind: types.KindText}
	ch <- types.PartDelta{Index: 0, Text: "ok"}
	ch <- types.PartEnd{Index: 0}
	close(ch)
	return ch, nil
}

func (p *modelSwitchProvider) Model() string { return p.model }

func (p *modelSwitchProvider) WithTarget(t types.Target) (types.Provider, error) {
	model := string(t.Model)
	c := *p
	c.model = model
	return &c, nil
}

// A ConfigPart block that sets Model must re-target the provider call.
func TestConfigContentModelSwitchesProvider(t *testing.T) {
	provider := newModelSwitchProvider("base-model")
	agent := must.Get(New(Config{Provider: provider, SystemPrompt: "sys"}))

	msg := types.UserMessage{Parts: []types.UserPart{
		types.ConfigPart{Target: types.ModelTarget("fast-model")},
		types.TextPart{Text: "hi"},
	}}
	stream := agent.Invoke(context.Background(), []types.Message{msg})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if got := *provider.seen; len(got) != 1 || got[0] != "fast-model" {
		t.Errorf("models used = %v, want [fast-model]", got)
	}
	if provider.Model() != "base-model" {
		t.Errorf("original provider model = %q, want base-model unchanged", provider.Model())
	}
}

// Without a ConfigPart model the provider is used as configured.
func TestNoConfigModelUsesConfiguredProvider(t *testing.T) {
	provider := newModelSwitchProvider("base-model")
	agent := must.Get(New(Config{Provider: provider, SystemPrompt: "sys"}))

	stream := agent.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if got := *provider.seen; len(got) != 1 || got[0] != "base-model" {
		t.Errorf("models used = %v, want [base-model]", got)
	}
}
