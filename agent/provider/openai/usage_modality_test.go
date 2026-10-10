package openai

import (
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/urmzd/saige/agent/types"
)

func TestChatUsageByModality(t *testing.T) {
	var u openai.CompletionUsage
	u.PromptTokensDetails.TextTokens = 12
	u.PromptTokensDetails.AudioTokens = 300
	u.CompletionTokensDetails.AudioTokens = 80
	prompt, completion := chatModalityCounts(u)
	if len(prompt) != 2 || prompt[types.ModalityText] != 12 || prompt[types.ModalityAudio] != 300 {
		t.Fatalf("prompt = %v", prompt)
	}
	if len(completion) != 1 || completion[types.ModalityAudio] != 80 {
		t.Fatalf("completion = %v", completion)
	}
	if p, c := chatModalityCounts(openai.CompletionUsage{PromptTokens: 5}); p != nil || c != nil {
		t.Fatalf("unreported details = %v %v, want nil", p, c)
	}
}
