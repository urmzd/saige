// Package anthropic adapts the Anthropic Messages API to the agent
// Provider interface, including streaming, tool use, extended thinking,
// structured output, and multi-modal content. Anthropic offers no embedding
// API, so pair this provider with an embedder from another provider when
// using RAG or knowledge graph features.
package anthropic
