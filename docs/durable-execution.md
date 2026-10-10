# Durable execution

Two engines make a run durable. `agent/durable/local` keeps runs on one machine's filesystem.
`agent/durable/duraturo` runs them on a duraturo ledger and queue, such as Postgres tables; see [the duraturo engine](#the-duraturo-engine).
Most of this page describes the local engine. The contracts for approvals, uncertain steps and budget receipts are the same in both.

## The local engine

`agent/durable/local` saves a run between worker invocations.
A run has one process owner at a time. Different run IDs can execute independently.
The engine requires Unix and a private local filesystem with reliable advisory locks and atomic rename.
It does not provide remote scheduling or an atomic transaction with an external service.

### Start, decide, and resume

```go
engine := local.New("./private-runs")
input := []types.Message{types.UserMsg(types.Text("Review and apply the change"))}
factory := func() *agent.Agent {
    a, err := agent.New(agent.Config{
        Provider: configuredProvider,
        Tools: types.NewToolRegistry(types.WithMarkers(writeTool,
            types.Marker{Kind: "approval"})),
        Budget: types.NewBudget(types.BudgetPolicy{
            Limit: types.USD(1), PerCallCost: types.USD(.10),
            MaxTokens: 20000, PerCallTokens: 4000, MaxRequests: 10,
        }),
    })
    if err != nil {
        log.Fatal(err) // a fixed configuration: it fails on every replay or none
    }
    return a
}
_, err := engine.Run(ctx, "run-42", "config-v3", factory, input)
if errors.Is(err, types.ErrSuspended) {
    state, err := engine.Inspect("run-42")
    if err != nil { return err }
    // Display each pending state.Interrupts entry to an authenticated user.
    // Store their decision using that entry's request ID:
    _ = state
}
```

After the host receives a decision:

```go
err := engine.Decide("run-42", "config-v3", interruptID, "decision-17",
    types.ApprovalDecision{Approved: true})
if err != nil { return err }
result, err := engine.Run(ctx, "run-42", "config-v3", factory, input)
```

The factory must return a fresh agent, tree, and budget for each call.
The revision identifies the provider, tools, permissions, and budget configuration.
Use the same revision to resume. The input is an append-only log: an input that repeats the logged input resumes the run, an input that extends it appends the new messages as a segment, and an input that diverges from it returns `ErrConflict`, as does a changed revision.
`Engine.Append(id, revision, key, msgs)` adds a segment without running; the next `Run` executes it after the logged input finishes. `key` makes the call idempotent.
Do not share a mutable tree between factories or load a partially reconstructed tree into the factory.
Tool objects and external services remain the host's isolation responsibility.
Local durable children must share the root budget. An independent child budget is rejected because reconciled receipts belong to the run ledger.
Result sinks and policy hooks can run again during replay. Make external writes in those hooks idempotent; they are outside the step journal.

Each pending approval has its own ID and expiry. The default expiry is 24 hours.
Gate approvals and marker approvals have separate IDs, including within subagents.
An identical decision retry is accepted before the run closes. A conflicting retry is rejected.
The host authenticates the person and checks their authority before calling `Decide`.
A modified argument map becomes the approved call's arguments.
`Engine.Router()` returns a host-side `types.InterruptRouter` for the engine's runs; each interrupt's `RunID` names the run it belongs to.
`agent.WithInterruptExpiry` sets the expiry policy: an expired approval is denied by default, fails the run with `InterruptExpireFail`, or is asked of the caller one level up with `InterruptExpireEscalate`.

### Waking waiters without polling

Set `Engine.Notifier` to announce state changes on the `local.SignalChannel` channel.
`Decide`, `Router.Reply`, a worker's `Reply`, `Append`, `Cancel`, and the end of `Run` each publish a `local.Signal` after they save.
`Engine.Await(ctx, id, ready)` subscribes, reads the run, and returns when `ready` accepts the state; it reads again after each signal for that run.
It reads once after subscribing, so a change saved before the call is not missed.
`local.Resumable` accepts a run that has new input, or a suspended run that received a reply or input after it suspended.

```go
n := postgres.NewNotifier(pool, postgres.NotifierOptions{}) // or notify.NewMemory(0) in one process
engine := &local.Engine{Directory: "./private-runs", ApprovalTTL: 24 * time.Hour, Notifier: n}

// Worker: run whenever a reply or new input arrives.
for {
    if _, err := engine.Await(ctx, "run-42", local.Resumable); err != nil { return err }
    _, err := engine.Run(ctx, "run-42", "config-v3", factory, nil)
    // ErrSuspended: wait for the next reply.
}

// Another process: deliver input or a decision.
err := engine.Append("run-42", "config-v3", "msg-18", msgs)
```

A signal is a wake-up hint, not the change itself. A signal lost while a listener reconnects delays the waiter until the next signal for the run; use `NotifierOptions.OnReconnect` to re-check.
A failed publish returns an error matching `local.ErrSignal`. The change is already saved, so a retry with the same idempotency key is safe.
Without a notifier, the engine behaves as before and `Await` returns `local.ErrNoNotifier`.
`agent.EventStream.Submit` delivers to a stream in the same process. To deliver from another process, `Append` to the durable run as above, or subscribe the process that owns the stream to a channel and call `Submit` for each message.

### Execution and recovery

Each provider call and regular tool call has a stable step name.
The engine saves a started record before calling it, then saves the result before returning it.
Completed steps replay from disk. They do not call the provider or tool again.
A pending approval returns immediately from its call; independent sibling steps can finish.
The parent model turn resumes only after all tool results are available.
The run returns `ErrSuspended` and releases its process lock after that batch drains.
This permits other runs to use workers while the host waits for a decision.

| Event | Saved evidence | Recovery action |
| --- | --- | --- |
| Approval is pending | Request, arguments, revision, expiry | Decide later, then resume with a fresh factory |
| Process exits before a step starts | No attempt record | Execute the step |
| Process exits during a step | Started record, conservative reservation if applicable | Return `ErrIndeterminate`; check the external effect |
| Result is saved before process exit | Completed result and settlement | Replay without repeating the call |
| Provider errors or has no usage | Uncertain or conservative charge | Retain the charge; reconcile before retrying a failed step |
| Tool returns an ordinary error | Saved tool error result | Replay the error for the model to handle |
| Tool panics | Indeterminate step | Check whether the tool already changed external state |
| Process exits during an idempotent tool step | Started record marked idempotent | The local engine runs the step again (`agent.Idempotent`, `types.IdempotentTool`) |
| Snapshot write fails | Runner is stopped | Release the worker; inspect the last complete snapshot before recovery |
| Second worker uses the same run | Existing process lock | Return `ErrBusy`; do not start another execution |
| Pending approval expires | Original request and expiry | Reject it; cancel or create a new run with current authorization |

Use `Reconcile(runID, revision, stepName, &knownResult)` after confirming the external outcome.
Use a nil result only when the host has established that retry is safe.
A retry retains the previous attempt's budget receipt. It does not erase an uncertain charge.
For a remote write, use an external idempotency key and query its outcome before permitting another attempt.
No local journal can guarantee exactly-once effects across an unrelated API.

`Cancel` prevents future resumes. If the run is active, cancel its context first and wait for it to release ownership.
Cancellation cannot undo a completed external write.
The local lock releases when a process exits; it has no network lease timeout.

### Budget admission and usage

Reservations prevent sibling calls from using the same available allowance.
The host must set safe per-call cost and token bounds for the configured context, output limit, and provider.
Without a bound, a call reserves the remaining capacity for that limit.
`ErrBudgetBusy` fails admission while other calls hold capacity; the SDK does not maintain a waiter queue.
`ErrBudgetExceeded` means the requested capacity is unavailable even without other reservations.

A reservation is saved before provider dispatch. Settlement replaces it with actual usage.
A missing or interrupted usage report retains at least the reserved charge and counts one request.
This is deliberately conservative: a connection error does not prove that the provider did no work.
Cumulative usage events use the largest reported counters instead of summing repeated totals.
Receipts are idempotent and restore cost, usage, uncertainty, and monetary approval grants on replay.

A declared bound is not a provider-side spending control.
If actual usage exceeds it, Saige records the larger amount and returns an error.
Retries hidden inside a decorator can incur several charges for one outer call.
Set bounds for all attempts, or move accounting to the attempt boundary.
Price cards must also include relevant cache tiers. Native tool fees and cache storage remain host accounting.
Token and request limits are independent of monetary approval grants.

### Rate limits, context limits, and runtime failures

A rate limit or transport failure can occur before or after provider work starts.
The durable engine does not automatically replay failed external attempts.
The host can inspect the error, reconcile the outcome, and schedule a bounded retry with backoff and jitter.
Do not hold a worker asleep for a long retry delay; reschedule it in the host queue.
A provider retry wrapper must not hide billable attempts from a strict budget policy.

Set request and tool deadlines. A tool that ignores context can delay the batch and worker release.
A crash releases the process lock, but leaves uncertain external operations for reconciliation.
The library cannot recover from process termination or resource exhaustion inside the same process.
Use process or container limits when tools need stronger isolation.

Automatic compaction is rejected for local durable runs, including children.
A summary needs stable per-owner checkpoints before it can participate in deterministic replay.
For now, bound the context or create an explicit new run with a reviewed summary and a new revision.
A context-limit error is not fixed by retrying the same oversized request.
Factories, tool gates, and result policies must reproduce the same decisions during replay.
A provider call stopped by an interrupting submission commits its completed text with a `TruncationPart` marker and records a `step.truncated` event, so replay does not repeat the call.
If a policy needs time, randomness, or an external read, version or checkpoint that input in the host.

### Files, traces, and deployment limits

Each run uses a SHA-256 directory name containing `state.json` and `worker.lock`.
`state.json` is a versioned recovery snapshot. It includes steps, interrupts, receipts, and ordered event metadata.
Input and result fields contain base64 records: gob envelopes with a format version, holding messages and tool output in the shared part codec with their media bytes. Records written before typed parts are still read and upgraded on replay, so a run in flight across an upgrade resumes.
The snapshot is not the public JSON conversation trace. Use `tree.Print` for that separate export.
State files use mode 0600 and new run directories use mode 0700.
Protect the parent directory, retain required audit records, and remove secrets from application inputs.
The engine does not provide encryption, a retention service, or a portable export format.
Each update rewrites the snapshot. Large or long-running workloads need a different storage backend.
`Engine.List` reads every run's snapshot, `Engine.Leased` tells a live worker from a run orphaned by a crash, and `Engine.Delete` removes a finished run.

## The duraturo engine

`agent/durable/duraturo` runs an agent as a [duraturo](https://github.com/urmzd/duraturo) workflow.
duraturo keeps runs and their step records in a ledger and delivers run IDs to workers through a queue.
`ledger.NewMemory` and `queue.NewMemory` form a complete single-process system.
`github.com/urmzd/duraturo/adapters/postgres` maps both onto Postgres tables the host owns, so Postgres is the only infrastructure.
The adapter validates the tables and never migrates them; `pgledger.RecommendedDDL` and `pgqueue.RecommendedDDL` return suggested DDL.

```go
engine := duraturo.New(lgr, q) // any duraturo ledger and queue
wf := engine.Register("reviewer.v3", func(runID string) *agent.Agent {
    a, err := agent.New(agent.Config{Provider: configuredProvider, Tools: tools})
    if err != nil {
        log.Fatal(err)
    }
    return a
})
go engine.Worker().Run(ctx) // one or more workers, in this process or others

_, err := engine.Run(ctx, wf, "run-42", input)
if errors.Is(err, types.ErrSuspended) {
    state, err := engine.Inspect(ctx, "run-42")
    if err != nil { return err }
    // Show state.Pending to an authenticated user, then:
    err = engine.Decide(ctx, "run-42", state.Pending[0].ID, "decision-17",
        types.ApprovalDecision{Approved: true})
    if err != nil { return err }
    result, err := engine.Wait(ctx, "run-42")
}
```

The run ID is the idempotency key. Starting an existing run with the same workflow and input is a no-op; a different input returns `ErrConflict`.
The workflow name is the compatibility contract for recorded runs. Give a workflow a new name when its recorded steps would no longer replay.
Every worker must come from `Engine.Worker`, so it resolves the registered workflows. Keep duraturo's default JSON codec.

Each provider call and tool call is a duraturo step, keyed by its stable step name.
On every claim the workflow runs from the top: recorded steps return their results, and the first unrecorded step executes.
Steps run one at a time, because duraturo derives record keys from call order. Durable tool calls are therefore sequential.
A tool step's context carries duraturo's idempotency key under `types.ToolContextIdempotencyKey`; pass it to an external service that drops duplicate requests.

An approval or interrupt is a durable event. The run records the request and parks: it leaves the queue, stays pending and holds no worker.
`Decide` and `Router().Reply` record the reply once and enqueue the run, which replays to the waiting call and continues.
The same reply again is a no-op, a changed reply returns `ErrConflict`, and a late reply matches `types.ErrInterruptExpired`.
A reply that lands while its run is still parking is picked up by the worker's janitor; `worker.WithJanitorEvery` bounds that delay.

A step that started and left no result has an unknown effect ([D-14](../DESIGN_DECISIONS.md#d-14-keep-uncertain-effects-explicit)).
The run parks with `ErrIndeterminate` and does not call the step again.
`Inspect` lists the step with its saved budget reservation. `Reconcile(ctx, runID, step, &result)` records the verified result; a nil result permits one more attempt and keeps the uncertain charge.
A tool that returns an error is a recorded result, not an uncertain step. A panic is uncertain.

| Feature | `agent/durable/local` | `agent/durable/duraturo` |
| --- | --- | --- |
| Storage | Private directory, one file per run | Any duraturo ledger and queue; Postgres tables in production |
| Workers | One process lock per run | Any number of workers; fenced, time-bounded leases |
| Agent per run | Factory passed to `Run` | Factory passed to `Register`, called with the run ID |
| Approvals and interrupts | `Decide`, `Router`, `Inspect` | `Decide`, `Router`, `Inspect`; the run parks off the queue |
| Uncertain steps | `ErrIndeterminate`, then `Reconcile` | `ErrIndeterminate`, then `Reconcile` |
| Budget reservations before dispatch | Saved with the run | Saved as a step record |
| Tool steps | Concurrent within a batch | Sequential |
| Input log and `Append` | Yes | No; a run has one input |
| Revision binding | Revision passed to every call | Workflow name |

| Boundary | Exists now | Required for distributed workers |
| --- | --- | --- |
| Run ownership | Local OS process lock | Shared lease, fencing token, expired-owner recovery |
| Accounting | Shared in-process reservations, saved per-run receipts | Transactional cross-worker ledger and account-level limits |
| Scheduling | Host calls `Run` | Queue, retry policy, fairness, backpressure, worker admission |
| Approvals | Saved requests and decisions | Authenticated decision API and resume queue |
| Storage | Synced atomic local snapshots | Transactional journal, bounded retention, schema migration |
| Routing | In-memory sticky session | Persisted profile and configuration revision |
| External tools | Host-owned objects and services | Idempotency keys, sandbox isolation, effect reconciliation |

Kubernetes can place and restart workers. It does not replace these contracts.
Do not use this local lock as a distributed lease on a shared network filesystem.
The duraturo engine supplies the shared lease, fencing, queue and transactional record store. Account-level limits across runs remain host accounting.
