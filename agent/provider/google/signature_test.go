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
	a, err := New(context.Background(), Config{APIKey: "test-key", Model: "gemini-3.1-flash-lite"},
		WithHTTPClient(&http.Client{Transport: sseTransport{events: events}}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("weather"))}})
	if err != nil {
		t.Fatal(err)
	}
	asm := types.NewPartAssembler()
	for d := range ch {
		if v, ok := d.(types.ErrorDelta); ok {
			t.Fatal(v.Error)
		}
		asm.Push(d)
	}
	if n := asm.Violations(); n != 0 {
		t.Fatalf("%d part protocol violations", n)
	}
	msg := types.AssistantMessage{Parts: asm.Parts()}
	if len(msg.Parts) != 3 {
		t.Fatalf("content = %+v, want a signed block and two calls", msg.Parts)
	}
	if th, ok := msg.Parts[0].(types.ThinkingPart); !ok || th.Signature != "c2lnLTE=" || th.Text != "" {
		t.Fatalf("first block = %+v", msg.Parts[0])
	}

	_, contents, err := (&mapper{names: map[string]string{}}).contents([]types.Message{types.UserMsg(types.Text("weather")), msg})
	if err != nil {
		t.Fatal(err)
	}
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

func TestEmptySystemTextSendsNoInstruction(t *testing.T) {
	inst, _, err := (&mapper{names: map[string]string{}}).contents([]types.Message{
		types.SystemMsg(types.Text("")),
		types.UserMsg(types.Text("hi")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst != nil {
		t.Fatalf("system instruction = %+v, want none for an empty system prompt", inst)
	}
}
