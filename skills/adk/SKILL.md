---
name: adk
description: Build streaming LLM agent loops in Go with typed deltas, tool execution, context compaction, sub-agent delegation, and RLHF feedback. Use when building AI agents, integrating LLM providers, or implementing tool-use patterns.
argument-hint: [task]
---

# adk

Build LLM agent loops using `agent`.

## Quick Start

```go
import (
    "github.com/urmzd/graph-agent-dev-kit/agent"
    "github.com/urmzd/graph-agent-dev-kit/agent/core"
    "github.com/urmzd/graph-agent-dev-kit/agent/provider/ollama"
)

client := ollama.NewClient("http://localhost:11434", "qwen2.5", "nomic-embed-text")
adapter := ollama.NewAdapter(client)

a := agent.NewAgent(agent.AgentConfig{
    Name:         "assistant",
    SystemPrompt: "You are a helpful assistant.",
    Provider:     adapter,
    Tools:        core.NewToolRegistry(),
    MaxIter:      10,
})

stream := a.Invoke(ctx, []core.Message{
    core.NewUserMessage("Hello!"),
})

for delta := range stream.Deltas() {
    if d, ok := delta.(core.TextContentDelta); ok {
        fmt.Print(d.Content)
    }
}
```

## Key Concepts

| Concept | Description |
|---------|-------------|
| **Provider** | Implement `ChatStream` to plug in any LLM backend |
| **Tools** | Register tools via `ToolRegistry`; use `ToolFunc` for inline definitions |
| **Compaction** | Configure via `CompactCfg: &core.CompactConfig{Strategy: core.CompactNone\|Sliding\|Summarize}` |
| **Sub-agents** | Delegate tasks to child agents with their own providers and tools |
| **File Upload** | Attach files via `core.NewFileMessage(uri)` or `core.NewUserMessageWithFiles(text, files...)`; URIs are resolved by `Resolvers` and extracted by `Extractors` in `AgentConfig` |
| **Embeddings** | `core.Embedder` interface; `ollama.NewEmbedder(client)` for Ollama-backed vector embeddings |
| **Feedback** | `a.Feedback(nodeID, core.RatingPositive, "comment")` — attach RLHF ratings as permanent leaf nodes |

## Feedback (RLHF)

```go
// Rate an assistant response — creates a dead-end branch off the target node.
tip, _ := a.Tree().Tip(a.Tree().Active())
a.Feedback(tip.ID, core.RatingPositive, "Clear and helpful")

// Collect all feedback across the tree.
for _, entry := range a.FeedbackSummary() {
    fmt.Printf("node=%s rating=%d comment=%q\n",
        entry.TargetNodeID, entry.Rating, entry.Comment)
}
```

## Adding a Tool

```go
tool := &core.ToolFunc{
    Def: core.ToolDef{
        Name: "greet", Description: "Greet a person",
        Parameters: core.ParameterSchema{
            Type: "object", Required: []string{"name"},
            Properties: map[string]core.PropertyDef{
                "name": {Type: "string", Description: "Person's name"},
            },
        },
    },
    Fn: func(ctx context.Context, args map[string]any) (string, error) {
        return fmt.Sprintf("Hello, %s!", args["name"]), nil
    },
}
registry := core.NewToolRegistry(tool)
```
