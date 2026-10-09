package types

// FinishReasonMaxTokens is the provider-neutral finish reason for a response
// the output token limit cut short. Providers name it differently (OpenAI and
// Ollama "length", Anthropic "max_tokens", Gemini "MAX_TOKENS", the Responses
// API "max_output_tokens"); TruncatedDelta, TruncationContent and
// ResponseTruncatedError carry this one value, so a caller checks a single
// constant whichever provider served the turn.
const FinishReasonMaxTokens = "max_tokens"

// NormalizeFinishReason maps every provider spelling of an output-limit stop
// to FinishReasonMaxTokens and returns any other reason unchanged.
func NormalizeFinishReason(reason string) string {
	if IsTruncationFinishReason(reason) {
		return FinishReasonMaxTokens
	}
	return reason
}
