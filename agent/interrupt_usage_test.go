package agent

import (
	"context"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// TestInterruptReportsUsageWithoutBudget checks that a call stopped by
// SubmitInterruptReplace reports its usage on the stream even when no budget
// is configured: the reported counts are kept, and missing ones estimated.
func TestInterruptReportsUsageWithoutBudget(t *testing.T) {
	tests := []struct {
		name       string
		before     []types.Delta
		wantPrompt int // exact count expected; 0 means any estimate above zero
	}{
		{name: "usage reported before the interrupt", before: []types.Delta{
			types.UsageDelta{PromptTokens: 1000}, types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "half"},
		}, wantPrompt: 1000},
		{name: "no usage reported yet", before: []types.Delta{types.PartStart{Index: 1, Kind: types.KindText}, types.PartDelta{Index: 1, Text: "half"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStepProvider(
				stepCall{before: tt.before, hold: make(chan struct{})},
				stepCall{before: agenttest.TextResponse("replaced")},
			)
			a := must.Get(New(Config{Provider: provider}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
			submitted, interrupted := false, false
			var text string
			var stopped *types.UsageDelta
			for d := range stream.Deltas() {
				switch v := d.(type) {
				case types.PartDelta:
					text += v.Text
				case types.InterruptedDelta:
					interrupted = true
				case types.UsageDelta:
					// The interrupted turn's usage precedes its marker; a
					// usage reported while the call was still live is not it.
					if submitted && !interrupted && stopped == nil && v.CompletionTokens > 0 {
						u := v
						stopped = &u
					}
				}
				if !submitted && text == "half" {
					submitted = true
					if _, err := stream.Submit(types.UserMsg(types.Text("new direction")), SubmitInterruptReplace); err != nil {
						t.Fatalf("Submit: %v", err)
					}
				}
			}
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if stopped == nil {
				t.Fatal("the interrupted turn reported no usage")
			}
			if stopped.PromptTokens <= 0 || (tt.wantPrompt > 0 && stopped.PromptTokens != tt.wantPrompt) {
				t.Fatalf("prompt tokens = %d, want %d", stopped.PromptTokens, tt.wantPrompt)
			}
		})
	}
}
