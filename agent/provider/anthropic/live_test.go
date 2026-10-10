package anthropic

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// redactedThinkingTrigger is the test prompt Anthropic documents for
// making a thinking model return a redacted_thinking block.
const redactedThinkingTrigger = "ANTHROPIC_MAGIC_STRING_TRIGGER_REDACTED_THINKING_46C9A13E193C177646C7398A98432ECCCE4C1253D5E2D82641AC0E52CC2876CB"

func liveStream(t *testing.T, a *Adapter, req types.Request) types.AssistantMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch, err := a.Stream(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var all []types.Delta
	asm := types.NewPartAssembler()
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatal(e.Error)
		}
		all = append(all, d)
		asm.Push(d)
	}
	streamcheck.RunPartConformance(t, all)
	if n := asm.Violations(); n != 0 {
		t.Fatalf("%d part violations", n)
	}
	return types.AssistantMessage{Parts: asm.Parts()}
}

// TestLiveRedactedThinkingRoundTrips asks claude-haiku-4-5 (the cheapest
// model with manual thinking) for the documented redacted-thinking
// trigger, checks the block arrives as a redacted ThinkingPart carrying its
// data, and replays it in a second turn, which the API accepts only with
// the data intact. It runs only with SAIGE_LIVE=1 and ANTHROPIC_API_KEY.
func TestLiveRedactedThinkingRoundTrips(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("SAIGE_LIVE") != "1" || key == "" {
		t.Skip("set SAIGE_LIVE=1 and ANTHROPIC_API_KEY to call the provider")
	}
	a := must.Get(New(Config{APIKey: key, Model: "claude-haiku-4-5"}, WithThinking(1024), WithMaxTokens(2048)))
	first := types.UserMsg(types.Text(redactedThinkingTrigger))
	reply := liveStream(t, a, types.Request{Messages: []types.Message{first}})
	var redacted *types.ThinkingPart
	for _, th := range types.Each[types.ThinkingPart](reply) {
		if th.Redacted {
			redacted = &th
		}
	}
	if redacted == nil || redacted.Signature == "" {
		t.Fatalf("reply parts = %+v, want a redacted thinking part with its data", reply.Parts)
	}
	again := liveStream(t, a, types.Request{Messages: []types.Message{first, reply, types.UserMsg(types.Text("Reply with the word ok."))}})
	if types.TextOf(again) == "" {
		t.Fatalf("second turn = %+v, want text", again.Parts)
	}
}
