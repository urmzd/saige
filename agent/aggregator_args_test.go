package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestDefaultAggregatorToolArguments(t *testing.T) {
	start := types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "f"}
	tests := []struct {
		name    string
		deltas  []types.Delta
		want    map[string]any
		wantErr string // substring of ArgumentsError, "" for none
	}{
		{
			name: "end part wins over buffered text",
			deltas: []types.Delta{start, types.PartDelta{Index: 0, Args: `{"a":1}`},
				types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "f", Arguments: map[string]any{"b": 2.0}}}},
			want: map[string]any{"b": 2.0},
		},
		{
			name: "buffered text decoded when end has no part",
			deltas: []types.Delta{start, types.PartDelta{Index: 0, Args: `{"a":`},
				types.PartDelta{Index: 0, Args: `1}`}, types.PartEnd{Index: 0}},
			want: map[string]any{"a": 1.0},
		},
		{
			name:    "malformed buffered text reported",
			deltas:  []types.Delta{start, types.PartDelta{Index: 0, Args: `{"a":`}, types.PartEnd{Index: 0}},
			wantErr: "unexpected end of JSON input",
		},
		{
			name: "producer reported parse error",
			deltas: []types.Delta{start,
				types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "f", ArgumentsError: "bad json"}}},
			wantErr: "bad json",
		},
		{
			name:   "no argument text is a call without arguments",
			deltas: []types.Delta{start, types.PartEnd{Index: 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			msg := agg.Message().(types.AssistantMessage)
			got := msg.Parts[0].(types.ToolCallPart)
			if got.ID != "c1" || got.Name != "f" {
				t.Fatalf("call = %+v, want ID c1 and name f", got)
			}
			if !reflect.DeepEqual(got.Arguments, tt.want) {
				t.Errorf("arguments = %#v, want %#v", got.Arguments, tt.want)
			}
			if tt.wantErr == "" && got.ArgumentsError != "" || !strings.Contains(got.ArgumentsError, tt.wantErr) {
				t.Errorf("arguments error = %q, want %q", got.ArgumentsError, tt.wantErr)
			}
			if tt.wantErr != "" && got.Arguments != nil {
				t.Errorf("a failed decode produced arguments %#v", got.Arguments)
			}
		})
	}
}

// Replaying a stored turn reproduces a call whose arguments failed to parse.
func TestReplayKeepsArgumentsError(t *testing.T) {
	stored := types.AssistantMsg(types.ToolCallPart{ID: "c1", Name: "f", ArgumentsError: "unexpected end of JSON input"})
	agg := NewDefaultAggregator()
	for d := range Replay([]types.Message{stored}).Deltas() {
		agg.Push(d)
	}
	msg := agg.Message().(types.AssistantMessage)
	if got := msg.Parts[0].(types.ToolCallPart); got.ArgumentsError == "" {
		t.Fatalf("replayed call = %+v, want the arguments error kept", got)
	}
}
