# eval

Composable evaluation framework that works across all saige subsystems. The core `eval` package has zero subsystem dependencies. Subsystem-specific scorers live alongside their domains: [`agent/eval`](../agent/eval/), [`rag/knowledge/eval`](../rag/knowledge/eval/), [`rag/eval`](../rag/eval/).

```go
import "github.com/urmzd/saige/eval"
```

Full API reference: [pkg.go.dev/github.com/urmzd/saige/eval](https://pkg.go.dev/github.com/urmzd/saige/eval)

## Core Abstractions

| Type | Purpose |
|------|---------|
| `Observation` | Universal eval case: Input, Output, GroundTruth as `json.RawMessage`, typed Annotations map |
| `Scorer` | Interface computing a named metric from an Observation |
| `Subject` | Function that populates an Observation's Output and Annotations |
| `Score` | Named metric value with optional reason and gate verdict (`Passed`) |
| `Labels`, `Where` | Case and variant coordinates on an Observation, and a label selector that scopes assertions, scorers, and result slices |
| `Assertion`, `Outcome` | A pass/fail gate on one metric, and the suite's three-valued answer: passed, failed, or unasserted |
| `Experiment`, `Variant`, `Matrix` | Intent object: a claim, the variants under test, scorers, and assertions; run against a dataset |
| `Comparison` | Paired comparison of two arms: deltas with intervals, pass-rate comparison, per-case diff |
| `GroupBy`, `Stat` | Grouped means and pass rates with intervals and completeness |
| `RunRecord`, `Unit` | Stored form of a run and of each scored observation, shared by every subsystem's evals |
| `Registry`, `ScorerSpec` | Scorers declared by kind and parameters instead of code |

## Built-in Scorers

**Text Quality** (pure functions, no LLM):

| Scorer | Description |
|--------|-------------|
| `SequenceSimilarityScorer` | Character-level LCS ratio between output and ground truth |
| `TokenF1Scorer` | Word-token precision/recall/F1 |
| `RougeLScorer` | ROUGE-L F1 at the token level |

**Checks and output contracts** (deterministic; each scores 1 or 0 with a reason):

| Scorer | Metric | Description |
|--------|--------|-------------|
| `JSONSchemaScorer(schema)` | `json_schema` | Output validates against a JSON Schema |
| `JSONFieldScorer(path, want)` | `json_field:<path>` | Output field at a dot path (`answer.city`, `items.0.id`) equals a value |
| `JSONFieldGroundTruthScorer(path)` | `json_field:<path>` | Same, with the expected value read from the ground truth |
| `RegexCountScorer(pattern, op, n)` | `regex_count:<pattern><op><n>` | Number of pattern matches in the output text compared with `op` |
| `ContainsScorer(substrings...)` | `contains:<substrings>` | Output text contains every substring |
| `ExactMatchScorer` | `exact_match` | Output text equals the ground truth after trimming whitespace |
| `NewCheckScorer(name, fn)` | `<name>` | Any predicate over the observation, as an oracle |

JSON checks read the output with `OutputJSON`: a JSON string output (the usual shape of an LLM reply) is decoded again as JSON, after removing a surrounding code fence. Output that is not JSON fails the check; it is not a scorer error. `JSONSchema` supports the keywords that describe a reply's shape (`type`, `enum`, `const`, `required`, `properties`, `additionalProperties`, `items`, length and range bounds, `pattern`, `allOf`, `anyOf`, `oneOf`, `not`, `nullable`) plus the annotation keywords (`$schema`, `$id`, `$comment`, `$defs`, `definitions`, `title`, `description`, `examples`, `default`, `deprecated`, `readOnly`, `writeOnly`). It rejects any other keyword, such as `$ref`, `format`, or a misspelling like `requried`, so a schema never checks less than it appears to.

**Budgets** (deterministic; decline an observation that did not record the measure):

| Scorer | Metric | Description |
|--------|--------|-------------|
| `TokenBudgetScorer(max)` | `token_budget` | Input plus output tokens at most `max` |
| `CostBudgetScorer(maxUSD)` | `cost_budget` | `Timing.CostUSD` at most `maxUSD` |
| `RespondsWithinScorer(limit)` | `responds_within` | `Timing.TotalMs` at most `limit` |
| `CostScorer` | `cost_usd` | Reports the recorded cost |
| `TotalTokensScorer` | `total_tokens` | Reports input plus output tokens |

`ObservationTiming.CostUSD` is a pointer: nil means the subject recorded no cost, which is never reported as free. Set it with `obs.Timing.SetCostUSD(usd)`, or with `AgentRun.Priced` in an agent subject.

To report two configurations of one scorer as separate metrics, rename one: `eval.Named(eval.TokenBudgetScorer(500), "tight_budget")`. A `*ScorerFunc` also has `WithName`.

**LLM-as-Judge**:

| Scorer | Description |
|--------|-------------|
| `NewJudgeScorer` | Pointwise scoring with customizable rubric |
| `NewPairwiseJudgeScorer` | A/B comparison between two outputs, asked in both orders to cancel position bias |

Judges ask for a JSON verdict, `{"reasoning": "...", "score": 0.8}`, described by `eval.JudgeSchema`. A generator that implements `StructuredGenerator` receives the schema and can constrain the reply to it; `agenteval.NewGenerator(provider)` does this for any agent provider with structured output. The parser reads the first JSON object with a score, inside a code fence or after prose, and falls back to `SCORE:` and `REASONING:` lines, accepting `SCORE: 0.8`, `**SCORE:** 0.8`, `Score = 0.8`, and `SCORE: 8/10`. A reply with no parseable score (a refusal, a truncated reply, `NaN`) is an errored score wrapping `ErrNoJudgeScore`, not a 0.0.

The pairwise judge asks twice by default, with the base response first and then second, and averages the two verdicts. This doubles judge calls. Both verdicts land in `Score.Samples`; when they disagree about which response is better, `Samples.Stable` is false and the reason says `position-inconsistent`. Use `eval.WithPositionSwap(false)` for one call in the fixed order.

**Agent** ([`agent/eval`](../agent/eval/)):

| Scorer | Description |
|--------|-------------|
| `TTFTScorer` | Time to first token (ms) |
| `TTLTScorer` | Time to last token (ms) |
| `MedianITLScorer` | Median inter-token latency (ms) |
| `ToolCallCountScorer` | Number of tool calls |
| `ToolSuccessRateScorer` | Fraction of tool calls with no execution error and parseable arguments |
| `TurnCountScorer` | Agent loop iterations |
| `CallsInOrderScorer` | The named tools were called in this order (as a subsequence) |
| `CallsWithScorer` | A call to the tool had the given arguments |
| `DoesNotCallScorer` | None of the named tools was called |
| `OnlyCallsScorer` | Every call was to one of the named tools |
| `ToolRespondsWithinScorer` | Every call to the tool (or every call) executed within a time limit; a call that started and never finished fails |

**Knowledge Graph** ([`rag/knowledge/eval`](../rag/knowledge/eval/)):

| Scorer | Description |
|--------|-------------|
| `EntityRecallScorer` | Fraction of expected entities extracted |
| `EntityPrecisionScorer` | Fraction of extracted entities matching expected |
| `RelationRecallScorer` | Relation extraction recall |
| `RelationPrecisionScorer` | Relation extraction precision |
| `FactSearchRecallScorer` | Fraction of relevant facts found by search |

**RAG** ([`rag/eval`](../rag/eval/)):

The 9 RAG metrics are also available as composable `Scorer` adapters: `ContextPrecisionScorer`, `ContextRecallScorer`, `NDCGScorer`, `MRRScorer`, `HitRateScorer`, `FaithfulnessScorer`, `AnswerRelevancyScorer`, `AnswerCorrectnessScorer`.

## Scorers by Name

Scorers can be declared in configuration and built from a registry:

```go
specs := []eval.ScorerSpec{
    {Kind: "json_schema", Params: json.RawMessage(`{"schema": {"type": "object", "required": ["answer"]}}`)},
    {Kind: "regex_count", Name: "citations", Params: json.RawMessage(`{"pattern": "\\[\\d+\\]", "op": ">=", "count": 2}`)},
    {Kind: "calls_in_order", Params: json.RawMessage(`{"tools": ["search", "answer"]}`)},
}
scorers, err := eval.DefaultRegistry.BuildAll(specs)
```

`eval.DefaultRegistry` holds the text, check, and budget scorers. Importing `agent/eval` adds the agent scorers (`calls_in_order`, `calls_with`, `does_not_call`, `only_calls`, `tool_responds_within`, and the timing and tool counts). `Name` renames the metric. Unknown parameters are rejected, so a typo fails instead of falling back to a default. `RegisterScorer` adds a kind; registering a kind twice is an error. LLM judges need a `Generator` and are built in code. `saige eval scorers` lists the registered kinds.

## Evaluate a Single System

```go
import "github.com/urmzd/saige/eval"

observations := []eval.Observation{
    {ID: "q1", Input: json.RawMessage(`"What is Go?"`), GroundTruth: json.RawMessage(`"A programming language."`)},
}

// Define a subject that calls the system under test.
subject := eval.Subject(func(ctx context.Context, obs *eval.Observation) error {
    // Call your system, populate obs.Output, obs.Annotations, obs.Timing
    obs.Output = json.RawMessage(`"Go is a statically typed language."`)
    return nil
})

// PopulateAll keeps going when the subject fails on one observation: the
// failure is recorded on it and Run reports it without scoring it.
_ = eval.PopulateAll(ctx, observations, subject, eval.WithConcurrency(4))

result, err := eval.Run(ctx, "my-eval", observations, []eval.Scorer{
    eval.TokenF1Scorer(),
    eval.RougeLScorer(),
    eval.NewJudgeScorer(llm, eval.WithJudgeRubric("Score for accuracy.")),
})
if err != nil {
    // A cancelled or timed-out run returns the partial suite with an error
    // wrapping ctx.Err(); result.Incomplete counts the unscored observations.
}
```

## Assertions

An `Assertion` is a gate on one metric. `Scope` picks what it checks: `EveryCase` (each score, the default) or `OnAggregate` (the mean). `Where` limits it to observations with matching labels, and `MinPassRate` relaxes an every-case gate to a share of the cases.

```go
gates := []eval.Assertion{
    {Metric: "token_f1", Op: eval.GTE, Threshold: 0.5},
    {Metric: "judge_score", Op: eval.GTE, Threshold: 0.8, Scope: eval.OnAggregate},
    {Metric: "faithfulness", Op: eval.GTE, Threshold: 0.7, MinPassRate: 0.9,
        Where: eval.Where{"topic": "billing"}},
}

// Gate during the run...
result, err := eval.Run(ctx, "my-eval", observations, scorers, eval.WithAssertions(gates...))

// ...or after the fact, on a suite loaded with ReadSuiteResult.
if result.Gate(gates...) == eval.OutcomeFailed {
    for _, v := range result.Violations {
        fmt.Println(v)
    }
    os.Exit(1)
}
```

`Gate` sets `SuiteResult.Outcome` (`passed`, `failed`, or `unasserted` when there are no assertions) and `Violations`, and stamps `Score.Passed` on every score an every-case assertion covers. A failed gate is a result, not a run error. `Check` returns the violations without recording anything.

An errored score, a metric that never scored, an incomplete suite, and subject failures are all violations, so a broken run cannot pass the gate. To require a gate to hold on every sample of a sampled scorer, sample with the `Min` reducer.

To scope a scorer instead of a gate, wrap it: `eval.Where{"lang": "go"}.Scorer(scorer)` scores matching observations and declines the rest.

## Sampling

One run of an LLM judge, or of an LLM subject, is one draw from a distribution. A `Sampler` asks for N draws and reports how much they moved, so a suite can tell a stable 0.8 from a 0.8 that was 0.4 a moment ago.

```go
sampler := eval.Sampler{N: 5, Tolerance: 0.1, Reduce: eval.Median}

// Sample every scorer in a run. Scorers marked Deterministic are scored once.
suite, _ := eval.Run(ctx, "suite", observations,
    []eval.Scorer{judge, eval.Deterministic(exactMatch)},
    eval.WithSampler(sampler))

fmt.Println(suite.UnstableScores) // verdicts that changed between samples

// Or sample only the scorer that needs it.
judge = eval.Sampled(judge, sampler)

// Sample the system under test: N copies of each observation, numbered in
// Observation.Sample, so Populate runs the subject once per copy.
observations = sampler.Replicate(observations)
```

A sampled `Score` carries the reduced `Value` plus `Samples`: every value and reason in order, mean, standard deviation, min, max, failed-sample count, and `Stable` (max minus min within `Tolerance`; the zero tolerance flags any change). Reducers: `Mean` (default), `Median` (resists one outlier), `Min` (a gate that must hold on every sample). A failed sample is counted and skipped; the score errors only when every sample fails.

## Comparing Two Subjects

Run two subjects on the same inputs and compare them:

```go
cmp, err := eval.Compare(ctx, inputs, baseSubject, expSubject,
    []eval.Scorer{rageval.NDCGScorer(10), rageval.MRRScorer()},
    eval.WithName("bm25-vs-hyde"),
    eval.WithOutputDir("experiments/bm25-vs-hyde"),
    eval.WithConcurrency(4),
    eval.WithRepeats(3),
    eval.WithAssertions(eval.Assertion{Metric: "ndcg", Op: eval.GTE, Threshold: 0.5}),
)
// cmp.Deltas["ndcg"] is the difference of the two aggregates.
// cmp.DeltaStats["ndcg"] is the paired difference with a 95% bootstrap
// interval: if [CILow, CIHigh] contains 0, the arms are not separated.
// cmp.PassStats["ndcg"] compares the gate verdicts with McNemar's interval.
// cmp.Regressions() lists the cases that got worse.
```

Every run option applies: `WithConcurrency` caps subjects and scorers, `WithSampler` samples the scorers, and `WithRepeats(n)` runs each subject n times per input. A subject error on one input is recorded on that observation (`BaseSubjectErrors`, `ExpSubjectErrors`) and the rest continue. When scoring fails, for example on cancellation, `Compare` returns the partial result with the error.

| Field | Meaning |
|-------|---------|
| `DeltaStats` | Mean paired difference per metric over cases scored on both sides (`N`), with a bootstrap interval. Repeated samples of a case are averaged first, so `N` counts cases. `WithBootstrap(resamples, confidence)` tunes it |
| `PassStats` | For metrics gated on both sides: each arm's pass rate with a Wilson interval, the paired difference with McNemar's interval, and the exact McNemar p-value. Only cases where the arms disagree move it. When a case was sampled more than once, `CaseRates` is set and the rates and difference are means of per-case pass rates with case-level bootstrap intervals |
| `Cases` | Per-case diff (`improved`, `regressed`, `unchanged`, `only_base`, `only_exp`). Samples of one case roll up; gate verdicts decide when both arms were gated, values otherwise. `WithLowerIsBetter("latency_ms")` flips the direction for a metric |
| `MissingInBase`, `MissingInExp` | Metrics scored on only one side, which have no delta |
| `BaseCounts`, `ExpCounts` | Number of scores behind each aggregate |

To check a run against a saved baseline, gate both suites with the same assertions and compare them without rerunning anything:

```go
baseline, _ := eval.ReadSuiteResult("baselines/nightly.json")
cmp := eval.CompareSuites(baseline, current)
if regs := cmp.Regressions(); len(regs) > 0 {
    // fail CI, listing regs
}
```

Cases pair by ID, Turn, Sample, and the `variant` label, so two suites from `Experiment.Run` compare each variant with itself and `CaseDiff.Variant` names it. When each arm holds one variant and they differ, as in `CompareVariants`, cases pair without the variant.

`RunExperiment`, `ExperimentResult`, `ExperimentOption`, `WithExperimentName`, `WithRunOptions`, `WriteExperiment`, and `ReadExperiment` remain as deprecated names for `Compare`, `Comparison`, `Option`, `WithName`, plain options, `WriteComparison`, and `ReadComparison`.

## Experiments

An `Experiment` states intent: a claim, the variants under test, the scorers, and the gates. It owns no data, so a run pairs it with a dataset.

```go
variants, err := eval.Matrix(func(l eval.Labels) (eval.Subject, error) {
    return newSubject(l["model"], l["tools"])
}, eval.Axis{Name: "model", Values: []string{"small", "large"}},
   eval.Axis{Name: "tools", Values: []string{"none", "search"}})

exp := eval.Experiment{
    Name:     "retrieval",
    Claim:    "search improves correctness on billing questions",
    Variants: variants,
    Scorers:  []eval.Scorer{correctness},
    Assertions: []eval.Assertion{
        {Metric: "correctness", Op: eval.GTE, Threshold: 0.8, Scope: eval.OnAggregate,
            Where: eval.Where{"tools": "search"}},
    },
}
suite, err := exp.Run(ctx, dataset, eval.WithConcurrency(8), eval.WithRepeats(5))

byVariant, _ := eval.CompareVariants(suite.Results, "small/none")
```

`Matrix` builds one variant per combination of axis values (`small/none`, `small/search`, ...) with the axis values as labels. `Run` labels each copy of an observation with its variant's labels and `variant=<name>`, runs every subject, scores everything in one suite, and gates it. Assertions apply to the whole suite, so scope a gate to one variant with `Where`. `Experiment.Sampler` samples the scorers as `WithSampler` does. `Matrix` rejects an axis named `variant`, and `Run` rejects a variant whose `variant` label differs from its name, since that label holds the name. `CompareVariants` compares each variant against the control; pass `WithAssertions(exp.Assertions...)` to also gate each variant on its own and fill `BaseOutcome` and `ExpOutcome`.

## Grouping and Statistics

Every view over results is a grouping by labels:

```go
for _, g := range eval.GroupBy(suite.Results, "correctness", "variant", "topic") {
    fmt.Printf("%s: %.2f [%.2f, %.2f] n=%d complete=%.0f%%\n",
        g.Labels, g.Stat.Value, g.Stat.Low, g.Stat.High, g.Stat.N, 100*g.Stat.Completeness)
}
rates := eval.GroupPassRate(suite.Results, "correctness", "variant")
questions := eval.QuestionPassRates(suite.Results, "correctness", 1, 5) // pass@1, pass@5
passAt5 := eval.PassAtK(suite.Results, "correctness", 5)              // per variant
```

`GroupBy` reports means with a normal interval, and `GroupPassRate` reports the share of gate verdicts that passed with a Wilson interval. `Stat.Completeness` is the share of the group's observations that produced a value, reported next to the estimate and never folded into it. `QuestionPassRates` reduces repeated samples (`WithRepeats`) to one rate per question with an unbiased pass@k estimate.

The math lives in [`eval/analysis`](analysis/), a pure package over plain counts and values: `Wilson`, `Rate`, `MeanStat`, `PassAtK`, `Completeness`, `Paired.Diff` (McNemar interval), `Paired.McNemarP`, `Bootstrap`, and `Quantile`.

## Stream Timing (Agent)

Instrument a delta channel to collect TTFT, TTLT, and median ITL. Capture the start time before invoking the agent, since deltas buffer while nothing drains them, and check the run's error:

```go
import agenteval "github.com/urmzd/saige/agent/eval"

start := time.Now()
stream := myAgent.Invoke(ctx, messages)
timing, text, deltas := agenteval.CollectStreamTimingFrom(start, stream.Deltas())
if err := stream.Wait(); err != nil || timing.Failed() {
    // The run failed; text may be truncated. timing.Errors holds each ErrorDelta.
}
// timing.TTFTMs, timing.TTLTMs, timing.MedianITL
```

To feed the tool and turn scorers, collect the whole run and annotate the observation in a subject:

```go
subject := eval.Subject(func(ctx context.Context, obs *eval.Observation) error {
    start := time.Now()
    stream := myAgent.Invoke(ctx, messagesFor(obs))
    run := agenteval.CollectAgentRunFrom(start, stream.Deltas())
    if err := stream.Wait(); err != nil {
        return err
    }
    return agenteval.AnnotateObservation(obs, run)
})

scorers := []eval.Scorer{
    agenteval.CallsInOrderScorer("search", "answer"),
    agenteval.CallsWithScorer("search", map[string]any{"limit": 5}),
    agenteval.DoesNotCallScorer("delete_file"),
    agenteval.ToolSuccessRateScorer(),
}
```

To record cost, price the run before annotating: `run = run.Priced(pricing)` with the model's `types.Pricing`. The run also carries its billable `Usage` and the response `Models` the provider reported. An unpriced rate card leaves the cost unrecorded, and so does a run that reported more than one response model (a fallback or routed run), since one rate card cannot price usage summed across models. A response-cache replay adds no tokens and no cost.

`CollectAgentRun` pairs each tool call with its arguments (or the error from parsing them) and its execution result, error, and duration, counts loop iterations, and ignores deltas nested inside sub-agent and streaming-tool output, so they do not enter the top-level trajectory.

Each trajectory metric name includes the scorer's arguments, such as `calls_in_order:search,answer` or `calls_with:search{limit=5}`, so several scorers of one kind in a suite report separate metrics.

## Storing Results

A stored evaluation is a `RunRecord` (one run: suite, lifecycle `Status`, gate `Outcome`, summary counts, total cost, and `Provenance`) plus one `Unit` per scored observation. `UnitKey` identifies a unit within a run by observation ID, turn, sample, and variant, such as `q7/0#2@candidate`. Every subsystem's evals produce the same shape, because they all score `Observation` values.

```go
import (
    "github.com/urmzd/saige/eval/store"
    "github.com/urmzd/saige/eval/store/filestore"
)

prov := eval.CaptureProvenance(ctx, ".", "gpt-6-luna") // git commit, dirty flag, models, Go version
suite, runErr := eval.Run(ctx, "nightly", observations, scorers)

results, _ := filestore.Open("eval-results")
run, err := store.SaveSuite(ctx, results, eval.NewRunID(), suite, runErr, prov)

// Later: compare against the newest succeeded run of the suite.
base, _ := store.LatestSucceeded(ctx, results, "nightly")
baseline, _ := store.LoadSuite(ctx, results, base.ID)
cmp := eval.CompareSuites(baseline, suite)
```

`StatusOf(runErr)` maps a run error to `succeeded`, `canceled` (context ended), or `errored`. The status is separate from the gate outcome: a run can succeed and fail its gates.

The `store.Store` interface has two implementations: `memstore` (in memory) and `filestore` (a directory per run with `run.json` written by temp file and rename, plus `units.jsonl` and `attempts.jsonl` appended and synced line by line). `PutUnit` on an existing key archives the stored unit as a prior attempt before replacing it, so a retried case keeps its history and never appears twice; `Attempts` returns the archive. Stores copy everything they accept and return. `SaveSuite` rejects a suite in which two results share a unit key (a repeated case ID, or variants told apart by a label other than `variant`) with `store.ErrDuplicateUnit` before writing anything. `eval/store/storetest` is the conformance suite for a new implementation.

`CaptureProvenance` reads the commit and dirty flag from git when it is available and from the binary's VCS stamp otherwise; a run outside any repository records no commit rather than failing.

`saige eval run --store DIR` records each harness run there (set `harness.Runner.Results` to do the same from code). `saige eval runs --store DIR` lists stored runs and `saige eval show RUN --store DIR` prints one, both with `--format json`.

## On-Disk Format

`WriteComparison` persists a comparison as structured JSON for reproducibility. Each file is written to a temporary file and renamed into place, so an interrupted write never leaves a partial file:

```
experiments/bm25-vs-hyde/
  result.json
  inputs/000.json         dataset fields only: id, turn, sample, input, ground_truth
  outputs/base/000.json
  outputs/exp/000.json
```

## Related

- [`eval/harness`](harness/README.md): live eval harness for multi-turn corpora against OpenAI-compatible APIs or saige providers, with a strict suite manifest (`saige eval`)
- [Root README](../README.md): project overview and installation
