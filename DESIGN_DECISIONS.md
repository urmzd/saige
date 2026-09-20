# Design decisions

These decisions describe Saige's orchestration and cache boundaries.
See [the API guide](docs/orchestration-policies.md) and [the cache audit](docs/cache-contracts.md) for examples and known limits.

## D-01: Keep branches separate from workers

A branch records an alternate conversation path. It does not establish a process, tool, or database boundary.
Each subagent invocation gets a new tree. The host isolates external state when required.
This avoids a false isolation guarantee from a thread-safe tree.

## D-02: Make delegation a call-and-return contract

A subagent always returns to its caller. Its default output is the final assistant message.
The complete committed trace remains available through a result handle or sink.
A result policy selects and validates data for an upstream service.
This prevents intermediate thoughts from becoming an apparent final answer.
A failed child returns an error and partial trace, not successful output.

## D-03: Treat handoff as ownership transfer

A handoff recipient can finish, continue elsewhere, or transfer back when it cannot answer.
There is no mandatory return to a waiting caller.
The audit tree retains all messages. The default provider view contains only the owner's context and transfer briefs.
A returning owner resumes its earlier view.
This supports triage without copying every specialist's transcript into every request.

## D-04: Add direct return links by default

Every declared handoff edge gets a reverse edge by default.
This gives a specialist a route back when the task is outside its scope.
A strict directed policy can remove that behavior at a trust boundary.
The transfer limit remains necessary because a return edge can also create a loop.
Multiple transfers in one turn are rejected because one conversation cannot have two simultaneous owners.

## D-05: Select complete model configurations

A route refers to an immutable provider configuration, not only a model string.
Each configuration has its own reasoning, sampling, endpoint, and cache settings.
The routing policy acts after context and tool selection, before provider execution.
Sticky selection is the default because repeated prefixes benefit from stable provider affinity.
A transient failure permits failover before output or usage commits the attempt.
After commitment, the request fails rather than mixing streams from two providers.

## D-06: Separate cache contracts

A response cache reuses output. A prompt cache reuses provider computation.
A durable step record prevents repeated work during recovery. A journal records authoritative changes.
These layers have different identity, expiry, billing, and correctness requirements.
No cache substitutes for a journal or a durable budget ledger.
The cache audit lists unsupported combinations and remaining defects.

## D-07: Default to private response-cache identity

The SDK cannot infer a tenant's authorization scope or the meaning of a deployment revision.
A cache wrapper therefore uses a private identity unless the host supplies scope and configuration keys.
Explicit sharing remains possible, but the host owns that contract.
A model name alone is insufficient because two configurations of one model can produce different results.

## D-08: Keep policies separate

Tool disclosure, execution permission, context selection, result selection, links, routing, and budgets use separate contracts.
A skill can supply content. It cannot grant permissions by itself.
A memory service can supply evidence. It cannot silently change system authority.
This allows a service to replace one policy without replacing the agent loop.

## D-09: Reserve budget before dispatch

A shared `Budget` reserves request, token, and cost capacity under one lock.
This prevents concurrent children from spending the same available allowance.
The host supplies per-call upper bounds. Without a bound, a call reserves the remaining allowance for that limit.
For example, four reservations of $0.25 can occupy a $1 allowance. A fifth call receives `ErrBudgetBusy`.
Settlement releases unused capacity and records actual usage once. Missing usage consumes the reservation and remains uncertain.
Local durable runs save reservations before dispatch and restore settlement receipts during replay.
This does not include a distributed account, provider-native fees, or cache storage invoices.

## D-10: Save approvals and release local workers

Streaming approvals park a goroutine but do not hold a regular tool execution slot.
The local durable engine saves each request and returns `ErrSuspended` after the current batch drains.
Completed siblings remain saved. A later worker replays those results and resumes the approved call.
For example, an independent read can finish while a write waits for approval.
Decisions require the original run revision and an idempotency key. Changed decisions and expired requests fail.
The host authenticates users and preserves separate decisions when its interface groups approvals.
A local process lock protects one run. Remote workers still need a shared lease and fencing contract.

## D-11: Fail closed on handoff compaction

A shared summary can erase ownership boundaries and expose another owner's context.
Automatic compaction is therefore rejected for handoff groups.
Per-owner checkpoints and summaries must exist before this restriction can be removed.
This is a deliberate limit, not an implicit promise that long handoff sessions fit every model.

## D-12: Reject unsupported request controls

Adapters validate configured controls before they send a request.
For example, temperature on an `o3` request returns `ErrInvalidModelConfig`.
The adapter does not silently remove the requested value.
This makes a configuration error visible and prevents retrying the same invalid request.
Each route must have settings that its own model accepts.
Validation preserves prompt cache options and reported cache usage.

## D-13: Own catalog snapshots at the boundary

The catalog copies mutable metadata on registration and on return.
A caller can edit a lookup result without changing another session or a stored revision.
For example, changing a returned effort list does not modify the next lookup.
A deliberate change requires a new registration, which records a revision.
An exact model declaration sets `Known=true`. A family inference sets it to false.
This separates a declared model from an unverified variant that has a similar name.

## D-14: Keep uncertain effects explicit

A completed step is saved before its result returns. A started step is saved before its function runs.
A crash between those records does not prove whether an external write succeeded.
Recovery therefore stops with `ErrIndeterminate`; it does not repeat the write automatically.
The host checks the external system, then supplies a result or explicitly permits retry through `Reconcile`.
For example, check a payment's idempotency key before allowing another payment attempt.
Local snapshots cannot provide an atomic transaction with an external API.

## D-15: Copy cache values at ownership boundaries

A read must not let one caller alter another caller's future result.
Tool results and embedding vectors are copied before storage and before return.
Embedding keys include the full input, including binary data and MIME type.
Tool keys include a configuration revision, scope, policy, arguments, and declared context.
Invalid key data fails explicitly. No fallback string is treated as a stable identity.
Stale data stays in storage through its stale window, but only a successful old result can mask a refresh failure.

## D-16: Use a local engine before a distributed scheduler

The local engine uses process locks, synced files, and atomic rename.
This gives one machine a testable crash boundary without a queue or database deployment.
An OS lock releases when its process exits. Pending approvals need no resident worker.
Snapshots use private files and contain versioned recovery data plus ordered event metadata.
They are not a portable trace format or an unbounded production journal.
A distributed implementation must add fenced leases, transactional reservations, scheduling, and retention.
See [durable execution](docs/durable-execution.md) for the failure table and deployment limits.
