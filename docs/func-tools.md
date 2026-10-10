# Typed function tools

`agent.Func` builds a tool from a Go function with a typed input, a typed output, and typed host dependencies.
`agent.AIFunc` builds a typed function that a model computes, and exposes it as a tool.

## Func

```go
type Inventory struct{ db *sql.DB }

type ReserveIn struct {
    SKU      string `json:"sku" description:"Product SKU"`
    Quantity int    `json:"quantity" description:"Units to reserve"`
    Note     string `json:"note,omitempty"`
}

reserve := agent.Func("reserve", "Reserve units of a SKU",
    func(rc agent.RunContext[*Inventory], in ReserveIn) (Receipt, error) {
        return reserveStock(rc, rc.Deps.db, in, rc.IdempotencyKey)
    },
    agent.Capability(types.ToolCapabilityWrite),
    agent.Approval("Reserve stock"),
    agent.Idempotent(),
)

a, err := agent.New(cfg, agent.WithDeps(&Inventory{db: db}))
if err != nil {
    return err
}
```

| Behavior | Rule |
|---|---|
| Schema | Derived from `In` with `types.SchemaFrom`. `json`, `description` and `enum` tags apply. A field without `omitempty` is required. `In` must be a struct. |
| Arguments | Decoded strictly into `In`. An unknown property or a value of the wrong type is a tool error that wraps `types.ErrInvalidToolArguments`, so the model can correct the call. |
| Result | A `string` is returned as is. Any other `Out` is encoded as JSON. |
| Dependencies | `RunContext.Deps` comes from `agent.ContextWithDeps` on the run's context, else from `agent.WithDeps`. A missing or mistyped value is a tool error. Use `agent.NoDeps` for a function that needs none. |

Options:

| Option | Effect |
|---|---|
| `agent.Capability(c)` | Sets `ToolDef.Capability`, which capability gates and approval policies judge. |
| `agent.Approval(msg)` / `agent.Markers(...)` | Wraps the tool in a `types.MarkedTool`, so every call waits for a decision. |
| `agent.Idempotent()` | Declares `types.IdempotentTool`. The local durable engine repeats such a call after a crash instead of waiting for `Reconcile`. |

## RunContext

`RunContext[D]` embeds the call's `context.Context` and adds:

| Field | Source |
|---|---|
| `Deps` | Host dependencies, typed as `D` |
| `Call` | Tool call ID, tool name, owning agent, root run ID (`RunID()`), branch (`Branch()`) |
| `Workspace` | The run's workspace, nil when none is configured |
| `IdempotencyKey` | The durable runner's key for this step, stable across replays. Empty without a durable runner that supplies one. |
| `Approval` | Whether a gate or marker held the call, the approver the host named in `Resolution.Approver`, and the grant that approved it, if any |
| `Knobs` | The agent's `types.ToolContext` |

`types.ToolContext` keeps working for existing tools. `agent.NewRunContext[D](ctx)` builds the same value inside any tool.

## Versions

A Func tool reports a version: `types.DefinitionHash` of its name, description, parameter schema and capability.
Changing `In` changes the version.

- `registry.ToolSet.Register` registers a versioned tool with that version. Registering it again unchanged returns the current revision. A changed schema adds a revision. `ToolSet.AtVersion` finds the revision a transcript names.
- The loop records the version in `ToolExecEndDelta.Version` and `ToolResultPart.ToolVersion`, so the tree names the schema that produced each result.
- `eval.AgentRun.AddProvenance` records versions in `eval.Provenance.Tools`. `eval.ConfigDrift` reports a tool that ran at different versions in two runs.

## AIFunc

```go
type Ticket struct{ Body string `json:"body"` }
type Triage struct{ Priority string `json:"priority" enum:"low,high"` }

triage, err := agent.AIFunc[Ticket, Triage]("triage", "Triage a support ticket", agent.AIConfig{
    Prompt: "Triage this ticket:\n{{.Body}}",
    Preset: bundle, // or Provider: adapter
    Repair: 1,
})
out, err := triage.Call(ctx, Ticket{Body: "Checkout returns 500"})
tool := triage.Tool()
```

- The prompt is a `text/template` rendered with the input. A missing field is an error.
- Each call runs `agent.Structured[Out]` on a new agent, so calls are independent and safe to run concurrently.
- `Out` must be a struct. Its schema constrains the answer. `Repair` re-asks the model after an invalid answer.
- The version hashes the prompt, the system prompt, both schemas, the output mode and the configuration hash. The configuration hash is `AIConfig.ConfigHash`, else the preset's configuration key (a `preset.Bundle` reports one), else the preset name and catalog revision, else the provider name and model.
- `Tool()` declares the read capability and reports the AIFunc version, so the tree and eval provenance record it like a Func version.

See [`examples/agent/func-tools`](../examples/agent/func-tools/).
