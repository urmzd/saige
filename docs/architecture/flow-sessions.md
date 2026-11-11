# Agent sessions

A session is one agent's conversation, keyed by `(tenant, parent, agent, label)`.
The parent is a place URI or another session ID. Reopening a key continues its
journal. A label isolates an independent pass; `Fresh` generates a new label.

`flow/session` names the conversation (`session.Store`: `Open`, children,
links, run indexes, grants, reservations), `flow/thread` keeps its journal,
and `flow/runtime` runs its turns. A host opens or continues a session through
the store and accepts a run on it with the runtime.

Hosts authenticate and authorize before accepting a run. `AcceptOwnSession`
preserves a host's already unique per-agent event ID. Its child variant records
the durable parent and question for terminal-result notification.

## Agent tools and governance

`runtime.WithSessionRuntime` installs `session_call`, `session_listen`,
`session_read`, `session_status` and `session_link` in the sandbox. Calls use the
parent's pinned `Access.Spawn`. The pinned policy's `session_depth` defaults to
3 and `open_children` to 8. Admission atomically reserves a slot per parent across
replicas. Retrying a call reuses its reservation; only failed admission or a
terminal ledger result releases it.

`runtime.WithSessionTools` limits those verbs to the ones a host names. A verb
left out is not offered to a turn and does not resolve for a spec that names
it. With no names, a turn is offered none: a host that calls agents through its
own tool (`runtime.WithCallTool`) can keep `session_read` and `session_link`
and leave the calling verbs out. Sessions themselves do not change.

A recorded parent stop fences child admission before and after reservation.
If acceptance races the stop, the runtime records the child's stop before
acknowledging the call, including calls started with `wait: false`.

`session_call` continues a child by default. `label` and `fresh` isolate contexts.
`wait: false` returns its session and run IDs. `session_listen` snapshots up to
eight sessions' current runs and parks on their terminal results. With
`until: "message"`, it pins transcript heads and waits for later entries, including
sessions with no run yet. Optional `cursors: [{session, since}]` supplies last-seen
positions. Results include bounded messages and their next cursors. The worker's
janitor repairs missed notifications, including future messages. Both waits are
durable and release their worker. No terminal state is inferred from silence.

Agent reads stay within their tree unless an `AccessStore` explicitly grants
access. A context or evidence link grants nothing. Grants and revocations are
authenticated host operations. They never cross tenants and are not inherited
by another agent merely because it is a child of the granted session.

A host uses `runtime.WithChildStarter` for its own governed admission path. The
hook must use `SessionCall.ID` as its idempotency key, return immediately after
durable acceptance, and never poll for an answer. `WithChildRunID` supplies its deterministic run ID before admission;
the starter must return that ID. Recovery preserves accepted work and fences
abandoned admission with a failed ledger record before releasing the slot.
`WithSessionRuntime` must also be set on a host-created sandbox.

## Lineage

Every event of a run carries its lineage: `Root`, `ParentRun` and
`ParentToolCall`. A root run is its own root and names no parent. A host sets a
child's lineage with `runtime.WithLineage` on the context it accepts the child
with. `AcceptOwnChild` and the session child admissions take the root and
parent from the durable parent, so the host passes only the call id. A
delegation flow starts itself is called through the parent's tool call, by
the id the parent's `run.delta.tool.call` events carry. A `ChildStarter`
receives that lineage on its context. A turn reads its own from
`Request.Lineage` and passes `Lineage.Child(RunID, call)` to a run it starts.
The lineage is fixed in the durable input, so a retry, replay or resume
stamps the same values. A run without lineage stores none.

An accept fails with `ErrInvalid` when a call names no parent run, a parent
run has no root, a run that no run called names another root, a run is its
own parent or the called root of its tree, or a child admission names a
parent or root other than the durable parent's.

## Storage and roles

`runtime.WithSessions` takes the store: `session.NewMemory`, or
`session.NewPostgres` on the caller's pool, which persists identity, links,
run indexes, grants and reservations. Custom stores
must implement `AdmissionStore` for agent calls, or admission fails closed.
Without `AccessStore`, cross-tree reads are denied.

Unlabelled surface sessions retain their existing `fiber_thread_*` journal keys.
Children and labelled sessions have isolated keys. Old durable inputs replay
without session metadata and keep their original journal identity.

## Budgets and live usage

A standalone session run requires a positive `limits.budget`. Each subagent call
also requires its own explicit `subagents[].budget` allocation. Zero grants no
work. `BudgetStore` atomically reserves parent capacity across concurrent calls,
persists integer cost, token and request caps, and settles immutable receipts.
Each native turn receives the remaining reservation through saige's budget.
Unpriced models require token or request caps without a money ceiling. Unknown
spend retains the reservation. An abandoned attempt is charged conservatively;
a failed budgeted turn ends rather than retrying beyond its reserved allowance.

A completed parent account can retain outstanding child reservations. Late child
or grandchild receipts settle through closed ancestors, once, after restart.
`UsageStore` reads live tree totals: actual model call IDs
are deduplicated at every ancestor, so child totals are never added a second
time. Completed run outputs and events remain immutable snapshots.

The live usage index is repaired from retained per-call receipts before a turn
finishes or starts its next wait. Temporary index failures park a memoized
reconciliation step. The janitor resumes it after recovery or worker restart,
without repeating completed model work or consuming its retry budget. Replayed
AccountingIDs remain idempotent; uncertain provider spend is never invented.

Hosts with `WithChildStarter` own budget admission and native request caps; flow
does not reserve the same money again. Legacy `Accept` and `AcceptOwn` inputs
without budget declarations remain compatible. Explicit session posts and
standalone child calls fail closed without a grant or capable budget store.
Custom turn runtimes receive `turn.Input.Budget` and must enforce those caps.

## Verification

Race tests cover identity creation, labelled and fresh history, tenant isolation,
explicit grants and revocation, concurrent admission and budget reservations,
immutable settlement receipts, runtime restart with late descendant settlement,
usage redelivery, native request caps, and both terminal and message waits with
only one worker. Postgres tests require `SAIGE_TEST_POSTGRES_DSN`.

Legacy
host-admitted children use `AcceptOwnChild`: parentage must name a retained run
in the same tenant, remains immutable across concurrent retries, and records no
question or automatic parent resumption. The host still owns admission policy
and budgets.
