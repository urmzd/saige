package agent

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestDefaultAggregatorToolArguments(t *testing.T) {
	tests := []struct {
		name     string
		deltas   []types.Delta
		wantArgs map[string]any
		wantErr  bool
	}{
		{
			name: "end arguments win over buffered text",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallArgumentDelta{Content: `{"x":`},
				types.ToolCallEndDelta{Arguments: map[string]any{"x": 1}},
			},
			wantArgs: map[string]any{"x": 1},
		},
		{
			name: "buffered text decoded when end has no arguments",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallArgumentDelta{Content: `{"x":`},
				types.ToolCallArgumentDelta{Content: `"y"}`},
				types.ToolCallEndDelta{},
			},
			wantArgs: map[string]any{"x": "y"},
		},
		{
			name: "malformed buffered text reported",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallArgumentDelta{Content: `{"x":`},
				types.ToolCallEndDelta{},
			},
			wantErr: true,
		},
		{
			name: "producer reported parse error",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallEndDelta{ID: "a", ArgumentsError: "unexpected end of JSON input"},
			},
			wantErr: true,
		},
		{
			name: "no argument text is a call without arguments",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallEndDelta{},
			},
		},
		{
			name: "fragments without ID go to the newest call",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "f"},
				types.ToolCallStartDelta{ID: "b", Name: "g"},
				types.ToolCallArgumentDelta{Content: `{"broken"`},
				types.ToolCallEndDelta{ID: "a"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			msg, ok := agg.Message().(types.AssistantMessage)
			if !ok || len(msg.Content) == 0 {
				t.Fatalf("message = %#v, want one tool call", agg.Message())
			}
			use := msg.Content[0].(types.ToolUseContent)
			if (use.ArgumentsError != "") != tt.wantErr {
				t.Fatalf("ArgumentsError = %q, wantErr %v", use.ArgumentsError, tt.wantErr)
			}
			if tt.wantErr && use.Arguments != nil {
				t.Errorf("arguments = %v, want nil on a parse error", use.Arguments)
			}
			if !tt.wantErr && !reflect.DeepEqual(use.Arguments, tt.wantArgs) {
				t.Errorf("arguments = %v, want %v", use.Arguments, tt.wantArgs)
			}
		})
	}
}

// Replaying a stored turn reproduces a call whose arguments failed to parse.
func TestReplayKeepsArgumentsError(t *testing.T) {
	stored := types.AssistantMessage{Content: []types.AssistantContent{
		types.ToolUseContent{ID: "c1", Name: "f", ArgumentsError: "unexpected end of JSON input"},
	}}
	agg := NewDefaultAggregator()
	for d := range Replay([]types.Message{stored}).Deltas() {
		agg.Push(d)
	}
	msg := agg.Message().(types.AssistantMessage)
	if got := msg.Content[0].(types.ToolUseContent); got.ArgumentsError == "" {
		t.Fatalf("replayed call = %+v, want the arguments error kept", got)
	}
}
