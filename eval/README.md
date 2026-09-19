# eval

Composable evaluation framework that works across all saige subsystems. The core `eval` package has zero subsystem dependencies. Subsystem-specific scorers live alongside their domains: [`agent/eval`](../agent/eval/), [`knowledge/eval`](../knowledge/eval/), [`rag/eval`](../rag/eval/).

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
| `Score` | Named metric value with optional reason |

## Built-in Scorers

**Text Quality** (pure functions, no LLM):

| Scorer | Description |
|--------|-------------|
| `SequenceSimilarityScorer` | Character-level LCS ratio between output and ground truth |
| `TokenF1Scorer` | Word-token precision/recall/F1 |
| `RougeLScorer` | ROUGE-L F1 at the token level |

**LLM-as-Judge**:

| Scorer | Description |
|--------|-------------|
| `NewJudgeScorer` | Pointwise scoring with customizable rubric |
| `NewPairwiseJudgeScorer` | A/B comparison between two outputs |

**Agent** ([`agent/eval`](../agent/eval/)):

| Scorer | Description |
|--------|-------------|
| `TTFTScorer` | Time to first token (ms) |
| `TTLTScorer` | Time to last token (ms) |
| `MedianITLScorer` | Median inter-token latency (ms) |
| `ToolCallCountScorer` | Number of tool calls |
| `ToolSuccessRateScorer` | Fraction of successful tool calls |
| `TurnCountScorer` | Agent loop iterations |

**Knowledge Graph** ([`knowledge/eval`](../knowledge/eval/)):

| Scorer | Description |
|--------|-------------|
| `EntityRecallScorer` | Fraction of expected entities extracted |
| `EntityPrecisionScorer` | Fraction of extracted entities matching expected |
| `RelationRecallScorer` | Relation extraction recall |
| `RelationPrecisionScorer` | Relation extraction precision |
| `FactSearchRecallScorer` | Fraction of relevant facts found by search |

**RAG** ([`rag/eval`](../rag/eval/)):

The 9 RAG metrics are also available as composable `Scorer` adapters: `ContextPrecisionScorer`, `ContextRecallScorer`, `NDCGScorer`, `MRRScorer`, `HitRateScorer`, `FaithfulnessScorer`, `AnswerRelevancyScorer`, `AnswerCorrectnessScorer`.

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

eval.Populate(ctx, observations, subject)

result, _ := eval.Run(ctx, "my-eval", observations, []eval.Scorer{
    eval.TokenF1Scorer(),
    eval.RougeLScorer(),
    eval.NewJudgeScorer(llm, eval.WithJudgeRubric("Score for accuracy.")),
})
```

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

## A/B Experiment

Compare two approaches on the same inputs:

```go
result, _ := eval.RunExperiment(ctx, inputs, baseSubject, expSubject,
    []eval.Scorer{rageval.NDCGScorer(10), rageval.MRRScorer()},
    eval.WithOutputDir("experiments/bm25-vs-hyde"),
    eval.WithExperimentName("bm25-vs-hyde"),
)
// result.Deltas["ndcg"] shows the improvement
```

## Stream Timing (Agent)

Instrument a delta channel to collect TTFT, TTLT, and median ITL:

```go
import agenteval "github.com/urmzd/saige/agent/eval"

stream := myAgent.Invoke(ctx, messages)
timing, text, deltas := agenteval.CollectStreamTiming(stream.Deltas())
// timing.TTFTMs, timing.TTLTMs, timing.MedianITL
```

## On-Disk Format

Experiment results persist as structured JSON for reproducibility:

```
experiments/bm25-vs-hyde/
  result.json
  inputs/000.json
  outputs/base/000.json
  outputs/exp/000.json
```

## Related

- [`eval/harness`](harness/README.md): live eval harness for multi-turn corpora against OpenAI-compatible APIs (`saige eval`)
- [Root README](../README.md): project overview and installation
