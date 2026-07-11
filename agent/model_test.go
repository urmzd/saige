package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// modelSwitchProvider is a fake types.ModelSwitcher that records which model
// variant served each ChatStream call.
type modelSwitchProvider struct {
	model string
	mu    *sync.Mutex
	seen  *[]string
}

func newModelSwitchProvider(model string) *modelSwitchProvider {
	return &modelSwitchProvider{model: model, mu: &sync.Mutex{}, seen: &[]string{}}
}

func (p *modelSwitchProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	*p.seen = append(*p.seen, p.model)
	p.mu.Unlock()
	ch := make(chan types.Delta, 4)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "ok"}
	ch <- types.TextEndDelta{}
	close(ch)
	return ch, nil
}

func (p *modelSwitchProvider) Model() string { return p.model }

func (p *modelSwitchProvider) WithModel(model string) types.Provider {
	c := *p
	c.model = model
	return &c
}

// A ConfigContent block that sets Model must re-target the provider call.
func TestConfigContentModelSwitchesProvider(t *testing.T) {
	provider := newModelSwitchProvider("base-model")
	agent := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "sys"})

	msg := types.UserMessage{Content: []types.UserContent{
		types.ConfigContent{Model: "fast-model"},
		types.TextContent{Text: "hi"},
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

// Without a ConfigContent model the provider is used as configured.
func TestNoConfigModelUsesConfiguredProvider(t *testing.T) {
	provider := newModelSwitchProvider("base-model")
	agent := NewAgent(AgentConfig{Provider: provider, SystemPrompt: "sys"})

	stream := agent.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait() = %v, want nil", err)
	}

	if got := *provider.seen; len(got) != 1 || got[0] != "base-model" {
		t.Errorf("models used = %v, want [base-model]", got)
	}
}
