# Replay Design

This is the correctness spec: how a run re-executes to the same result any number of times, across crashes, retries, races, and releases. Everything here is implemented in `replay` (the algorithm), `worker` (the claim protocol), and the `ledger` / `queue` contracts (the primitives it stands on).

## Record Identity

A record's key follows the activity, not its position in the code:

| Call | Key |
|------|-----|
| Kth call to activity or step `name` in program order | `name#K` (zero-indexed occurrences) |
| Explicit user key via `CallKeyed` | `k:<key>` (order-independent) |
| Kth `Event` wait on `name` | `event:<name>#<K>` |
| Any of the above made inside another wrapped call | the same key prefixed with the parent's key (`process-order#0/charge-payment#0`) |

Identity-by-name is what lets code roll forward across releases with no version, patch, or pinning API. Inserting, removing, or reordering differently-named calls between releases leaves existing checkpoints valid; occurrence counters are per name and per parent scope, so unrelated edits never shift another activity's keys. The one deliberate act is a breaking input/output change: that is a new name (`charge-payment.v2`). Activities are the ABI.

Keys are hierarchical, and that is load-bearing: a memoized parent's body never re-executes, so its children's calls never happen on replay. If children shared the top-level counter space, every later same-name call would silently shift by the skipped occurrences; scoping each call under the executing parent's key makes the skipped subtree self-contained (`replay.Frame.nextKey`).

One consequence: call `Event` from the workflow body (the function handed to `Start`), not from inside a nested activity. `Client.Signal` targets top-level `event:<name>#<k>` keys; an event awaited inside a nested activity carries its parent's prefix, so its record must be written by hand.

## The Replay Algorithm

Every wrapped call bottoms out in `replay.Frame.Do`. The frame is built once per claim from `ledger.Load` (the one-round-trip prefetch of the run and all its records) and carried through user code in `context.Context`.

1. **Derive the key** as above, incrementing the occurrence counter for the name within the executing parent's scope.
2. **Hash the input** (activities only): sha256 hex of the marshaled input. Steps and events carry no hash; their payload is the non-determinism being captured, not an input to verify.
3. **Memo hit**: if the key has a record, verify it. Kind or name mismatch, or an activity input-hash mismatch, is `run.ErrNonDeterministic`: terminal and non-retryable, because identical code against identical records cannot succeed, so failing fast beats burning retry budget. The record's stored codec (`Record.Codec`, the `Codec.ContentType()` that produced its payload) is also verified against the frame's codec on every hit; a mismatch is terminal and non-retryable, because a payload recorded under one codec cannot be decoded by another and retrying cannot fix a configuration mismatch. A `RecordFailed` record replays as `*run.RecordedError` (itself non-retryable). A `RecordOK` record returns its output instantly. No user code runs.
4. **Miss on an event**: park. `Do` returns `run.ErrParked`; user code propagates it like any error (see Park and Resume).
5. **Frontier**: the first unrecorded call executes for real.
6. **Retryable failure**: nothing is written. The attempt's error propagates and the worker releases the run with `failed=true`, consuming one unit of retry budget; the next attempt re-executes this call.
7. **Non-retryable failure**: the failure itself is memoized as a `RecordFailed` record, then propagates to fail the run. It surfaces as `*run.RecordedError` even on the attempt that recorded it, so user code branching on the failure sees the identical value on every attempt: determinism outranks error-identity fidelity. Branch on `RecordedError` and its message, never on `errors.Is` against the original error value.
8. **Record, first write wins**: the result is written with `ledger.Record`, which is write-once on `(runID, key)`. On conflict it returns the already-stored record alongside `run.ErrAlreadyRecorded`, and the caller **adopts** the stored truth: verifies it, returns its output, and discards its own computed value. Concurrent attempts converge on exactly one history.
9. **Seal**: a best-effort `DeltaSeal` divider marks the key resolved (see Deltas and Dividers). The checkpoint is already durable; a lost seal degrades stream trimming, never correctness.

Records the workflow never reached (typically calls removed by a code change) are advisory: the worker logs them and the computed result stands.

The happy-path ledger write budget is two run-level writes, `Accept` and `Complete`, plus one `Record` per completed activity or step. Heartbeats never touch the ledger.

## The Claim Protocol

A claim carries two deliberately separate counters (`queue.Item{RunID, Attempt, Failures}`):

- **Attempt** is the fencing token. It is owned by the queue, increments on every claim, and is monotonic across the run's whole lineage, including park/resume cycles. Fencing identity is `(runID, attempt)`: every subsequent queue operation presents the pair, and a stale pair gets `run.ErrSuperseded`.
- **Failures** is the retry budget. It moves only when an execution genuinely fails: a `Release(..., failed=true)` after a retryable error, or an expiry reclaim (the execution died under its lease). Settling and re-enqueueing never touches it, so park/resume cycles, janitor backstop polls, and graceful-shutdown interruptions are free. `Run.MaxAttempts` bounds Failures, never claims.

Each claim lifecycle is `claim -> heartbeat* -> exactly one of settle | release | expire`. The worker drives it as:

1. **Claim** a run ID with a lease of `LeaseTTL`. The queue hands each claimable run (queued past ready time, or leased past its expired deadline) to exactly one caller and increments the attempt. Reclaiming an expired lease also increments Failures: the previous execution died. A worker that stops while its claim is in flight should not lose a lease the store grants anyway: a `Claim` on a done context takes nothing, a queue should wait out (bounded) the answer to a claim already sent rather than abandon it, and the worker releases a run claimed as it stops at once, unexecuted, with no delay and `failed=false`. A lease whose answer is lost belongs to nobody until it lapses, and the reclaim charges the run a failure it never had (rows 13 and 14 give each backend's bound).
2. **Load** the run and records from the ledger.
3. **Claim-time terminal check**: if the run is already terminal, settle without executing. This is the relocated reaper: the repair for an attempt that completed the run but died before settling, done lazily at the next claim instead of by a scanning process.
4. **Poison bound**: if `Failures >= MaxAttempts`, complete the run failed with `run.ErrMaxAttempts` and settle. The bound is on failures, not claims: crash-looping runs burn budget through expiry reclaims, but park/resume cycles and janitor backstop polls settle cleanly and cost nothing, so a run may wait on an event indefinitely without being terminally failed for waiting.
5. **Attempt divider**: append a best-effort `DeltaAttempt` divider to the delta log (when the queue has the capability).
6. **Replay to the frontier** under a heartbeat goroutine that renews the lease every `HeartbeatEvery` (default `LeaseTTL/3`). A heartbeat answered with `run.ErrSuperseded` cancels the execution context: the zombie learns it lost and stops.
7. **Outcome**, exactly one queue verb per claim, issued on a context detached from cancellation (`context.WithoutCancel`) so a worker shutting down mid-outcome still persists it:
   - success or non-retryable failure (including `ErrNonDeterministic`): `Complete` then `Settle`. `Complete` is a compare-and-swap from pending to terminal; the first terminal write wins and `run.ErrAlreadyTerminal` is tolerated, because a losing racer computed the same memoized prefix.
   - `run.ErrParked`: `Settle` only. The run leaves the queue but stays pending in the ledger, burning no retry budget.
   - retryable failure (including panics, which are recovered and consume budget like any other genuine failure): `Release` with backoff and `failed=true`, or terminal failure once the budget is spent.
   - worker shutdown mid-run: `Release` with no delay and `failed=false`. Interruption is not a failure of the code, so no budget is burned, and a run that finished or parked during shutdown gets that outcome persisted instead. Graceful shutdown never degrades into a crash: leases do not wait out their TTL and computed results are not discarded.
   - worker death: no verb at all; the lease expires and the next claim takes attempt N+1 with one more failure on the books.

**The janitor** is a fenced, idempotent repair loop safe to run on every worker. Each tick it walks the entire pending set older than one lease, paginating `ledger.ListPending` with its `(CreatedAt, ID)` cursor: parked runs stay pending indefinitely, so a fixed first page would starve newer lost runs behind them. It enqueues each run it finds; `Enqueue` is a no-op for runs already queued or leased, so sweeping the whole pending set is harmless. One loop recovers accepted-but-never-enqueued runs, rebuilds a wiped queue from ledger truth, and doubles as the backstop poll for parked runs (replay to the frontier is cheap; a still-unsatisfied run simply re-parks, and neither the poll nor the re-park consumes retry budget).

## Failure Modes

| # | Crash point | State left behind | Recovered by |
|---|-------------|-------------------|--------------|
| 1 | Submit crashes after `Accept`, before `Enqueue` | Run pending in the ledger, not queued | Janitor re-enqueues it via `ListPending`; a caller resubmit no-ops through idempotent `Accept`. The non-crash variant (`Enqueue` returns an error) is a degraded ack: `Start` returns the handle alongside the error, because the run is durable and the janitor will execute it |
| 2 | Submit crashes after `Enqueue`, before the caller sees the ack | Run accepted and queued; caller unsure | Resubmitting the same run ID returns a handle to the existing run; `Accept` and `Enqueue` are both idempotent |
| 3 | Worker dies after `Claim`, before any record | Leased run, empty history | Lease expires; the next `Claim` takes attempt N+1 and replays from the top (nothing to memoize yet) |
| 4 | Worker dies mid-run after k records | Leased run, k memoized records | Lease expires; the next attempt replays records 1..k instantly and resumes executing at k+1 |
| 5 | Worker dies after a side effect, before its record | The effect happened; no record says so | Replay re-executes the call: at-least-once. `IdempotencyKey(ctx)` = `runID:recordKey` hands downstream systems the dedup key that makes it effective-once |
| 6 | Two live attempts race one unrecorded key | Both executed the frontier call | `Record` is unique on `(runID, key)`: first write wins and returns the stored record on conflict; the loser adopts the stored truth and discards its own value |
| 7 | Worker dies after `Complete`, before `Settle` | Terminal run, live queue entry | Claim-time terminal check: the next claimer sees the terminal status in the ledger and settles without executing |
| 8 | Zombie (superseded attempt) still running | Two processes think they own the run | Heartbeat, settle, release, and delta appends are fenced with `run.ErrSuperseded`; its `Complete` is tolerated (first terminal write wins); its records are valid converged truth |
| 9 | Poison run failing every attempt | Failure counter at the budget | `Failures >= MaxAttempts` at claim: `Complete(failed, run.ErrMaxAttempts)` and settle. Only genuine failures (retryable errors, expiry reclaims) advance the counter; park/resume cycles, backstop polls, and shutdown interruptions never do, so a run merely waiting on an event is never poisoned. The exception is a claim whose answer is lost as the worker stops: the lease it took lapses, and the reclaim charges a failure (rows 13 and 14) |
| 10 | Code changed between attempts | Memoized record disagrees with the live call | Kind and input-hash verification: `run.ErrNonDeterministic`, terminal, no retry burn |
| 11 | Queue wiped | Queue and delta log gone; ledger intact | Janitor rebuilds the queue from `ListPending`; attempt and failure counters restart with the rebuilt queue (a pre-wipe zombie's stale pair still fails the fence); no run is lost, only observability history, accumulated retry burn, and at most one lease of latency |
| 12 | Postgres lost | The truth is gone | The ledger IS the truth, which is exactly why it lives in the user's own database, under the user's existing backup and recovery policy |
| 13 | Worker stops while its `Claim` is in flight | The store may have granted a lease the worker has not heard of yet | The queue waits out the answer to a claim already sent and returns the lease, and the stopping worker releases the run at once with `failed=false`, unexecuted. `queue.Memory` claims under its lock and loses no answer. `pgqueue` asks nothing once the stop lands while it waits for work or a connection, and waits up to 5s past the stop for a claim statement already sent; only a Postgres silent past that, or a connection broken after the commit, leaves the lease to lapse, recovered as in row 3 and charged a failure. |

## Park and Resume

Waiting is not a mechanism; it is the absence of a record. `Event[T](ctx, name)` reads the record `event:<name>#<k>`. If it exists, the payload returns instantly (on replay too). If not, `run.ErrParked` propagates up through the workflow, the worker settles the queue item, and the run stays pending in the ledger at zero cost: no lease, no polling loop, no retry burn. Settling and re-enqueueing never touches the `Failures` counter, so a run can park and resume any number of times without spending budget; the attempt still increments at each claim, keeping the fence monotonic across the whole park/resume lineage.

Call `Event` from the workflow body, not from inside a nested activity: record keys are parent-scoped, and `Client.Signal` targets top-level `event:<name>#<k>` keys (see Record Identity).

Resume is one write plus one enqueue, from anyone:

- `Client.Signal(ctx, runID, name, payload)` is the sugar: it writes the first unclaimed occurrence of the event record (racing signals skip to the next occurrence) and enqueues the run.
- A row written into your own table by your own code, followed by an enqueue, is exactly equivalent.
- The janitor is the backstop: parked runs are pending runs, so every sweep re-enqueues them; a run whose event is still absent replays cheaply and re-parks.

The replayed attempt finds the event record where it once found absence and continues past it. Human-in-the-loop with zero framework surface.

## Deltas and Dividers

Activities `Emit` streaming deltas persisted on the queue side behind the optional `queue.DeltaLog` capability (discovered by type assertion; no capability means `Emit` is a no-op). Deltas are advisory flow, never truth: losing them loses observability history, never correctness.

The framework writes two divider kinds into the same log:

- `DeltaAttempt` at the start of each claimed attempt.
- `DeltaSeal` when a record key is checkpointed in the ledger.

Consumer algorithm for a run's log, in append order:

1. Track the current attempt from `DeltaAttempt` dividers. User deltas belong to the attempt segment they fall in.
2. A `DeltaSeal` for a key means that key's segment is resolved by the ledger: the checkpoint is the truth, and the streamed deltas for it are sealed history. Checkpointed activities never re-stream, because replay hits the memo and never executes user code.
3. Live output is only the current attempt's unsealed tail. Segments from earlier attempts that were never sealed are superseded partial output; discard or gray them out.

`PriorDeltas(ctx)` exposes the raw payloads the currently executing record key emitted on earlier attempts, for provider-side resume of an interrupted stream. The read is exposed; nothing is applied automatically.

## Versioning and Rolling Forward

There is no workflow version, no patch API, no pinning. Code rolls forward against records that never change:

- Adding, removing, or reordering differently-named calls between releases is safe: keys follow names, existing checkpoints stay valid, removed calls surface as logged unconsumed records.
- Changing what a name means for I/O is a new name (`charge-payment.v2`). Old runs finish on the old records; new runs use the new activity.
- Divergence that verification can detect (same key, different kind, name, or input hash) fails loudly with `run.ErrNonDeterministic` instead of silently corrupting a run. Divergence it cannot detect (a step whose program order changed under the same name) is why steps whose order may vary need distinct names, and order-varying activity fan-out gets explicit keys via `CallKeyed`.
- A recorded payload that no longer decodes into today's type is deterministic corruption: terminal (`DecodeError`, non-retryable), fixed by a versioned name, never by retrying.
- Every record stores the `Codec.ContentType()` that produced it, and replay verifies it on every memo hit. A mismatch (a fleet replaying JSON-recorded runs under a different codec) fails terminally rather than decoding garbage: client and worker must agree on the codec, and the recorded content type is what makes disagreement loud.
