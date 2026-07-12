# agent

Streaming-first AI agent framework: typed delta events, parallel tool execution, sub-agent delegation, human-in-the-loop markers, conversation tree persistence, and multi-provider resilience.

```go
import "github.com/urmzd/saige/agent"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/agent](https://pkg.go.dev/github.com/urmzd/saige/agent)

## Contents

- [Quick Start](#quick-start)
- [Provider Interface](#provider-interface)
- [Messages](#messages)
- [Deltas](#deltas)
- [Tools](#tools)
- [Sub-Agents](#sub-agents)
- [Markers (Human-in-the-Loop)](#markers-human-in-the-loop)
- [Structured Output](#structured-output)
- [Provider Resilience](#provider-resilience)
- [Compaction](#compaction)
- [Conversation Tree](#conversation-tree)
- [Feedback (RLHF)](#feedback-rlhf)
- [File Pipeline](#file-pipeline)
- [TUI](#tui)
- [Testing](#testing)

## Quick Start

```go
import (
    "github.com/urmzd/saige/agent"
    "github.com/urmzd/saige/agent/types"
    "github.com/urmzd/saige/agent/provider/ollama"
)

client := ollama.NewClient("http://localhost:11434", "qwen2.5", "nomic-embed-text")
a := agent.NewAgent(agent.AgentConfig{
    Name:         "assistant",
    SystemPrompt: "You are a helpful assistant.",
    Provider:     ollama.NewAdapter(client),
    Tools:        types.NewToolRegistry(myTool),
},
    agent.WithMaxIter(20),
    agent.WithLogger(slog.Default()),
)

stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("Hello!")})
for delta := range stream.Deltas() {
    switch d := delta.(type) {
    case types.TextContentDelta:
        fmt.Print(d.Content)
    }
}
```

See [`examples/agent/`](../examples/agent/) for runnable programs covering every feature below.

## Provider Interface

Implement one method to integrate any LLM backend:

```go
type Provider interface {
    ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan Delta, error)
}
```

**Built-in providers:**

| Provider | Package | Structured Output | Content Negotiation | Embedder |
|----------|---------|:-:|:-:|:-:|
| Ollama | `agent/provider/ollama` | yes | JPEG, PNG | yes |
| OpenAI | `agent/provider/openai` | yes | JPEG, PNG, GIF, WebP, PDF | yes |
| Anthropic | `agent/provider/anthropic` | yes | JPEG, PNG, GIF, WebP, PDF | no |
| Google | `agent/provider/google` | yes | JPEG, PNG, GIF, WebP, PDF | yes |

> **Note:** Anthropic does not offer an embedding API. When using Anthropic as your LLM
> provider with RAG or Knowledge Graph features, supply a separate embedder from another
> provider. In the CLI: `--provider anthropic` with an additional API key set
> (e.g. `OPENAI_API_KEY`). In Go code: construct the Anthropic adapter for `Provider`
> and a separate OpenAI/Google/Ollama adapter for the `Embedder`.

## Messages

Three roles. Tool results are content blocks, not a separate role.

| Type | Role | Content Types |
|------|------|---------------|
| `SystemMessage` | system | `TextContent`, `ToolResultContent`, `ConfigContent` |
| `UserMessage` | user | `TextContent`, `ToolResultContent`, `ConfigContent`, `FileContent` |
| `AssistantMessage` | assistant | `TextContent`, `ToolUseContent`, `ThinkingContent` |

`ToolResultContent` carries an `IsError` field that signals whether the text represents an error or a successful result. This distinction is preserved through to the LLM: Anthropic passes it natively, Google uses an `error` key in the function response, and OpenAI/Ollama prefix the text with `[TOOL ERROR]`.

## Deltas

18 concrete types across six categories: LLM-side, thinking, execution-side, marker, feedback, and metadata.

| Type | Category | Purpose |
|------|----------|---------|
| `TextStartDelta` | LLM | Text block opened |
| `TextContentDelta` | LLM | Text chunk |
| `TextEndDelta` | LLM | Text block closed |
| `ThinkingStartDelta` | Thinking | Extended thinking block opened |
| `ThinkingContentDelta` | Thinking | Thinking chunk |
| `ThinkingEndDelta` | Thinking | Thinking block closed (carries signature) |
| `ToolCallStartDelta` | LLM | Tool call generation started |
| `ToolCallArgumentDelta` | LLM | JSON argument chunk |
| `ToolCallEndDelta` | LLM | Tool call complete |
| `ToolExecStartDelta` | Execution | Tool began executing |
| `ToolExecDelta` | Execution | Streaming delta from tool/sub-agent |
| `ToolExecEndDelta` | Execution | Tool finished |
| `MarkerDelta` | Marker | Tool gated pending approval |
| `FeedbackDelta` | Feedback | RLHF rating recorded on a node |
| `UsageDelta` | Metadata | Token usage + wall-clock timing |
| `ErrorDelta` | Terminal | Provider or tool error |
| `DoneDelta` | Terminal | Stream complete |

## Tools

```go
tool := &types.ToolFunc{
    Def: types.ToolDef{
        Name:        "greet",
        Description: "Greet a person",
        Parameters: types.ParameterSchema{
            Type:     "object",
            Required: []string{"name"},
            Properties: map[string]types.PropertyDef{
                "name": {Type: "string", Description: "Person's name"},
            },
        },
    },
    Fn: func(ctx context.Context, args map[string]any) (string, error) {
        return fmt.Sprintf("Hello, %s!", args["name"]), nil
    },
}
```

When the LLM requests multiple tool calls, all tools execute **concurrently**.

## Sub-Agents

Sub-agents are registered as tools and execute within parallel tool dispatch. Their deltas are forwarded through the parent's stream. **Sub-agents are stateless**: a fresh agent is constructed for each delegation, so conversation history is not preserved between calls. This is intentional. Sub-agents are task executors, not persistent conversational partners.

```go
a := agent.NewAgent(agent.AgentConfig{
    Provider: adapter,
    SubAgents: []agent.SubAgentDef{
        {
            Name:         "researcher",
            Description:  "Searches the web for information",
            SystemPrompt: "You are a research assistant.",
            Provider:     adapter,
            Tools:        types.NewToolRegistry(searchTool),
        },
    },
})
```

## Markers (Human-in-the-Loop)

Gate tool execution pending consumer approval:

```go
safeTool := types.WithMarkers(myTool,
    types.Marker{Kind: "human_approval", Message: "This modifies production data."},
)

// Consumer resolves:
stream.ResolveMarker(d.ToolCallID, approved, nil)
```

## Structured Output

Constrain LLM responses to a JSON schema:

```go
schema := types.SchemaFrom[MyResponse]()
a := agent.NewAgent(agent.AgentConfig{
    Provider: adapter,
}, agent.WithResponseSchema(schema))
```

## Provider Resilience

```go
import (
    "github.com/urmzd/saige/agent/provider/retry"
    "github.com/urmzd/saige/agent/provider/fallback"
)

provider := fallback.New(
    retry.New(primary, retry.DefaultConfig()),
    retry.New(backup, retry.DefaultConfig()),
)
```

## Compaction

Data-driven context management:

| Strategy | Behavior |
|----------|----------|
| `CompactNone` | No compaction |
| `CompactSlidingWindow` | Keep system prompt + last N messages |
| `CompactSummarize` | Summarize older messages via the provider |

## Conversation Tree

Persistent branching conversation graph with checkpoints, rewind, and archive. All mutation methods (`AddChild`, `Branch`, `UpdateUserMessage`, `AddFeedback`) accept a `context.Context` for cancellation, deadlines, and tracing, including WAL writes:

```go
tr := a.Tree()
tr.AddChild(ctx, parentID, msg)
tr.Branch(ctx, nodeID, "experiment", msg)
tr.UpdateUserMessage(ctx, nodeID, newMsg)
tr.Checkpoint(branchID, "before-refactor")
tr.Rewind(checkpointID)
```

## Feedback (RLHF)

Attach positive/negative ratings and comments to any node in the conversation tree. Feedback is stored as permanent leaf nodes branching off the target: never sent to the LLM, available for post-analysis and training.

```go
// Rate an assistant response.
tip, _ := a.Tree().Tip(a.Tree().Active())
a.Feedback(ctx, tip.ID, types.RatingPositive, "Clear and helpful")
a.Feedback(ctx, tip.ID, types.RatingNegative, "Too verbose")

// Collect all feedback across the tree.
for _, entry := range a.FeedbackSummary() {
    fmt.Printf("node=%s rating=%d comment=%q\n",
        entry.TargetNodeID, entry.Rating, entry.Comment)
}
```

Feedback nodes have `NodeFeedback` state. They cannot have children added, forming dead-end branches that do not interfere with the conversation flow. During `Replay`, feedback emits `FeedbackDelta` for consumers that track ratings.

## File Pipeline

Automatic URI resolution and content negotiation for multi-modal input:

```go
a := agent.NewAgent(agent.AgentConfig{
    Provider: adapter,
},
    agent.WithResolvers(map[string]types.Resolver{
        "file": myFileResolver,
        "s3":   myS3Resolver,
    }),
    agent.WithExtractors(map[types.MediaType]types.Extractor{
        types.MediaPDF: myPDFExtractor,
    }),
)
```

## TUI

Three modes for streaming agent interaction:

```go
import "github.com/urmzd/saige/agent/tui"

// Non-interactive (works in pipes/CI)
result := tui.StreamVerbose(header, stream.Deltas(), os.Stdout)

// Interactive single-stream (bubbletea)
model := tui.NewStreamModel(header, stream.Deltas())
tea.NewProgram(model).Run()

// Multi-turn conversation loop (reads input, resolves markers, loops until /quit)
runner := &tui.Runner{Title: "My Agent"}
runner.Run(ctx, myAgent)
```

## Testing

```go
import "github.com/urmzd/saige/agent/agenttest"

provider := &agenttest.ScriptedProvider{
    Responses: [][]types.Delta{
        agenttest.ToolCallResponse("id-1", "greet", map[string]any{"name": "Alice"}),
        agenttest.TextResponse("Hello, Alice!"),
    },
}
```

## Related

- [`agent/eval`](eval/): stream timing and tool-call scorers for the [eval framework](../eval/README.md)
- [Root README](../README.md): project overview and installation
