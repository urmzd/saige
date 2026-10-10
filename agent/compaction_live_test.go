package agent_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/types"
)

// TestCompactionLive compacts a long conversation with a real model and asks
// about a fact from its start, which only the selected span or the summary
// still holds. It runs only with SAIGE_LIVE=1 and an ANTHROPIC_API_KEY.
func TestCompactionLive(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("SAIGE_LIVE") != "1" || key == "" {
		t.Skip("set SAIGE_LIVE=1 and ANTHROPIC_API_KEY to call the provider")
	}
	const fact = "QX-58213"
	history := []types.Message{
		types.NewUserMessage("I am planning a conference. I will send notes; acknowledge each one briefly."),
		types.NewUserMessage("Note: the keynote speaker's hotel confirmation number is " + fact + "."),
		types.NewAssistantMessage("Noted the keynote speaker's hotel confirmation number."),
	}
	topics := []string{"catering menu", "badge printing", "projector rental", "parking passes", "wifi vouchers",
		"stage lighting", "signage", "volunteer shifts", "coffee breaks", "photographer", "name tags",
		"livestream", "room layout", "shuttle bus", "welcome bags", "registration desk", "sponsor booths",
		"accessibility ramps", "cloakroom", "first aid kit"}
	for i, topic := range topics {
		history = append(history,
			types.NewUserMessage(fmt.Sprintf("Note %d: the %s vendor confirmed for day %d; budget line %d is approved.", i, topic, i%3+1, 100+i)),
			types.NewAssistantMessage(fmt.Sprintf("Noted the %s confirmation.", topic)))
	}
	history = append(history, types.NewUserMessage("What is the keynote speaker's hotel confirmation number? Reply with the number only."))

	tests := []struct {
		name string
		cfg  types.CompactConfig
	}{
		{"relevant_plus_summary", types.CompactConfig{Strategy: types.CompactRelevantPlusSummary, KeepTurns: 2, SelectK: 2, MaxInputTokens: 500}},
		{"summary", types.CompactConfig{Strategy: types.CompactSummary, KeepTurns: 2, MaxInputTokens: 500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			a := agent.NewAgent(agent.AgentConfig{
				Provider:     anthropic.NewAdapter(key, "claude-haiku-5-5"),
				SystemPrompt: "You are a concise assistant.",
				MaxIter:      1,
				CompactCfg:   &cfg,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			stream := a.Invoke(ctx, history)
			var (
				text strings.Builder
				recs []types.CompactionContent
			)
			for d := range stream.Deltas() {
				switch v := d.(type) {
				case types.TextContentDelta:
					text.WriteString(v.Content)
				case types.CompactionDelta:
					recs = append(recs, v.Record)
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if len(recs) == 0 {
				t.Fatal("no compaction ran")
			}
			r := recs[0]
			t.Logf("strategy %s: tokens %d -> %d, kept %d, selected %d, summarized %d; answer %q",
				r.Strategy, r.TokensBefore, r.TokensAfter, len(r.Kept), len(r.Selected), len(r.Summarized), text.String())
			if r.TokensAfter >= r.TokensBefore || len(r.Summarized) == 0 {
				t.Fatalf("record = %+v", r)
			}
			if tt.cfg.Strategy == types.CompactRelevantPlusSummary && len(r.Selected) == 0 {
				t.Fatal("BM25 selected nothing")
			}
			if !strings.Contains(text.String(), fact) {
				t.Fatalf("answer %q does not hold the early fact", text.String())
			}
		})
	}
}
