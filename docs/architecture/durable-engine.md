# Durable engine

`github.com/urmzd/saige/durable` makes existing Go code durable without rewrites. Users wrap functions, run a worker, and their code survives crashes: on retry the workflow re-executes from the top, recorded calls return instantly from the ledger, and the first unrecorded call executes for real. This document describes the beta architecture; interfaces are stabilizing but pre-1.0.

## Three Concepts

1. **Ledger** (durable truth): runs and their memoized records, stored in tables the caller already owns. `ledger.Ledger` is a pure interface: an idempotent keyed insert (`Accept`), a conditional pending-to-terminal update (`Complete`), a prefetching read (`Load`), a write-once record insert (`Record`), and a cursor-paginated pending scan (`ListPending`). Any store that can do those five things qualifies. A point read of one run (`GetRun`) is the optional `ledger.RunGetter` capability, which result polling prefers over `Load`'s full record prefetch; a read of a run's records by key prefix (`RecordsWithPrefix`) is the optional `ledger.PrefixReader`.
2. **Queue** (flow and clock): delivery of run IDs under fenced, time-bounded claims. `queue.Queue` is `Enqueue`, `Claim`, `Heartbeat`, `Release`, `Settle`. Every claim carries two separate counters: `Attempt`, the fencing token that increments on every claim, and `Failures`, the retry budget that moves only when an execution genuinely fails (a `Release` with `failed=true`, or a lease that expired under its worker). Waiting is free: park/resume cycles never touch the budget. The queue carries only IDs; data lives in the ledger, so every queue backend is disposable and rebuildable from ledger truth.
3. **Worker** (a pull loop, no inbound surface): claims runs, re-executes them under replay to the frontier, persists exactly one outcome per claim. It exposes no listener; every arrow points outward, to the ledger for truth and the queue for flow. It can be the submitting process itself (a goroutine) or a separate fleet; workers coordinate through nothing but the two interfaces. Graceful shutdown is an interruption, not a crash: outcomes are written on a cancellation-detached context, and an interrupted run is released immediately without burning retry budget. A claim in flight when the worker stops is waited out by the queue rather than abandoned, within each backend's bound, and the run it took is released the same way, unexecuted ([durable-replay.md](durable-replay.md), failure mode 13).

There is no owned schema and no server, and the core packages import only the standard library. saige's agent runs build on it: `agent/durable/anchor` makes every model call and tool execution a memoized step ([durable execution](durable-execution.md)).

## Package Graph

The packages form a strict one-way DAG:

- `run` is the leaf: runs, records, deltas, sentinel errors, the codec, the type-erased registry. Stdlib only, imported by every other package, imports none of them.
- `ledger` and `queue` define the two interfaces, each with a complete single-process `Memory` implementation (the queue's also implements `DeltaLog`). Their conformance suites, `ledger/ledgertest` and `queue/queuetest`, are the executable contracts every backend must pass.
- `replay` is the correctness heart: the `Frame` that memoizes re-execution (see [durable-replay.md](durable-replay.md)).
- `worker` is the consumer loop over ledger, queue, and replay.
- The root package (`durable`) is the user-facing SDK: `Activity`, `Step`, `Event`, `Emit`, `Client`, `Start`/`Exec`. It imports only `run`, `ledger`, `queue` and `replay`, never `worker` and never an adapter.
- Generics stop at the root: `ActivityFn[I, O]` compiles down to a type-erased `run.WorkflowFn` (bytes in, bytes out) in `run.Registry`, so `worker` never sees a type parameter.

Nothing below the root imports it, and adapters are invisible to all of it.

## Dependencies

`durable`, `run`, `ledger`, `queue`, `replay` and `worker` import only the standard library. The Postgres adapters under `durable/adapters/postgres` are the only engine packages that import pgx, so a program that never imports them never links it.

## The Postgres Adapters

**adapters/postgres** maps the ledger onto existing user tables via a declarative mapping (`pgledger`). `Validate` introspects the live schema and errors with guidance when a column is missing or mistyped; `RecommendedDDL` returns suggested DDL as a string and never executes it; its run IDs and record keys are `COLLATE "C"`, so the database orders and prefix-matches them byte by byte, as Go does. Adapters validate, never migrate: they run no DDL on their own. Two contract details: mapped identifiers are lower_snake_case only, and the uniqueness the ledger depends on (the runs table's run-ID column, the records table's run-ID and key pair) must be a plain unique constraint or unique index: non-partial, non-expression, non-deferrable. Partial and expression unique indexes are not accepted, because the adapter's `ON CONFLICT` inference cannot use them; `Validate` rejects them rather than letting first-write-wins silently degrade. The same directory ships `pgqueue`, a Postgres queue built on `FOR UPDATE SKIP LOCKED`, so a Postgres-only deployment (ledger and queue in one database) is fully supported. `pgqueue.NewDeltaQueue` adds the `DeltaLog` capability on a second, append-only table mapped the same way (`DeltaMapping`, `RecommendedDeltaDDL`): each append takes a per-run transaction-scoped advisory lock, checks the queue row's attempt counter (the fence), inserts, and `NOTIFY`s in one round trip, so a run's sequence order is its commit order and cursors never skip a late commit. Watchers share one `pgkit.Listener` subscription, held only while a `Watch` is live, and fall back to polling when the connection is unavailable (for example behind a transaction-pooling proxy); `pgqueue.WithListener` hands the queue the process's Listener so one connection serves every component. A plain `pgqueue.New` queue stays without the capability. A stop mid-claim does not abandon a claim statement already sent: `Claim` runs it on a context the caller's cancellation does not reach and waits up to 5s past the stop for the answer, so the stopping worker gets back, and releases, a lease Postgres committed; only a Postgres silent past that, or a connection broken after the commit, leaves the lease to lapse.

**adapters/postgres/pgkit** is the stack's shared Postgres plumbing, built once here so the layers above stop hand-rolling it. `Apply(ctx, db, name, steps...)` runs idempotent steps in one transaction under a transaction-scoped advisory lock derived from `name`, so processes that start together migrate one at a time and a crashed migrator never leaves the lock held; it keeps no version table, and it runs only the statements its caller passes (the adapters' `RecommendedDDL` strings are one choice). `Listener` multiplexes `LISTEN` over one dedicated connection taken out of the pool while anything is subscribed: `Listen(ctx, channel)` returns a Go channel closed when `ctx` ends, the connection reconnects with backoff, and every subscription gets a `Resync` message when its channel is (re)listened, and in place of whatever a full buffer drops, meaning "re-read your state". A slow subscriber never blocks the others.

## Capabilities, Not Interface Growth

Optional powers are capability interfaces discovered by type assertion, never widenings of `Ledger` or `Queue`. Two examples set the pattern:

- `queue.DeltaLog`: `queue.Memory` and `pgqueue.DeltaQueue` implement it, and a backend without it simply makes `Emit` a no-op and skips dividers. The worker discovers it once, at construction; the client asserts per call inside `Client.Watch` and returns an error when the queue lacks it.
- `ledger.RunGetter`: a point read of one run (`GetRun`) without its records. `Handle.Result` polls with it when present and falls back to `Load` (which prefetches the full record history on every poll) when not. Implement it on custom ledgers.
- `ledger.PrefixReader`: a run's records whose keys start with literal prefixes (`RecordsWithPrefix`), for a poll that wants a few small records (an event's arrival, a stop request) without reading every other one. `ledger.Memory` and `pgledger` implement it, and `ledgertest.Run` exercises it whenever a ledger does.

## Deltas and Dividers

Activities can `Emit` streaming deltas (progress, token streams). Deltas persist on the queue side, in the `DeltaLog`, because they are flow, not truth: losing them loses observability history, never correctness. The framework writes attempt dividers (`DeltaAttempt`) at each claim and seal dividers (`DeltaSeal`) at each checkpoint, so consumers can split superseded partial output from live tail and sealed history. Checkpointed activities never re-stream: replay hits the memo and user code never runs. `Client.Watch` tails a run's stream; `PriorDeltas` exposes earlier attempts' output for provider-side resume. The consumer algorithm is specified in [replay.md](replay.md).

## Wait as Absence

`Event[T](ctx, name)` is not a suspend mechanism; an event is simply a record that does not exist yet. Present: the payload returns instantly. Absent: `run.ErrParked` propagates, the worker settles the queue item, and the run stays pending at zero cost, with no retry budget spent no matter how long or how often it waits. Resume is one record write plus one enqueue, from anywhere: `Client.Signal` is the sugar, a row in your own table plus an enqueue is equivalent, and the janitor is the backstop. Call `Event` from the workflow body, not inside a nested activity: record keys are parent-scoped and `Signal` targets top-level `event:<name>#<k>` keys. Human-in-the-loop approval flows need no framework surface at all.

## Versioning Stance

Record identity is `name#occurrence`: keys follow the activity name, not the call site, and a call made inside another wrapped call is scoped under its parent's key (`process-order#0/charge-payment#0`), so a memoized parent cannot desynchronize its siblings' occurrence counters. Code rolls forward across releases with no version, patch, or pinning API; checkpoints stay valid when other, differently-named calls are inserted, removed, or reordered; detected divergence fails loudly with `run.ErrNonDeterministic` (terminal, no retry burn). Every record also stores the codec content type that produced it, and replay verifies it on each memo hit, failing terminally on mismatch. A breaking input/output change is a rename (`charge-payment.v2`). See [replay.md](replay.md) for the full rules.

## Adoption Is Monotone

Outside a run, every wrapped function is a plain Go call: `Activity` and `Step` pass straight through to the wrapped function, `Emit` is a no-op. Tests, scripts, and codebases mid-migration keep working unchanged; durability is added by running the code under a worker, not by changing the code.

## Roadmap

- Distributed activities (activities executed by a different worker than the run's)
- Forking and child runs as client-side compositions (`ParentRunID` and record-key namespaces are the reserved seams)
- Durable timers
- Multi-goroutine workflow bodies (v1 workflow bodies are single-goroutine; the frame is mutex-guarded so violations surface as loud determinism errors, not corruption)
- Wrapping codecs (encryption, claim-check) behind `run.Codec`
