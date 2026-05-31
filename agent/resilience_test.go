package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// midStreamErrorProvider streams some text and then a mid-stream ErrorDelta,
// mimicking a provider that overloads after message_start.
type midStreamErrorProvider struct{}

func (midStreamErrorProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta, 4)
	ch <- types.TextStartDelta{}
	ch <- types.TextContentDelta{Content: "partial"}
	ch <- types.ErrorDelta{Error: errors.New("overloaded")}
	close(ch)
	return ch, nil
}

// TestMidStreamErrorFailsTurn ensures a mid-stream provider error fails the turn
// instead of being treated as a successful (truncated) response.
func TestMidStreamErrorFailsTurn(t *testing.T) {
	a := NewAgent(AgentConfig{Provider: midStreamErrorProvider{}, SystemPrompt: "s"})
	deltas := collectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("hi")}))

	errs := collectDeltasByType[types.ErrorDelta](deltas)
	if len(errs) == 0 {
		t.Fatal("expected an ErrorDelta for the mid-stream provider failure")
	}
	if !strings.Contains(errs[0].Error.Error(), "overloaded") {
		t.Errorf("error = %v, want it to carry the provider error", errs[0].Error)
	}

	// The failed turn must NOT persist a 'successful' assistant message.
	msgs, _ := a.Tree().FlattenBranch("main")
	for _, m := range msgs {
		if _, ok := m.(types.AssistantMessage); ok {
			t.Error("a partial/failed turn must not persist an AssistantMessage")
		}
	}
}

// TestMidStreamErrorIsRetobservable confirms the durable path also surfaces the
// error (RunDurable captures it rather than returning a phantom success).
func TestMidStreamErrorDurable(t *testing.T) {
	a := NewAgent(AgentConfig{Provider: midStreamErrorProvider{}, SystemPrompt: "s"})
	final, err := a.RunDurable(context.Background(), newRecordingRunner(), []types.Message{types.NewUserMessage("hi")}, "")
	if err == nil {
		t.Fatal("expected RunDurable to return the mid-stream error")
	}
	if final != nil {
		t.Errorf("expected nil final message on error, got %+v", final)
	}
}
