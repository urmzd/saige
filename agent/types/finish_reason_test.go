package types

import "testing"

func TestNormalizeFinishReason(t *testing.T) {
	for in, want := range map[string]string{
		"length":            FinishReasonMaxTokens, // OpenAI, Ollama
		"max_tokens":        FinishReasonMaxTokens, // Anthropic
		"MAX_TOKENS":        FinishReasonMaxTokens, // Gemini
		"max_output_tokens": FinishReasonMaxTokens, // OpenAI Responses
		"stop":              "stop",
		"end_turn":          "end_turn",
		"":                  "",
	} {
		if got := NormalizeFinishReason(in); got != want {
			t.Errorf("NormalizeFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
