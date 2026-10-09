// Package openai adapts the OpenAI API to the agent Provider and Embedder
// interfaces, including streaming, tool use, structured output, and
// multi-modal content. NewAdapter uses the Chat Completions API and
// NewResponsesAdapter uses the Responses API; both take the same options.
package openai
