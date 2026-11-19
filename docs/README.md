# Run your first tool call

Saige is a Go SDK for building agents. In this example an agent calls a weather function and returns its answer. A local test model chooses the tool, so you can run the whole conversation without credentials.

## Run the example

Install Go 1.26.11 or newer, then run these commands.

```sh
git clone https://github.com/urmzd/saige.git
cd saige
go test -v -count=1 ./agent/agenttest \
  -run '^ExampleNewToolCallEmulator$'
```

Look for `--- PASS: ExampleNewToolCallEmulator`. Go checks the agent’s printed answer against `weather: sunny in sample city`. A different answer makes the example fail.

## Follow the call

The example defines the tool as a Go function. Its input declares the argument the model supplies, and its return value becomes the tool result.

```go
weather := agent.Func("weather", "current weather",
    func(_ agent.RunContext[agent.NoDeps], in struct {
        City string `json:"city"`
    }) (string, error) {
        return "sunny in " + in.City, nil
    })
```

1. **Register the tool.** `types.NewToolRegistry(weather)` makes this function available to the agent.
2. **Run one turn.** `Invoke` receives a user message. The emulator requests `weather` with the city set to `sample city`. The agent executes the function and supplies its result to the emulator.
3. **Read the answer.** `agent.CollectText` drains the stream and returns the final text and any error.

[Read the executable Go example](https://github.com/urmzd/saige/blob/main/agent/agenttest/example_test.go#L29-L40) · [Define your own typed tool](guides/func-tools.md)

## Use a model

With a model provider configured, run `go run ./examples/quickstart/tool` from the same checkout. This version registers a weather function and asks about Lisbon. Its catalog preset selects an available adapter. Provider, catalog and tool remain separate so you can change where the model runs while keeping the function.

[Read the model-backed example](https://github.com/urmzd/saige/blob/main/examples/quickstart/tool/main.go) · [Configure model presets](reference/catalog.md) · [Browse all guides and decisions](guide-index.md)
