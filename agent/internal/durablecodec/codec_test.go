package durablecodec

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestGobRoundTripsContent(t *testing.T) {
	tests := []struct {
		name string
		msg  types.Message
	}{
		{"truncated turn", types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "partial"}, types.TruncationContent{Reason: "max_tokens"},
		}}},
		{"server tool and route", types.AssistantMessage{Content: []types.AssistantContent{
			types.ServerToolContent{ID: "s", Kind: types.ServerToolKind("web_search"), Text: "r"},
			types.RouteContent{Profile: "p"},
		}}},
		{"steered message", types.UserMessage{Content: []types.UserContent{
			types.TextContent{Text: "x"}, types.SteerContent{ID: "sub"},
		}}},
		{"tool call arguments", types.AssistantMessage{Content: []types.AssistantContent{
			types.ToolUseContent{ID: "c", Name: "f", Arguments: map[string]any{"a": []any{"x", map[string]any{"b": 1.0}}}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			in := []types.Message{tt.msg}
			if err := gob.NewEncoder(&buf).Encode(&in); err != nil {
				t.Fatal(err)
			}
			var out []types.Message
			if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out, in) {
				t.Fatalf("round trip = %#v, want %#v", out, in)
			}
		})
	}
}
