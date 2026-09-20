# Durable execution on one machine

`agent/durable/local` saves a run between worker invocations.
A run has one process owner at a time. Different run IDs can execute independently.
The engine requires Unix and a private local filesystem with reliable advisory locks and atomic rename.
It does not provide remote scheduling or an atomic transaction with an external service.

## Start, decide, and resume

```go
engine := local.New("./private-runs")
input := []types.Message{types.NewUserMessage("Review and apply the change")}
factory := func() *agent.Agent {
    return agent.NewAgent(agent.AgentConfig{
        Provider: configuredProvider,
        Tools: types.NewToolRegistry(types.WithMarkers(writeTool,
            types.Marker{Kind: "approval"})),
        Budget: types.NewBudget(types.BudgetPolicy{
            Limit: types.USD(1), PerCallCost: types.USD(.10),
            MaxTokens: 20000, PerCallTokens: 4000, MaxRequests: 10,
        }),
    })
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
Use the same input and revision to resume. A changed value returns `ErrConflict`.
Do not share a mutable tree between factories or load a partially reconstructed tree into the factory.
Tool objects and external services remain the host's isolation responsibility.
Local durable children must share the root budget. An independent child budget is rejected because reconciled receipts belong to the run ledger.
Result sinks and policy hooks can run again during replay. Make external writes in those hooks idempotent; they are outside the step journal.

Each pending approval has its own ID and expiry. The default expiry is 24 hours.
Gate approvals and marker approvals have separate IDs, including within subagents.
An identical decision retry is accepted before the run closes. A conflicting retry is rejected.
The host authenticates the person and checks their authority before calling `Decide`.
A modified argument map becomes the approved call's arguments.

## Execution and recovery

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

## Budget admission and usage

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

## Rate limits, context limits, and runtime failures

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
If a policy needs time, randomness, or an external read, version or checkpoint that input in the host.

## Files, traces, and deployment limits

Each run uses a SHA-256 directory name containing `state.json` and `worker.lock`.
`state.json` is a versioned recovery snapshot. It includes steps, interrupts, receipts, and ordered event metadata.
Input and result fields contain base64 gob data to preserve Go content types and binary tool results.
The snapshot is not the public JSON conversation trace. Use `tree.Print` for that separate export.
State files use mode 0600 and new run directories use mode 0700.
Protect the parent directory, retain required audit records, and remove secrets from application inputs.
The engine does not provide encryption, a retention service, cross-version gob migration, or a portable export format.
Each update rewrites the snapshot. Large or long-running workloads need a different storage backend.

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
The DBOS runner keeps its own step semantics and does not yet implement `ApprovalRunner`.
