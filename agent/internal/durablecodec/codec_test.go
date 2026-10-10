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
		{"truncated turn", types.AssistantMessage{Parts: []types.AssistantPart{
			types.TextPart{Text: "partial"}, types.TruncationPart{Reason: "max_tokens"},
		}}},
		{"server tool and route", types.AssistantMessage{Parts: []types.AssistantPart{
			types.ServerToolCallPart{ID: "s", ToolKind: types.ServerToolKind("web_search"), Name: "web_search"},
			types.ServerToolResultPart{CallID: "s", ToolKind: types.ServerToolKind("web_search"), Text: "r"},
			types.RoutePart{Profile: "p"},
		}}},
		{"steered message", types.UserMessage{Parts: []types.UserPart{
			types.TextPart{Text: "x"}, types.SteerPart{ID: "sub"},
		}}},
		{"media and tool output", types.UserMessage{Parts: []types.UserPart{
			types.Image(types.Bytes(types.MediaPNG, []byte{1, 2, 3})),
			types.ToolOK("c", types.Text("t"), types.JSONPart{JSON: []byte(`{"a":1}`)}, types.Document(types.URL("file:///d.pdf", types.MediaPDF))),
		}}},
		{"citation and refusal", types.AssistantMessage{Parts: []types.AssistantPart{
			types.CitationPart{Citation: types.NewCitation(types.CitationWeb, "https://x", "x")}, types.RefusalPart{Text: "no"},
		}}},
		{"tool call arguments", types.AssistantMessage{Parts: []types.AssistantPart{
			types.ToolCallPart{ID: "c", Name: "f", Arguments: map[string]any{"a": []any{"x", map[string]any{"b": 1.0}}}},
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
