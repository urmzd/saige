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

## D-09: State the budget limit accurately

The current budget counts reported usage after execution. It is not a reservation service.
Children share it by default, but concurrent requests can exceed the ceiling together.
Cache storage, missing usage, and provider-native tool fees require additional accounting.
Distributed execution needs durable admission and settlement before it can claim a strict monetary ceiling.

## D-10: Do not claim durable interrupts

An approval currently parks a goroutine and retains a worker slot.
Independent calls can continue, but the parent waits for its tool batch.
Approval IDs are registered before delivery and child IDs are scoped to their caller.
Process-independent interrupts still require persisted decisions, worker leases, and resumable continuations.
The host must preserve per-call decisions even if its user interface groups them.

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
