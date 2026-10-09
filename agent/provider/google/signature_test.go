package google

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestFunctionCallSignatureRoundTrip checks that the thought signature Gemini
// 3 puts on a function call part survives the turn: it streams as an empty,
// signed thinking block before the call, and goes back on the call's part,
// not as a separate thought part.
func TestFunctionCallSignatureRoundTrip(t *testing.T) {
	events := []string{`{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"get_weather","args":{"city":"Oslo"}},"thoughtSignature":"c2lnLTE="},` +
		`{"functionCall":{"name":"get_weather","args":{"city":"Rome"}}}]},"finishReason":"STOP"}]}`}
	a, err := NewAdapter(context.Background(), "test-key", "gemini-3.1-flash-lite",
		WithHTTPClient(&http.Client{Transport: sseTransport{events: events}}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.ChatStream(context.Background(), []types.Message{types.NewUserMessage("weather")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var msg types.AssistantMessage
	var thinkingOpen bool
	for d := range ch {
		switch v := d.(type) {
		case types.ThinkingStartDelta:
			thinkingOpen = true
		case types.ThinkingEndDelta:
			if !thinkingOpen {
				t.Fatal("thinking end without start")
			}
			msg.Content = append(msg.Content, types.ThinkingContent{Signature: v.Signature})
			thinkingOpen = false
		case types.ToolCallEndDelta:
			msg.Content = append(msg.Content, types.ToolUseContent{ID: v.ID, Name: "get_weather", Arguments: v.Arguments})
		case types.ErrorDelta:
			t.Fatal(v.Error)
		}
	}
	if len(msg.Content) != 3 {
		t.Fatalf("content = %+v, want a signed block and two calls", msg.Content)
	}
	if th, ok := msg.Content[0].(types.ThinkingContent); !ok || th.Signature != "c2lnLTE=" || th.Thinking != "" {
		t.Fatalf("first block = %+v", msg.Content[0])
	}

	_, contents := toGeminiContents([]types.Message{types.NewUserMessage("weather"), msg})
	parts := contents[len(contents)-1].Parts
	if len(parts) != 2 {
		t.Fatalf("model parts = %d, want the two calls only", len(parts))
	}
	if parts[0].FunctionCall == nil || !bytes.Equal(parts[0].ThoughtSignature, []byte("sig-1")) {
		t.Fatalf("first call part = %+v, want the signature attached", parts[0])
	}
	if parts[1].FunctionCall == nil || len(parts[1].ThoughtSignature) != 0 {
		t.Fatalf("second call part = %+v, want no signature", parts[1])
	}
}
