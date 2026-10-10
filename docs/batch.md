# Batch processing

Vendor batch APIs run many independent model calls as one asynchronous job. They charge about half the interactive price and finish within minutes to hours.
They suit evals, judges, bulk extraction and other offline work. saige puts every vendor's batch API behind one interface, `types.BatchProvider`, and adds a durable job runner, budget accounting and eval integration on top.

## When to batch

Batch work that is **single-turn**: each request is answered on its own, and nothing waits on its answer to ask the next question.

| Good fit | Poor fit |
| --- | --- |
| Eval subjects and LLM judges over a dataset | Interactive chat |
| Extraction or classification over many documents (`AIFunction.Batch`) | Multi-turn agent loops with tools |
| One-shot answers to many prompts (`agent.RunBatch`) | Anything with a latency target under a day |

**Multi-turn agent loops are out of scope.** Each turn depends on the previous turn's tool results. In a batch every turn waits for the whole batch, so a five-turn loop could take five days.
`agent.RunBatch` therefore sends no tools. A subject that makes dependent calls still works under the coalescer (see [evals](#evals)), but it waits for one batch per call.

## Vendors

Checked against the vendor documentation on 2026-10-09.

| Adapter | Batch API | Input | Results | Limits | Turnaround | Discount |
| --- | --- | --- | --- | --- | --- | --- |
| `anthropic.Adapter` | [Message Batches](https://platform.claude.com/docs/en/build-with-claude/batch-processing) | Inline requests | JSONL, any order, by `custom_id` | 100,000 requests or 256 MB | Most under 1 hour; expires after 24 hours; results kept 29 days | 50% |
| `openai.Adapter` | [Batch API](https://developers.openai.com/api/docs/guides/batch) on `/v1/chat/completions` | Uploaded JSONL file | Output and error files, any order, by `custom_id` | 50,000 requests or 200 MB per file | 24-hour window | 50% |
| `openai.ResponsesAdapter` | Batch API on `/v1/responses` | Uploaded JSONL file | As above | As above | As above | 50% |
| `google.Adapter` (Gemini API) | [Batch mode](https://ai.google.dev/gemini-api/docs/batch-mode) | Inline requests | Inline responses, keyed by metadata | About 20 MB inline | Target 24 hours; expires after 48 hours | 50% |
| `google.VertexBatch` (Vertex AI) | [Batch prediction](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/batch-prediction-gemini) | JSONL in Cloud Storage | JSONL in Cloud Storage, keyed by request label | 200,000 requests; 1 GB input | Queues up to 72 hours | 50% |
| `batch.Local` (Ollama, any provider) | None: runs the calls itself | In memory | In memory | Process lifetime | As fast as the model | None |

Every vendor bills completed requests only. Errored, canceled and expired requests are not charged.

## The interface

```go
type BatchProvider interface {
    Submit(ctx context.Context, requests []BatchRequest, opts BatchSubmitOptions) (BatchHandle, error)
    Status(ctx context.Context, h BatchHandle) (BatchStatus, error)
    Results(ctx context.Context, h BatchHandle) iter.Seq2[BatchResult, error]
    Cancel(ctx context.Context, h BatchHandle) error
}
```

A `BatchRequest` carries what a normal call carries: messages, tools, a response schema and `RequestOptions`, including dials. Each adapter validates and encodes a request exactly as `ChatStreamWithOptions` (or `ChatStreamWithSchema`) would, so dials compile per model and a control the model rejects fails the same way.

**Options are rejected at submit (D-12).** Nothing is uploaded until every request passes. Batch-specific limits are checked too: Anthropic's `custom_id` alphabet, request counts, payload sizes, and `max_tokens` above zero. A request that cannot be expressed fails with an error matching `types.ErrInvalidModelConfig`.

A `BatchResult` holds the request's `CustomID`, its `Outcome` (`succeeded`, `errored`, `canceled` or `expired`), the assistant message, the finish reason, and usage. A succeeded request that stopped at the output limit or a content filter has `Err` set as well, the same error a streaming call reports. A failed request's `Err` matches `types.ErrBatchRequest` and, where the vendor gave a cause, the classified kind, such as `types.ErrInvalidRequest`.

`BatchHandle` is plain data: provider, model, vendor ID and the details a later call needs. A process that saved it can resume a batch it did not submit.

### Ollama and other providers without a batch API

`batch.NewLocal(provider, concurrency)` runs each request through the provider, at most `concurrency` at a time, behind the same interface. It is priced at interactive rates.
A local batch lives in its process. After a restart its handle is unknown, and the runner submits the requests again, which repeats the calls.

### Vertex AI needs a Cloud Storage bucket

Vertex AI batch prediction reads its input from Cloud Storage and writes its output there. There is no inline option. The plain adapter on Vertex AI refuses a batch and points to `google.NewVertexBatch`:

```go
a, _ := google.NewAdapter(ctx, "", "gemini-3.1-flash-lite", google.WithVertex(project, "us-central1"))
vb, err := google.NewVertexBatch(a, "gs://my-bucket/saige-batches")
```

Each batch writes `input.jsonl` and its output under `gs://BUCKET/PREFIX/TAG/`. The credentials need object read and write on the bucket, and so does the Vertex AI service agent. Each request carries its custom ID as the label `saige_key`, which batch prediction echoes in the output.

## Durable jobs

A batch can outlive the process that submitted it. `batch.Runner` keeps a job record in a `batch.Store`:

```go
store, _ := batch.NewFileStore("./batch-jobs")       // or batch.NewMemoryStore(), postgres.NewBatchStore(pool)
jobs := batch.NewRunner(adapter, store, batch.WithBudget(budget))

results, err := jobs.Run(ctx, "nightly-extract-2026-10-09", requests)
```

The record holds the job ID, the vendor batch ID, a SHA-256 manifest of the requests, the request IDs and the state. It never holds the prompts.

| Store | Use |
| --- | --- |
| `batch.MemoryStore` | Tests and jobs that need not survive the process |
| `batch.FileStore` | One process at a time on one machine; atomic writes |
| `postgres.BatchStore` | Shared between processes; the table `saige_batch_jobs` is created by `postgres.RunMigrations` |

**Submit is idempotent by job ID.** The record is saved before the vendor sees anything. Calling `Submit` (or `Run`) again with the same ID and requests resumes the job:

- A submitted job is not submitted again. A restarted process polls the batch the first one created.
- Different requests under the same ID fail with `batch.ErrManifestMismatch`.
- A submit that was interrupted after the vendor may have created the batch, but before its ID was saved, is looked up (`types.BatchFinder`). OpenAI and Gemini search by a tag in the batch metadata or display name. Anthropic batches carry no metadata, so the lookup matches recent batches by request count and by the request IDs in their results.
- When the lookup cannot tell, the job becomes `JobIndeterminate` and `Submit` returns `batch.ErrIndeterminate` (D-14). Check the vendor's console, then call `Reconcile` with the batch you found or permit a resubmit:

```go
jobs.Reconcile(ctx, id, batch.Resolution{Handle: &types.BatchHandle{Provider: "anthropic", ID: "msgbatch_..."}})
jobs.Reconcile(ctx, id, batch.Resolution{Resubmit: true}) // only after confirming no batch exists
```

Request IDs are rewritten on the wire as `<job prefix>-<index>`, which fits every vendor's alphabet whatever the caller's IDs look like, and maps back to the caller's IDs in `Results`.

### Waiting for completion

`Wait` polls with backoff, from 5 seconds up to 2 minutes by default (`batch.WithPollInterval`). With `batch.WithNotifier`, it also wakes when another process sees the job end: `Sweep` and `Watch` publish the job ID on the `saige.batch` channel. Use `postgres.Notifier` to wake waiters in other processes.

### In a durable workflow

`duraturo.Engine.AwaitBatch` submits a job from a workflow body and parks the run until the job ends, so a batch that takes hours holds no worker. Run `WatchBatches` next to the workers: it polls open jobs and signals the run that owns each one.

```go
results, err := engine.AwaitBatch(ctx, jobs, "extract-"+runID, requests)
// elsewhere, next to the workers:
go engine.WatchBatches(ctx, jobs, time.Minute)
```

The submit and the results are recorded steps, so a replay neither submits again nor reads the results again.

## Cost and budget

Catalog pricing rows declare `batch_discount` (0.5 for every Anthropic, OpenAI and Google row) and, where the vendor stacks the batch discount with the cache-read discount, `batch_cached_input_per_mtok`:

| Vendor | Batch discount | Cache reads in a batch |
| --- | --- | --- |
| Anthropic | 50% | Discounts stack: half the cache-read rate |
| OpenAI | 50% | Half the cache-read rate |
| Google | 50% | Cache-read rate unchanged: on Vertex AI the cache discount applies instead of the batch discount |

`types.Pricing.Batch()` applies them. A row without a declared discount is charged at interactive rates, which over-counts rather than under-counts.

With `batch.WithBudget`, the runner:

1. **reserves** each request at submit, using the budget's `PerCallCost` and `PerCallTokens` bounds. A batch the budget cannot cover is refused before anything is sent;
2. **settles** each request on its result, at the batch rate card;
3. **releases** requests that errored, were canceled or expired, since vendors do not bill them;
4. charges a request missing from the vendor's results its whole reservation, marked uncertain (`Budget.Uncertain`).

A budget is process-local. A resumed job reserves again in the new process.

## Evals

`eval.WithBatch` runs the model calls of `PopulateAll`, `Run` and `Compare` as batches. Give the same `batch.Coalescer` to the subjects and judges, as their provider or generator, and to the run:

```go
store, _ := batch.NewFileStore("./eval-batches")
jobs := batch.NewRunner(adapter, store)
model := batch.NewCoalescer(jobs, batch.WithJobPrefix("nightly"))

judge := eval.NewJudgeScorer(model)
cmp, err := eval.Compare(ctx, dataset, baseSubject(model), expSubject(model), []eval.Scorer{judge}, eval.WithBatch(model))
```

Every case runs at once, and the coalescer sends a batch as soon as every case is waiting on a call. `Compare` populates both arms together and scores both together, so a comparison with a judge makes two batches. Request IDs are the arm, case ID, turn and sample (and the scorer, for judge calls), so results map back to the case that asked. A restarted run with the same dataset sends the same batches, and the runner resumes them.

Outside an eval, `batch.Coalescer` gathers any concurrent calls: callers that `Join` are participants, and without participants a batch is sent after an idle window (2 seconds by default).

From the CLI:

```bash
saige eval run --manifest evals/saige.eval.json --provider anthropic --batch --batch-store .saige/batches
```

`--batch` needs a saige provider (`--provider` or the manifest's subject provider). Every script runs at once, so each turn's calls share a batch. With `--batch-store`, re-running the same command after an interruption resumes the submitted batches.

## Agents and typed functions

```go
results, err := a.RunBatch(ctx, []agent.BatchInput{{ID: "doc-1", Messages: msgs1}, {ID: "doc-2", Messages: msgs2}}, agent.BatchConfig{})

items, err := classify.Batch(ctx, tickets, agent.BatchConfig{JobID: "triage-2026-10-09"})
for _, it := range items {
    if it.Err != nil { /* per-input failure, or ErrSchemaInvalid */ }
    _ = it.Out
}
```

`RunBatch` applies the agent's system prompt, dials, dial policy and response schema to every input, and charges the agent's budget. `AIFunction.Batch` sends the output schema natively and decodes each answer into `Out`. An answer that does not decode wraps `agent.ErrSchemaInvalid`; it is not repaired, because a repair is a second turn.

Both find the batch API behind the agent's provider, through decorators such as retry, with `agent.BatchProviderFor`. A router or fallback chain batches on its first member. A provider with no batch API runs locally with `batch.NewLocal`. Pass `BatchConfig.Runner` to choose the store, budget and polling.
