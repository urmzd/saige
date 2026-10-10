package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// modalStepProvider is a stepProvider whose rate card prices audio input
// at $3 per million tokens over a $1 text input rate, and nothing else.
type modalStepProvider struct{ *stepProvider }

func (modalStepProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Provider: "modal", Model: "m", Known: true,
		Pricing: types.Pricing{InputPerMTok: 1, OutputPerMTok: 4, AsOf: "2026-10-10",
			Modal: map[types.Modality]types.ModalityRate{types.ModalityAudio: {InputPerMTok: 3}}}}
}

// TestSettlementPricesModalities checks that a call's audio tokens are
// charged at the audio rate, and that usage of a modality the card does
// not price keeps the turn but ends the run with ErrUnpriced unless the
// policy allows unpriced calls.
func TestSettlementPricesModalities(t *testing.T) {
	audio := types.UsageDelta{PromptTokens: 200_000, PromptByModality: map[types.Modality]int{types.ModalityText: 100_000, types.ModalityAudio: 100_000}}
	video := types.UsageDelta{PromptTokens: 200_000, PromptByModality: map[types.Modality]int{types.ModalityText: 100_000, types.ModalityVideo: 100_000}}
	for _, tc := range []struct {
		name    string
		usage   types.UsageDelta
		allow   bool
		wantErr error
		spent   types.Cost
	}{
		{name: "priced audio", usage: audio, spent: types.USD(0.1 + 0.3)},
		{name: "unpriced video fails closed", usage: video, wantErr: types.ErrUnpriced, spent: types.USD(0.2)},
		{name: "unpriced video allowed", usage: video, allow: true, spent: types.USD(0.2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := modalStepProvider{newStepProvider(stepCall{before: append([]types.Delta{tc.usage}, agenttest.TextResponse("answer")...)})}
			budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(5), AllowUnpriced: tc.allow})
			a := NewAgent(AgentConfig{Provider: provider}, WithBudget(budget))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
			var text string
			for d := range stream.Deltas() {
				if v, ok := d.(types.PartDelta); ok {
					text += v.Text
				}
			}
			err := stream.Wait()
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("Wait = %v, want %v", err, tc.wantErr)
			}
			if text != "answer" {
				t.Errorf("text = %q, want the paid turn kept", text)
			}
			if got := budget.Spent(); got != tc.spent {
				t.Errorf("spent = %s, want %s", got, tc.spent)
			}
		})
	}
}
