package anthropic

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestEmptySystemPromptIsOmitted checks that blank system text never becomes
// an empty text block, which the API rejects with 400, and that a request
// with no system text left sends no system field at all.
func TestEmptySystemPromptIsOmitted(t *testing.T) {
	for _, tc := range []struct {
		name string
		msgs []types.Message
		want int // system blocks expected; 0 means the field is absent
	}{
		{"empty", []types.Message{types.SystemMsg(types.Text("")), types.UserMsg(types.Text("hi"))}, 0},
		{"whitespace", []types.Message{types.SystemMsg(types.Text(" \n\t")), types.UserMsg(types.Text("hi"))}, 0},
		{"blank beside real text", []types.Message{types.SystemMsg(types.Text("")), types.SystemMsg(types.Text("rules")), types.UserMsg(types.Text("hi"))}, 1},
		{"no system message", []types.Message{types.UserMsg(types.Text("hi"))}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := NewAdapter("k", "claude-haiku-5-5", WithBaseURL(server.URL))
			ch, err := a.Stream(context.Background(), types.Request{Messages: tc.msgs})
			if err != nil {
				t.Fatal(err)
			}
			drain(ch)
			system, present := (*bodies)[0]["system"]
			if tc.want == 0 {
				if present {
					t.Fatalf("system = %v, want the field absent", system)
				}
				return
			}
			blocks, _ := system.([]any)
			if len(blocks) != tc.want {
				t.Fatalf("system = %v, want %d block(s)", system, tc.want)
			}
			for _, b := range blocks {
				if b.(map[string]any)["text"] == "" {
					t.Fatalf("empty system block sent: %v", system)
				}
			}
		})
	}
}
