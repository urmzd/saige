# eval/harness

Live eval harness for multi-turn LLM corpora. Point it at any OpenAI-compatible `/chat/completions` endpoint, or at any saige provider, and it drives each script through one or more flows, writes every generated artifact to disk, and records token, latency, and reliability metrics per script. Its only dependencies outside `eval` are `agent/types` (the provider interface and error taxonomy) and `agent/provider/catalog` (model capabilities).

```go
import "github.com/urmzd/saige/eval/harness"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/eval/harness](https://pkg.go.dev/github.com/urmzd/saige/eval/harness)

## Core Abstractions

| Type | Purpose |
|------|---------|
| `Client` | Metered chat client with two transports: raw HTTP to an OpenAI-compatible API (`NewClient`), or any `types.Provider` (`NewProviderClient`). Usage capture includes cached tokens |
| `Manifest` | Strict JSON suite manifest (`saige.eval.json`): corpus, subject, flows, policy, and assertions |
| `Issue` | One validation problem, addressed by file and JSON Pointer, with a severity |
| `Script` | One scripted multi-turn eval case: named system prompts plus ordered turns (turn 0 synthesizes, later turns edit). `Experiment` is its deprecated former name |
| `Flow` | Strategy for driving a script through the model; built-ins are `BaseFlow` and `StatelessFlow`. The flows of a runner act as its variants |
| `Runner` | Runs flows over scripts with bounded concurrency, skips completed ones, writes `metrics.json` per script, and can resume a stored run and gate the result |
| `DefaultMetrics` | Generic metrics document: per-flow turn metrics, totals, and comparisons against the first flow over the edit turns both flows completed |

## Corpus Layout

Each subdirectory of the corpus directory is one script:

```
<corpus>/<script-id>/
  script.json       optional: {"format": "text/html", "systems": {"base": "system.md"}}
  system.md         Systems["base"] when script.json is absent
  turn-0.md         synthesis prompt (required)
  turn-1.md ...     edit instructions, sorted numerically
```

`format` defaults to `text/markdown` and controls the output file extension. System values in `script.json` are file paths relative to the script directory, so custom flows can carry additional system prompts under their own names. A directory with only the former `experiment.json` still loads. The config file is decoded strictly: an unknown key fails the load instead of being ignored, and two files for one turn index (`turn-1.md` and `turn-01.md`) are rejected. `FilterScripts` (formerly `FilterExperiments`) selects scripts by ID prefix and count.

`ValidateCorpus` checks a corpus without running it and returns every problem, not only the first: unknown config keys with their JSON Pointer, missing system files, a missing `turn-0.md`, duplicate turn indices, and, as warnings, gaps in the turn numbering.

## Quick Start

```bash
saige eval init evals                                  # sample corpus plus evals/saige.eval.json
export OPENAI_API_KEY=sk-...
saige eval validate evals                              # strict, offline checks
saige eval run --manifest evals/saige.eval.json --dry-run
saige eval run --manifest evals/saige.eval.json
```

`saige eval run` works with or without a manifest. Flags: `--manifest`, `--experiments-dir`, `--model`, `--api-base`, `--api-key`, `--flows base,stateless`, `--id` prefix filter, `--count`, `--force` to re-run scripts that already have a `metrics.json`, `--continue-on-error` (default true), `--concurrency N`, `--store DIR` with `--suite NAME` to record the run, `--resume RUN` to continue a stored run, `--assert` to add a gate, `--dry-run` to stop after the checks and print the plan, and `--allow-unknown-model`. With a manifest, only the flags that are set override it. The root `--provider` flag (when set) selects a saige provider instead of the OpenAI-compatible endpoint.

Before any model call the command validates the manifest and corpus, checks that a key is available, and checks the model against the catalog. A model that matches no catalog family is refused for `openai` (including the OpenAI API host), `anthropic`, and `google` unless `--allow-unknown-model` is passed. The command exits with status 1 when a script fails or an assertion is violated, and with status 3 when the run is inconclusive (see [Failures](#failures)).

### API keys

For the OpenAI-compatible endpoint the key is, in order: `--api-key`, the manifest's `api_key_env`, `$SAIGE_EVAL_API_KEY`, `$OPENAI_API_KEY` when the base URL came from `$OPENAI_BASE_URL`, and then the variable that belongs to the endpoint's host:

| Host | Variable |
|------|----------|
| `api.openai.com` | `OPENAI_API_KEY` |
| `generativelanguage.googleapis.com` | `GEMINI_API_KEY`, then `GOOGLE_API_KEY` |
| `api.groq.com` | `GROQ_API_KEY` |
| `openrouter.ai` | `OPENROUTER_API_KEY` |
| `api.mistral.ai` | `MISTRAL_API_KEY` |
| `models.github.ai`, `models.inference.ai.azure.com` | `GITHUB_TOKEN` |

A key for one vendor is never sent to another vendor's host. Any other host needs `--api-key`, `$SAIGE_EVAL_API_KEY`, or `api_key_env`. The base URL is `--api-base`, the manifest's `base_url`, `$SAIGE_EVAL_API_BASE`, `$OPENAI_BASE_URL`, or `https://api.openai.com/v1`.

A manifest may name with `api_key_env` only the host's own variable from the table or a `SAIGE_EVAL_*` variable, so it cannot send an unrelated secret. A manifest is loaded from the working directory by default, so its `base_url` is not trusted with your keys: when the base URL comes from the manifest, only `--api-key` and the host's own variable are sent. To send `$SAIGE_EVAL_API_KEY` or a `SAIGE_EVAL_*` variable to that host, confirm it with `--api-base`.

After a run each script directory contains:

```
outputs/base/turn-0.md ...       artifacts from the base flow
outputs/stateless/turn-0.md ...  artifacts from the stateless flow
metrics.json                     DefaultMetrics document
error.json                       written instead when the script failed
```

## Suite Manifest

A manifest declares one suite in JSON. It is decoded strictly: an unknown key is an error with its JSON Pointer and the closest known key, so a typo cannot be ignored. Relative paths resolve against the manifest's directory. The manifest never holds a secret: `api_key_env` names the variable that holds the key.

```json
{
  "version": 1,
  "name": "readme-edits",
  "corpus": "./evals",
  "flows": ["base", "stateless"],
  "subject": {
    "provider": "openai-compatible",
    "model": "llama-3.1-8b-instant",
    "base_url": "https://api.groq.com/openai/v1",
    "api_key_env": "GROQ_API_KEY"
  },
  "policy": {"concurrency": 4, "continue_on_error": true},
  "assert": [
    {"metric": "turn_succeeded", "op": ">=", "threshold": 1},
    {"metric": "latency_ms", "op": "<=", "threshold": 5000, "scope": "aggregate"}
  ],
  "store": "./eval-results"
}
```

| Key | Meaning |
|-----|---------|
| `version` | Required, `1` |
| `corpus` | Required, the corpus directory |
| `name` | Suite name for the recorded run |
| `id_prefix`, `count` | Script selection, as `--id` and `--count` |
| `flows` | Built-in flows in order; default `base`, `stateless` |
| `subject.provider` | `openai-compatible` (default), `openai`, `anthropic`, `google`, or `ollama` |
| `subject.model` | Model name; a model the catalog does not list is a warning in `validate` |
| `subject.base_url` | API base for `openai-compatible` and `openai` |
| `subject.api_key_env` | Key variable for `openai-compatible`; the other providers read their own (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GOOGLE_API_KEY`) |
| `policy.concurrency` | Scripts run at once; default 1 |
| `policy.continue_on_error` | Keep going after a script fails; default true |
| `assert` | `eval.Assertion` gates over the turn metrics (`turn_succeeded`, `latency_ms`, `input_tokens`, `output_tokens`, `output_bytes`) |
| `store` | Results store directory |

`LoadManifest` returns the manifest with a list of `Issue` values; `saige eval validate [path]` prints them as `file:/pointer: severity: message` and fails on any error. `path` is a manifest, a directory holding `saige.eval.json`, or a bare corpus directory.

## Providers

`NewProviderClient(p)` sends every request through a saige `types.Provider`, so the provider's adapter, retry, fallback, and routing decorators apply. The provider transport makes one attempt per call and leaves retries to the provider stack (`saige eval run` wraps the provider in a 6-attempt retry decorator). `WithJSONSchema` needs a `types.StructuredOutputProvider`, and a `Temperature` or `Seed` set on the client needs a `types.OptionsProvider`; a provider without them fails the call rather than dropping the request. `NewProviderClient` leaves both unset, so the provider's configured sampling applies.

`NewClient` sends temperature 0 unless the model catalog says the model rejects it with its default reasoning settings (`AcceptsTemperature`). Reasoning models such as `o3` get no temperature; a model the catalog does not know resolves to the OpenAI baseline, which accepts it.

## Failures

On the HTTP transport, failures are classified with the shared provider error taxonomy in `agent/types`. Rate limits, unavailable or overloaded servers, request timeouts, and transient transport errors are retried up to 6 attempts. A `Retry-After` (or `Retry-After-Ms`) header sets the wait, capped at 60 seconds; otherwise the wait doubles from 1 to 16 seconds. Authentication errors, invalid requests, and context-length errors are not retried. The returned error is a `*types.ProviderError`, so `types.KindOf`, `types.IsContextLength`, and the other helpers work on it. Error messages embed at most 2 KiB of the response body.

When an edit turn's request still fails, the built-in flows record that turn with `failed: true` and a `failure_reason` starting with `request failed`, then continue from the last good artifact. A failed turn-0 request fails the flow, since there is nothing to edit.

A failed flow fails its script: the runner writes `<script>/error.json` (`experiment_id`, `flow`, `error`, `timestamp`) and no `metrics.json`, so the next run retries it. With `Runner.ContinueOnError` the remaining scripts still run and all failures are returned together; without it the run stops at the first failure.

A failure on infrastructure is inconclusive, not failed: `eval.IsInfra` reports rate limits, unavailable servers, other retryable statuses, authentication failures, network failures, timeouts, cancellation, and errors marked with `eval.Infra`. An edit turn lost that way is recorded with `inconclusive: true`, and its `turn_succeeded` score carries the error and `Inconclusive` instead of a 0, so a gate leaves it out instead of failing it. A script lost that way is recorded as a subject error marked inconclusive. When no script failed for another reason and no gate failed, `Run` returns an `*InconclusiveError` (`errors.Is(err, harness.ErrInconclusive)`) if more than `Runner.MaxInconclusive` of the scripts are inconclusive (default 0, none tolerated), if the gate was inconclusive, or if the run was cancelled. Within the tolerance it returns nil. `saige eval run` exits with status 3 for an inconclusive run; `--resume` reruns only the scripts that did not complete.

`DefaultMetrics` includes a `reliability` report per flow that has edit turns, built by `ComputeReliability`. Its `provider` field names the client's transport.

## Concurrency, Resume, and Gates

`Runner.Concurrency` runs that many scripts at once. Units are recorded in script order whatever the timing. Without `ContinueOnError`, a failure stops new scripts from starting while scripts already running finish.

`Runner.Resume` names a stored run to continue. Scripts that completed there (every flow recorded, no script failure) are not run again: their stored units are copied into the new run. The other scripts run even when a `metrics.json` exists. The new run gets its own ID and is complete on its own.

`Runner.Assert` gates the recorded units with `eval.SuiteResult.GateWith` and a `GatePolicy` of `Runner.MaxInconclusive`. A violated gate makes `Run` return an `*AssertionError` (`errors.Is(err, harness.ErrAssertionsFailed)`), and a stored run carries the outcome and violations. A failed gate does not mark the run itself as errored. Scripts skipped because their `metrics.json` exists add no units unless `Runner.ReuseMetrics` is set, which rebuilds them from the file when it records the same model. `saige eval run` sets it, so a rerun gates the whole corpus without new calls.

`Runner.Plan` reports what `Run` would do with each script (run, skip, or resume) and how many chat calls it would make. `saige eval run --dry-run` prints it.

## Recording Runs

Set `Runner.Results` to an `eval/store` store to record each `Run` as one stored run, with provenance from `Runner.Provenance` plus the client's model. Every executed script adds one unit per flow per turn, keyed like `001-example/1@stateless`, with the turn's latency and tokens as timing and the scores `turn_succeeded`, `latency_ms`, `input_tokens`, `output_tokens`, and `output_bytes` (see `ScriptObservations`). A failed script adds one unit carrying its error, counted as a subject error. Skipped scripts add nothing, so pass `--force` to record a full rerun.

```bash
saige eval run --experiments-dir evals --store eval-results --suite docs
saige eval runs --store eval-results
```

## Built-in Flows

`BaseFlow` regenerates the full artifact each turn inside one growing conversation. `StatelessFlow` sends only the current artifact plus the edit instruction each turn; when it runs after `BaseFlow` in the same script it seeds from the base flow's artifact instead of making its own synthesis call, so the comparison starts from identical content.

## Custom Flows

Implement `Flow` to plug in a protocol-specific strategy, and set `Runner.Assemble` when you need a custom metrics schema:

```go
type MyFlow struct{}

func (MyFlow) Name() string { return "mine" }

func (MyFlow) Run(ctx context.Context, c *harness.Client, script harness.Script, fc *harness.FlowContext) (harness.FlowResult, error) {
    result, err := c.Chat(ctx, []harness.Message{
        {Role: "system", Content: script.Systems["base"]},
        {Role: "user", Content: script.Turns[0].Prompt},
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
err := runner.Run(ctx, scripts)
```

Custom flows build their turn records with `NewTurnMetrics`, `NewTurnResult`, and `NewFailedTurnResult`, so they match the built-in flows. A turn that takes several calls sums them with `ChatResult.Add`.

`Client.ChatWithRepair` runs a validate-and-repair loop: it validates each answer with a `Validator`, and while validation fails and `RepairOptions.MaxRepairs` allows, it appends the rejected answer and a repair request and asks again. It returns a `RepairResult` with the summed usage, the number of repairs, and the last validation error. `RepairResult.ApplyTo` records the repairs on a `TurnResult` and marks a turn that never validated as `validation failed`.

```go
start := time.Now()
rr, err := c.ChatWithRepair(ctx, messages, func(text string) error {
    return json.Unmarshal([]byte(harness.ExtractJSONObject(text)), new(map[string]any))
}, harness.RepairOptions{MaxRepairs: 2})
if err != nil {
    turns = append(turns, harness.NewFailedTurnResult(turn, start, err))
} else {
    tr := harness.NewTurnResult(turn, rr.Result, start, rr.Result.Text)
    rr.ApplyTo(&tr)
    turns = append(turns, tr)
}
```

`TurnResult.Extra` and `FlowResult.Extra` values are flattened into the metrics JSON as top-level keys, so custom flows extend the schema without forking it. Failure reasons follow a prefix contract consumed by `ComputeReliability`: `request failed`, `envelope parse failed`, `validation failed`, `invalid envelope`, and `apply failed`.

The reference downstream consumer is [generative-artifact-protocol](https://github.com/urmzd/generative-artifact-protocol), which layers its protocol-specific flow and metrics schema on this harness.

## Related

- [`eval`](../README.md): composable scorers, gates, comparisons, experiments, LLM-as-judge
- [CLI reference](../../cmd/saige/README.md): all `saige` subcommands
- [Root README](../../README.md): project overview and installation
