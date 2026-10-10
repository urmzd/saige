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

### Inconclusive Results

A result is a real failure only when the subject answered and the answer was judged wrong, or when the subject or scorer failed for a reason a retry cannot fix (an invalid request, a context overflow, a content filter, a truncated reply, an unclassified error). When the infrastructure failed instead, the result says nothing about the system and is **inconclusive**. `eval.IsInfra(err)` decides:

| Inconclusive | Real failure |
|--------------|--------------|
| `types.ErrorKindRateLimit`, `ErrorKindUnavailable`, `ErrorKindTransient`, `ErrorKindAuth` | `ErrorKindInvalidRequest`, `ErrorKindContextLength`, `ErrorKindContentFilter`, `ErrorKindTruncated` |
| `context.Canceled`, `context.DeadlineExceeded` | `ErrorKindPermanent` and any unclassified error |
| network failures `types.ClassifyTransportError` recognizes (refused or reset connections, dial errors, timeouts) | a score below its threshold |
| batch requests (`types.BatchRequestError`, with `WithBatch` or `saige eval run --batch`) that expired, were canceled, or errored without a classified cause | a batch request that errored with a classified real cause, such as an invalid request |
| errors wrapped with `eval.Infra(err)` or matching `eval.ErrInfra`, such as an unreachable connector | |

`PopulateAll` marks an inconclusive subject failure with the `eval.subject_inconclusive` annotation next to `eval.subject_error` (`eval.MarkSubjectError` does the same for code that records failures itself), and `Run` sets `Score.Inconclusive` on a scorer error `IsInfra` reports, such as a judge whose provider was down. `SuiteResult.Inconclusive` counts the results that could not be measured; they are left out of aggregates, `PassRate`, `GroupPassRate`, `MinPassRate`, and the paired statistics, so a Wilson interval covers only measured cases and `Completeness` shows the gap.

`Check` reports them as violations of kind `inconclusive` (`Violation.Kind` is `metric`, `scorer`, `subject`, or `inconclusive`), as it does an incomplete suite and a metric missing from a scope where nothing was measured. `Gate` and `GateWith` decide the outcome:

- `failed` when any violation is a real failure, whatever else happened;
- `inconclusive` when every violation is inconclusive and the share of inconclusive cases (`SuiteResult.InconclusiveRate`, by observation ID, counting incomplete observations) is above `GatePolicy.MaxInconclusive`;
- `passed` otherwise. The default policy tolerates no inconclusive case.

```go
outcome := suite.GateWith(eval.GatePolicy{MaxInconclusive: 0.05}, gates...)
switch outcome {
case eval.OutcomeFailed:       // a measured result broke a gate: fix the system
case eval.OutcomeInconclusive: // too much was lost to outages: rerun
}
```

Runs and units stored before this distinction existed carry no marker, so their subject and scorer errors still read as failures.

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
| `Cases` | Per-case diff (`improved`, `regressed`, `unchanged`, `only_base`, `only_exp`, `inconclusive`). Samples of one case roll up; gate verdicts decide when both arms were gated, values otherwise. A case one arm could not measure because infrastructure failed is `inconclusive`, never a regression. `WithLowerIsBetter("latency_ms")` flips the direction for a metric |
| `BaseInconclusive`, `ExpInconclusive` | Results each arm could not measure; they are never paired |
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

`Comparison.Gate` turns a comparison into a regression decision. For each metric it uses the paired pass rate when both arms were gated on it and the paired mean otherwise, and the metric regresses when it dropped by more than the tolerance. With `Significant`, the whole interval of the drop (at `Comparison.Confidence`, set with `WithBootstrap`) must lie beyond the tolerance, so noise does not fail the gate:

```go
rep := eval.CompareSuites(baseline, current, eval.WithBootstrap(eval.DefaultResamples, 0.95)).Gate(eval.RegressionPolicy{
    MaxRegression: 0.02,                                // default tolerated drop
    Thresholds:    map[string]float64{"faithfulness": 0}, // per metric
    Significant:   true,
    MinCases:      10,                                   // fewer paired cases is inconclusive
    LowerIsBetter: map[string]bool{"latency_ms": true},
})
// rep.Outcome is failed (a metric regressed), inconclusive (a metric had too
// few paired cases or was measured by one arm only), or passed.
// rep.Metrics holds one MetricVerdict per metric; rep.Regressions the cases
// that got worse.
```

Without `Metrics` it checks the metrics either arm gated, or every shared metric plus those the candidate lost when nothing was gated. `saige eval compare` runs the same gate over two stored runs; see [Regression Gate in CI](#regression-gate-in-ci).

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

The `store.Store` interface has three implementations: `memstore` (in memory), `pgstore` (PostgreSQL, see [Storing Results in Postgres](#storing-results-in-postgres)), and `filestore` (a directory per run with `run.json` written by temp file and rename, plus `units.jsonl` and `attempts.jsonl` appended and synced line by line). `PutUnit` on an existing key archives the stored unit as a prior attempt before replacing it, so a retried case keeps its history and never appears twice; `Attempts` returns the archive. Stores copy everything they accept and return. `SaveSuite` rejects a suite in which two results share a unit key (a repeated case ID, or variants told apart by a label other than `variant`) with `store.ErrDuplicateUnit` before writing anything. `eval/store/storetest` is the conformance suite for a new implementation.

`CaptureProvenance` reads the commit and dirty flag from git when it is available and from the binary's VCS stamp otherwise; a run outside any repository records no commit rather than failing.

`saige eval run --store DIR` records each harness run there (set `harness.Runner.Results` to do the same from code). `saige eval runs --store DIR` lists stored runs and `saige eval show RUN --store DIR` prints one, both with `--format json`.

## Storing Results in Postgres

`eval/pgstore` keeps runs and units in PostgreSQL 18, so several machines and CI jobs share one history. `postgres.RunMigrations` creates its tables along with the rest of the saige schema, after checking the server version and extensions. `pgstore.New` returns `pgstore.ErrSchemaMissing` when the tables are not there.

```go
import (
    "github.com/urmzd/saige/eval/pgstore"
    "github.com/urmzd/saige/postgres"
)

pool, _ := postgres.NewPool(ctx, postgres.Config{URL: dsn})
_ = postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{})

results, err := pgstore.New(ctx, pool, "acme") // tenant scope; "" for a single tenant
run, err := store.SaveSuite(ctx, results, eval.NewRunID(), suite, runErr, prov)
```

A store is scoped to one tenant. Every row carries the tenant and every query filters on it, so two tenants can reuse run IDs without seeing each other's runs. The store passes the shared conformance suite in `eval/store/storetest`.

The full run and unit records are stored as JSON, so a read returns exactly what was written. Indexed columns answer the common questions without loading every unit:

| Question | Call | Index |
|----------|------|-------|
| Newest runs, by suite and status | `ListRuns` | `(tenant, started_at)`, `(tenant, suite, started_at)` |
| Baseline for a regression check | `store.LatestSucceeded` or `Baseline` | `(tenant, suite, status, started_at)` |
| Units of a run, by label | `Units` with `UnitFilter.Where` | `(tenant, run_id, seq)`, GIN on the labels |
| History of one case across runs | `CaseHistory(ctx, suite, caseID, limit)` | `(tenant, observation_id, recorded_at)` |
| Score-by-score diff of two runs | `Diff(ctx, base, candidate, DiffFilter{...})` | one `eval_score` row per score, keyed by run and metric |

```go
base, _ := results.Baseline(ctx, "nightly")
drops, _ := results.Diff(ctx, base.ID, run.ID, pgstore.DiffFilter{Regressions: true, MinDrop: 0.1})
for _, d := range drops {
    delta, ok := d.Delta() // false when a side is missing or errored
    fmt.Println(d.Key, d.Metric, delta, ok)
}
```

`Diff` sorts missing and errored scores first, then the largest drop. With `Regressions` it keeps only baseline scores that dropped by more than `MinDrop`, errored, or went missing in the candidate. A candidate score that is inconclusive, or missing because the candidate's subject failed on infrastructure for that case, sets `CandidateInconclusive` and is never a regression. `eval_score.inconclusive` holds the flag; `RunMigrations` adds the column to a table an older release created, and rows written before it read as conclusive.

Every eval command that takes `--store` accepts a PostgreSQL URL in place of a directory, and `--tenant` picks the scope:

```bash
saige eval run --manifest evals/saige.eval.json --store postgres://user:pass@host/db --tenant acme
saige eval runs --store postgres://user:pass@host/db --tenant acme
saige eval show <run-id> --store postgres://user:pass@host/db --tenant acme
```

The CLI runs the migrations when it connects. A manifest's `store` may also be a PostgreSQL URL; keep credentials out of the file and pass `--store` instead.

## Online Evals

`eval/online` scores production runs after they finish, with the same scorers a suite uses, and records the results in any results store. A **run** ends at an assistant turn that calls no tools; it starts after the user message before it. The tool calls, results, route (model and preset), and first error (a tool error or a truncated turn) between the two are read from the conversation.

| Source | Reads |
|--------|-------|
| `online.TreeSource{"conv-id": tree}` | conversation trees in memory, every branch |
| `online.PGSource{Pool: pool, Scope: "acme"}` | conversations stored by `agent/pgstore`, optionally one tenant scope |
| `online.RecordSource{online.FromAgentRun(ref, input, finished, run)}` | runs collected with `agent/eval.CollectAgentRun`, with their route and trace records |

A `Sampler` filters the records, samples a fraction of them, scores the sample, and writes one run:

```go
import (
    "github.com/urmzd/saige/agent/provider/openai"
    "github.com/urmzd/saige/agent/types"
    agenteval "github.com/urmzd/saige/agent/eval"
    "github.com/urmzd/saige/eval/online"
)

budget := types.NewBudget(types.BudgetPolicy{Limit: types.USD(0.50), MaxRequests: 200})
judge := eval.NewJudgeScorer(&online.BudgetedGenerator{
    Provider: openai.NewAdapter(key, "gpt-6-luna"), // a cheap judge model
    Budget:   budget,
}, eval.WithJudgeRubric("Score 1 when the answer is correct and complete."))

errored := true
s := &online.Sampler{
    Store:   results,
    Rate:    0.1, // score 10% of matching runs
    Seed:    42,
    Filter:  online.Filter{Models: []string{"gpt-6-luna"}, Tools: []string{"search"}, Errored: &errored},
    Scorers: []eval.Scorer{agenteval.ToolSuccessRateScorer(), eval.TokenBudgetScorer(4000)},
    Judges:  []eval.Scorer{judge},
    Budget:  budget,
}
rep, err := s.Sweep(ctx, online.PGSource{Pool: pool}, online.Window{From: time.Now().Add(-24 * time.Hour)})
```

- **Sampling** is deterministic: a record is in the sample by a hash of `Seed`, its conversation, and its node, so the same seed picks the same runs in every sweep and in watch mode, whatever the order. A higher rate keeps every run a lower rate kept. `Rate` zero scores every matching run.
- **Filters** select by model, preset, tools called, error, and any label (`Filter.Where`), including the labels below.
- **Judges** are charged to `Budget` through `BudgetedGenerator`, which reserves before each call and settles with the reported usage. Once the budget refuses a call, judges decline the remaining runs, the deterministic scorers still run, and `Report.JudgesSkipped` counts the refusals.
- **Judge outages** are inconclusive: a judge call that fails on infrastructure (see [Inconclusive Results](#inconclusive-results)) records an errored score with `Inconclusive` set, `RunRecord.Inconclusive` counts it, and `Promote` never promotes a unit for it.
- **Results** are one `RunRecord` per sweep (suite `online` by default, label `source=online`, the window in `Provenance.Extra`), with one unit per scored run. A unit's observation ID is the node that ended the run, and its labels carry `source`, `conversation`, `node`, `model`, `preset`, and `errored`. A record with a trace ID also sets `Unit.Trace`. An empty window still records an empty run.

**Long-running mode.** `Sampler.Watch` keeps one run open and scores each run a producer announces, until its context ends. Producers call `online.Announce` after storing the run's final node; with `postgres.Notifier` the announcement crosses processes. Notifications are hints, not a queue, so set `WatchOptions.Since` to sweep the runs that finished while no watcher was listening:

```go
n := postgres.NewNotifier(pool, postgres.NotifierOptions{})
rep, err := s.Watch(ctx, n, online.PGSource{Pool: pool}, online.WatchOptions{Since: time.Now().Add(-time.Hour)})

// In the serving process, after the run's last node is stored:
_ = online.Announce(ctx, n, "", online.Ref{Conversation: convID, Node: string(node.ID)})
```

**Promote to dataset.** `online.Promote` turns failing or flagged units into dataset cases: the input, an expected output when `PromoteOptions.Expected` supplies one, and otherwise a rubric built from the reasons the unit failed. Every string in the input, expected output, rubric, and reasons is run through the `agent/privacy` redactor (the default detector unless `Detector` is set) and replaced irreversibly, so those fields never hold a value the detector finds, such as an email address, phone number, card number, or secret. `online.Failing(threshold, metrics...)` promotes flagged units (label `flagged=true`), failed gates, errored scores that are not inconclusive, and the listed metrics below the threshold. `WriteCases` writes JSON Lines; `Case.Observation` turns a case back into an `eval.Observation`.

```go
units, _ := results.Units(ctx, rep.Run.ID, store.UnitFilter{})
cases, err := online.Promote(ctx, units, online.PromoteOptions{Failing: online.Failing(0.5, "judge_score")})
_ = online.WriteCases(f, cases)
```

From the terminal, `saige eval online` sweeps the conversations in a PostgreSQL database (`--source`, by default the `--store` URL):

```bash
saige eval online --store postgres://user:pass@host/db --since 24h --rate 0.1 --seed 42 \
  --scorer tool_success_rate --scorer '{"kind": "token_budget", "params": {"max_tokens": 4000}}' \
  --judge --provider openai --model gpt-6-luna --judge-budget 0.50 \
  --promote cases.jsonl
saige eval online --store postgres://user:pass@host/db --watch --scorer tool_success_rate
```

Filters are `--only-model`, `--only-preset`, `--tool`, `--label key=value`, and `--errored true|false`; `--scope` reads one tenant's conversations and `--tenant` writes to one tenant's results. `--watch` listens on `--channel` (default `saige.eval.online`) until interrupted, after sweeping the last `--since`.

## Regression Gate in CI

`saige eval compare` gates a stored run against a baseline run from the same results store, using `Comparison.Gate`:

```bash
saige eval compare RUN_ID --store eval-results                     # against the newest earlier succeeded run of its suite
saige eval compare --suite pr-42 --baseline-suite main --store "$DSN" # the newest pr-42 run against the newest earlier main run
saige eval compare RUN_ID --baseline BASE_ID --store eval-results \
  --max-regression 0.02 --threshold faithfulness=0 --significance 0.05 --min-cases 10 \
  --lower-is-better latency_ms --assert 'turn_succeeded>=1'
saige eval compare RUN_ID --store eval-results --format markdown --output compare.md
saige eval compare RUN_ID --store eval-results --format junit --output compare.xml
```

| Flag | Meaning |
|------|---------|
| `--baseline` | A run ID, or `latest` (default): the newest succeeded run of `--baseline-suite` (default the candidate's suite) that started before the candidate, skipping runs whose gate was inconclusive |
| `--metric` | Metrics to check (default: the metrics either run gated, else every shared metric) |
| `--max-regression`, `--threshold metric=value` | Tolerated drop, overall and per metric: a share of cases for a gated metric, the metric's units otherwise |
| `--significance` | A drop counts only when the (1 - significance) interval lies beyond the tolerance (default 0.05); `1` counts any drop beyond it |
| `--min-cases` | Fewest paired cases a metric needs (default 3); fewer is inconclusive |
| `--lower-is-better` | Ungated metrics where a rise is the regression |
| `--assert`, `--max-inconclusive` | Gate both runs again first; the candidate's gate counts too |
| `--format` | `human` (default), `markdown` (a pull request comment), `junit` (one test case per metric; a regression is a failure, an inconclusive metric is skipped), `json` |
| `--output` | Write the report to a file; a one-line summary always goes to stderr |

Exit status: 0 no regression, 1 a metric regressed or the candidate failed its gate, 3 inconclusive, 2 invalid input. `saige eval run` uses 3 the same way, for a run lost to infrastructure failures, so CI can rerun on 3 and fail on 1.

A GitHub Actions job that records every main branch run as the baseline, gates each pull request against it, comments the result, and reruns once when the run is inconclusive:

```yaml
name: eval
on:
  push:
    branches: [main]
  pull_request:

jobs:
  eval:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write
    env:
      SAIGE_EVAL_STORE: ${{ secrets.SAIGE_EVAL_STORE }} # postgres://... shared by every run
      OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
      # main for the baseline, one suite per pull request for candidates
      SUITE: ${{ github.event_name == 'push' && 'main' || format('pr-{0}', github.event.pull_request.number) }}
    steps:
      - uses: actions/checkout@v6
      - uses: actions/setup-go@v6
        with:
          go-version: stable
      - run: go install github.com/urmzd/saige/cmd/saige@latest

      # Exit 3 means the run was lost to an outage or a rate limit: rerun it
      # once. Exit 1 is a real failure and stops here.
      - name: Run the suite
        run: |
          for attempt in 1 2; do
            code=0
            saige eval run --manifest evals/saige.eval.json --force \
              --store "$SAIGE_EVAL_STORE" --suite "$SUITE" --assert 'turn_succeeded>=1' || code=$?
            [ "$code" -eq 3 ] || break
          done
          exit "$code"

      - name: Compare with main
        if: github.event_name == 'pull_request'
        run: |
          code=0
          saige eval compare --suite "$SUITE" --baseline-suite main --store "$SAIGE_EVAL_STORE" \
            --format markdown --output compare.md || code=$?
          saige eval compare --suite "$SUITE" --baseline-suite main --store "$SAIGE_EVAL_STORE" \
            --format junit --output compare.xml || true
          gh pr comment "${{ github.event.pull_request.number }}" --body-file compare.md --edit-last \
            || gh pr comment "${{ github.event.pull_request.number }}" --body-file compare.md
          exit "$code"
        env:
          GH_TOKEN: ${{ github.token }}

      - uses: actions/upload-artifact@v7
        if: always() && github.event_name == 'pull_request'
        with:
          name: eval-compare
          path: compare.xml
```

Give the main branch runs their own suite and each pull request its own, as here, so a pull request is never compared with another pull request and `--suite` always finds the run just recorded. Pass `compare.xml` to any JUnit test reporter to show each metric as a test.

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
