// Package agent implements a streaming-first AI agent loop with parallel
// tool execution, sub-agent delegation, human-in-the-loop markers, structured
// output, compaction, and a persistent branching conversation tree.
//
// Construct agents with NewAgent and compose behavior incrementally through
// AgentOption functions. Invoke returns a Stream of typed deltas defined in
// the agent/types package. Provider adapters for Ollama, OpenAI, Anthropic,
// and Google live under agent/provider.
package agent
