package types

// CapAssistantPrefill means a request may end with a partial assistant turn, and
// the model continues that turn instead of starting a new one. The agent uses
// it to resume a turn cut short by a stop request or the output token limit.
const CapAssistantPrefill Capability = "assistant_prefill"
