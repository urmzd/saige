---
name: agent
description: Build streaming LLM agent loops in Go with typed parts and part deltas, tool execution, media input, context compaction, sub-agent delegation, and RLHF feedback. Use when building AI agents, integrating LLM providers, or implementing tool-use patterns.
metadata:
  argument-hint: [task]
---

# agent

Build LLM agent loops using `saige/agent`.

## Quick Start

A catalog preset builds the adapters. `default` serves the cheapest model of each vendor whose credentials are set, then a model pulled into a local Ollama.

```go
import (
    "github.com/urmzd/saige/agent"
    "github.com/urmzd/saige/agent/provider/catalog"
    "github.com/urmzd/saige/agent/provider/preset"
    "github.com/urmzd/saige/agent/types"
)

bundle, err := preset.Build(ctx, catalog.Default(), "default", nil, preset.Options{})
if err != nil {
    return err
}
defer bundle.Close(ctx)

a, err := agent.New(agent.Config{SystemPrompt: "Answer in one sentence."}, agent.WithPreset(bundle))
if err != nil {
    return err
}

stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("Hello!"))})
for delta := range stream.Deltas() {
    if d, ok := delta.(types.PartDelta); ok {
        fmt.Print(d.Text)
    }
}
if err := stream.Wait(); err != nil {
    return err
}
```

`agent.CollectText(a.Invoke(...))` returns the final text instead.

## One adapter

Every constructor takes a `Config` and options and returns an error. Types that own resources (bundles, routers, MCP pools, stores) close with `Close(ctx)`; `types.CloseProvider(ctx, p)` closes a provider stack that owns any.

```go
import "github.com/urmzd/saige/agent/provider/ollama"

adapter, err := ollama.New(ollama.Config{Host: "http://localhost:11434", Model: "qwen3.5:4b"})
if err != nil {
    return err
}
a, err := agent.New(agent.Config{Provider: adapter, MaxIter: 10})
```

The other adapters: `anthropic.New(anthropic.Config{APIKey: key, Model: "claude-haiku-5-5"})`, `openai.New` (Chat Completions) and `openai.NewResponses` (Responses API) with `openai.Config`, and `google.New(ctx, google.Config{...})`. `provider.Build(ctx, provider.Config{Provider: "openai", Model: "gpt-6-luna"})` picks the adapter and serving API from the catalog.

## Key Concepts

| Concept | Description |
|---------|-------------|
| **Messages** | Ordered typed parts: `types.UserMsg(types.Text("hi"))`, `types.SystemMsg`, `types.AssistantMsg`, `types.ToolResults`. The role seals which part kinds a message holds |
| **Media** | A part with a `Source`: `types.Image(types.Bytes(types.MediaPNG, png))`, `types.Document(types.URL("https://...", types.MediaPDF))`; `types.Media(src)` picks the kind by media type |
| **Output** | Streams as `PartStart`, `PartDelta` and `PartEnd`, each with the part's `Index` in the final message. `types.NewPartAssembler()` rebuilds the turn |
| **Provider** | Implement `Stream(ctx, types.Request) (<-chan types.Delta, error)`; `Request` carries messages, tools, an optional schema and options |
| **Tools** | `agent.Func` derives a schema from a struct; `types.ToolFunc` for an inline tool; register them in `types.NewToolRegistry` |
| **Conversion** | Media the serving model cannot take is rejected unless a modality action is permitted (`extract`, `describe`, `transcribe`, `omit`) |
| **Targets** | Typed IDs (`types.ModelID`, `ProfileID`, `PresetName`, `ProviderName`); switch with `types.ConfigPart{Target: types.PresetTarget("fast")}` or `p.WithTarget(types.ModelTarget(m))` |
| **Compaction** | `CompactCfg: &types.CompactConfig{Strategy: types.CompactSummarize}`; see the strategies in `docs/context-management.md` |
| **Sub-agents** | `SubAgents: []agent.SubAgentDef{...}` gives the model `delegate_to_<name>` tools; handoffs with `agent.WithHandoffs` |
| **Embeddings** | `types.Embedder`; `ollama.NewEmbedder(client)` for Ollama-backed vectors |
| **Feedback** | `a.Feedback(ctx, nodeID, types.RatingPositive, "comment")` attaches an RLHF rating as a leaf node |

## Adding a Tool

```go
type GreetIn struct {
    Name string `json:"name" description:"Person's name"`
}

greet := agent.Func("greet", "Greet a person",
    func(rc agent.RunContext[agent.NoDeps], in GreetIn) (string, error) {
        return "Hello, " + in.Name + "!", nil
    })

a, err := agent.New(agent.Config{Provider: adapter, Tools: types.NewToolRegistry(greet)})
```

An untyped tool:

```go
tool := &types.ToolFunc{
    Def: types.ToolDef{
        Name: "greet", Description: "Greet a person",
        Parameters: types.ParameterSchema{
            Type: "object", Required: []string{"name"},
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

A tool that returns media or JSON implements `types.RichTool`, whose `ExecuteRich` returns `types.ToolResult{Parts: []types.ToolOutputPart{types.Text(s), img}}`.

## Sending media

```go
png, err := os.ReadFile("chart.png")
if err != nil {
    return err
}
msg := types.UserMsg(
    types.Text("What does this chart show?"),
    types.Image(types.Bytes(types.MediaPNG, png)),
)
```

To send a PDF to a model that takes no documents, permit extraction:

```go
a, err := agent.New(agent.Config{Provider: adapter},
    agent.WithConversion(types.ConversionPolicy{
        Dial:       types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{types.ModalityDocument: {types.ActExtract}}},
        Converters: []types.Converter{convert.Documents()},
    }))
```

Without a permitted action the request fails with an error matching `types.ErrModalityUnsupported` that names the part.

## Implementing a Provider

```go
type echo struct{}

func (echo) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
    ch := make(chan types.Delta, 4)
    go func() {
        defer close(ch)
        text := "echo: " + types.TextOf(req.Messages[len(req.Messages)-1])
        for _, d := range types.PartDeltas(0, types.Text(text)) {
            ch <- d
        }
        ch <- types.DoneDelta{}
    }()
    return ch, nil
}
```

A decorator embeds `wrapper.Base` (`wrapper.NewBase(inner, rewrap)`) so it forwards names, capabilities, `WithTarget` and `Close` without writing them.

## Feedback (RLHF)

```go
// Rate an assistant response: creates a dead-end branch off the target node.
tip, err := a.Tree().Tip(a.Tree().Active())
if err != nil {
    return err
}
if _, err := a.Feedback(ctx, tip.ID, types.RatingPositive, "Clear and helpful"); err != nil {
    return err
}

// Collect all feedback across the tree.
for _, entry := range a.FeedbackSummary() {
    fmt.Printf("node=%s rating=%d comment=%q\n",
        entry.TargetNodeID, entry.Rating, entry.Comment)
}
```

## Further reading

`docs/parts.md` (parts, sources, part deltas, wire versions), `docs/modality-conversion.md`, `docs/catalog.md`, `docs/dials.md`, `docs/tool-calling.md`, `docs/delegation.md` and `docs/upgrade-notes.md` in the saige repository.
