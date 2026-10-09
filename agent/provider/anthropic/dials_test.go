package anthropic

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// Scenario (a) and (c) on Claude: creativity is dropped on an adaptive
// model, and depth high is sent as effort high.
func TestDialsCompileOnAdaptiveModel(t *testing.T) {
	server, bodies := captureServer(t)
	focused := types.CreativityFocused
	a := NewAdapter("k", "claude-haiku-5-5", WithBaseURL(server.URL),
		WithDials(types.DialLayer{Scope: types.DialScopeEntry, Dials: types.Dials{Creativity: &focused}}))
	ch, err := a.ChatStreamWithOptions(context.Background(), []types.Message{types.NewUserMessage("hi")}, nil,
		types.RequestOptions{Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: types.DepthHigh}}})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	body := (*bodies)[0]
	if _, ok := body["temperature"]; ok {
		t.Fatalf("temperature sent to an adaptive model: %v", body)
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Fatalf("output_config = %v", body["output_config"])
	}
}

// Scenario (b): a seed is contractual and Claude takes none.
func TestDialSeedRejectedOnClaude(t *testing.T) {
	server, bodies := captureServer(t)
	seed := int64(1)
	_, err := NewAdapter("k", "claude-haiku-5-5", WithBaseURL(server.URL)).ChatStreamWithOptions(context.Background(),
		[]types.Message{types.NewUserMessage("hi")}, nil, types.RequestOptions{Dials: types.Dials{Seed: &seed}})
	if !errors.Is(err, types.ErrInvalidModelConfig) || len(*bodies) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(*bodies))
	}
}
