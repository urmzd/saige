# eval/harness

Live eval harness for multi-turn LLM corpora. Point it at any OpenAI-compatible `/chat/completions` endpoint and it drives each experiment through one or more flows, writes every generated artifact to disk, and records token, latency, and reliability metrics per experiment. Like the core [`eval`](../README.md) package, it has zero subsystem dependencies.

```go
import "github.com/urmzd/saige/eval/harness"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/eval/harness](https://pkg.go.dev/github.com/urmzd/saige/eval/harness)

## Core Abstractions

| Type | Purpose |
|------|---------|
| `Client` | Metered chat client: retries on 429/5xx, deterministic seed and temperature, usage capture including cached tokens |
| `Experiment` | One multi-turn eval case: named system prompts plus ordered turns (turn 0 synthesizes, later turns edit) |
| `Flow` | Strategy for driving an experiment through the model; built-ins are `BaseFlow` and `StatelessFlow` |
| `Runner` | Runs flows over experiments, skips completed ones, writes `metrics.json` per experiment |
| `DefaultMetrics` | Generic metrics document: per-flow turn metrics, totals, and comparisons against the first flow |

## Corpus Layout

Each subdirectory of the corpus directory is one experiment:

```
<corpus>/<experiment-id>/
  experiment.json   optional: {"format": "text/html", "systems": {"base": "system.md"}}
  system.md         Systems["base"] when experiment.json is absent
  turn-0.md         synthesis prompt (required)
  turn-1.md ...     edit instructions, sorted numerically
```

`format` defaults to `text/markdown` and controls the output file extension. System values in `experiment.json` are file paths relative to the experiment directory, so custom flows can carry additional system prompts under their own names.

## Quick Start

```bash
saige eval init evals            # scaffold a sample corpus in evals/
export OPENAI_API_KEY=sk-...
saige eval run --experiments-dir evals
```

`saige eval run` accepts `--model`, `--api-base` (`SAIGE_EVAL_API_BASE` or `OPENAI_BASE_URL`), `--api-key` (`SAIGE_EVAL_API_KEY` first, then common provider keys), `--flows base,stateless`, `--id` prefix filter, `--count`, and `--force` to re-run experiments that already have a `metrics.json`.

After a run each experiment directory contains:

```
outputs/base/turn-0.md ...       artifacts from the base flow
outputs/stateless/turn-0.md ...  artifacts from the stateless flow
metrics.json                     DefaultMetrics document
```

## Built-in Flows

`BaseFlow` regenerates the full artifact each turn inside one growing conversation. `StatelessFlow` sends only the current artifact plus the edit instruction each turn; when it runs after `BaseFlow` in the same experiment it seeds from the base flow's artifact instead of making its own synthesis call, so the comparison starts from identical content.

## Custom Flows

Implement `Flow` to plug in a protocol-specific strategy, and set `Runner.Assemble` when you need a custom metrics schema:

```go
type MyFlow struct{}

func (MyFlow) Name() string { return "mine" }

func (MyFlow) Run(ctx context.Context, c *harness.Client, exp harness.Experiment, fc *harness.FlowContext) (harness.FlowResult, error) {
    result, err := c.Chat(ctx, []harness.Message{
        {Role: "system", Content: exp.Systems["base"]},
        {Role: "user", Content: exp.Turns[0].Prompt},
    }, harness.WithJSONSchema("my_doc", mySchema))
    if err != nil {
        return harness.FlowResult{}, err
    }
    artifact := harness.CleanArtifact(result.Text)
    // ... run edit turns, collect []harness.TurnResult ...
    return harness.FlowResult{Artifact: artifact, Extra: map[string]any{"parse_rate": 1.0}}, nil
}

runner := &harness.Runner{
    Client: harness.NewClient(apiBase, apiKey, model),
    Flows:  []harness.Flow{harness.BaseFlow{}, MyFlow{}},
}
err := runner.Run(ctx, experiments)
```

`TurnResult.Extra` and `FlowResult.Extra` values are flattened into the metrics JSON as top-level keys, so custom flows extend the schema without forking it. Failure reasons follow a prefix contract consumed by `ComputeReliability`: `envelope parse failed`, `validation failed`, `invalid envelope`, and `apply failed`.

The reference downstream consumer is [generative-artifact-protocol](https://github.com/urmzd/generative-artifact-protocol), which layers its protocol-specific flow and metrics schema on this harness.

## Related

- [`eval`](../README.md): composable scorers, A/B experiments, LLM-as-judge
- [CLI reference](../../cmd/saige/README.md): all `saige` subcommands
- [Root README](../../README.md): project overview and installation
