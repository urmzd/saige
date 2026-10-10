package google

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// Scenario (c) on Gemini 3: depth high is sent as thinking level HIGH, and
// depth max maps to the highest declared level.
func TestDialsCompileToThinkingLevel(t *testing.T) {
	for _, d := range []types.Depth{types.DepthHigh, types.DepthMax} {
		var bodies []map[string]any
		a, err := NewAdapter(context.Background(), "k", "gemini-3.8-flash",
			WithHTTPClient(&http.Client{Transport: captureTransport{events: []string{doneEvent}, bodies: &bodies}}),
			WithDials(types.DialLayer{Scope: types.DialScopeEntry, Dials: types.Dials{Reasoning: &types.ReasoningDial{Depth: d}}}))
		if err != nil {
			t.Fatal(err)
		}
		ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
		cfg, _ := bodies[0]["generationConfig"].(map[string]any)
		if got := fmt.Sprint(cfg["thinkingConfig"]); got != "map[includeThoughts:true thinkingLevel:HIGH]" {
			t.Fatalf("depth %s: thinkingConfig = %s", d, got)
		}
	}
}
