# Flow: events and the durable turn

> **v0 alpha, pre-1.0.** `github.com/urmzd/saige/flow`. This document describes the architecture as built and
> tested. Interfaces may change between minor versions. Where something is
> designed but not yet finished, it is called out in
> [Limitations](#limitations) rather than described as though it exists.

flow is an agent library and a low-latency pub/sub bus. Store an agent as a
saige definition, make it a member of a channel, publish an event on that
channel, and the run's events stream to anyone subscribed and are delivered
wherever the channel leads.

The single idea the whole design turns on: **where an event arrives and where
its answer lands are independent.** A GitHub comment can be answered into Slack.
A schedule nobody watched can answer into both. That independence is what makes
this a mesh rather than a collection of integrations, and almost every decision
below follows from it.

The event union, the channel bus, definitions and the `Runtime` seam are
specified in [flow-events.md](flow-events.md). This document covers how a run behaves:
delivery, durability, stopping, compaction, usage and parking.

## What each layer owns

flow composes the rest of saige and adds events, channels, delivery and the durable turn.

| Concern | Owner |
|---------|-------|
| The agent loop, typed deltas, RAG, citations, versioned agent definitions | saige's `agent/` and `rag/` |
| Durability, replay, exactly-once effects, per-run journal | [`durable/`](durable-engine.md) |
| Sandboxed execution nodes, NGAC access control | [`sandbox/`](sandbox.md) |
| The event union and its catalog, the channel bus, membership, delivery | flow |
| Definition storage, pins and the Policy sidecar | flow |
| Ingress and egress adapters, and native actions | flow |

The division is strict. durable owns whether work survives a crash; sandbox
owns what a tool is allowed to touch; saige owns what the model does and what
an agent is. flow owns addressing, distribution, and the control plane, and
delegates the rest.

## One envelope, one catalog

Every event is an `event.Envelope` whose `Data` is a typed `event.Body`. The
envelope's `type` is the only discriminator and carries the version:
`message.posted.v1`, `github.pull_request.opened.v1`, `cron.tick.v1`,
`run.delta.text.v1`, `run.completed.v1`, `run.usage.v1`. Every type is
registered once in the catalog with its decoder, a renderer that turns the body
into model input, and a sensitivity flag. A test fails when a type has no
renderer. A type this build does not know decodes to `event.Unknown`, which
keeps its bytes and is never an error for a subscriber that did not ask for it.

A run is started by an event, not by free text. A chat message is
`message.posted.v1` with a `text` field; the model reads the catalog rendering
of whatever event started the run.

## Channels and subscriptions

The bus carries every event on a channel: a URI such as `slack://T01/C02`,
`github://acme/handbook/issues/42` or `cron://acme/nightly`. A subscriber names
a tenant, channel prefixes and type prefixes:

```go
bus.Subscription{
    Tenant:   "acme",                       // from the principal, never a request
    Channels: []string{"github://acme/"},   // channel prefixes
    Types:    []string{"run.completed"},    // type prefixes
}
```

Subscriptions are default deny. An empty tenant, channel list or type list
matches nothing, so there is no "everything" subscription. Prefixes are segment
aware: `slack://T01/C0` does not cover `slack://T01/C02`, and `run.delta` does
not cover `run.deltas.v1`. Opt-in families (`run.usage`, `run.context`) reach a
subscription only when it names them. A control channel
(`control://<tenant>/<verb>/<hash>`) matches only a subscription that names it
exactly.

Some things are deliberately not part of a channel:

| Not in the channel | Is instead | Why |
|---|---|---|
| Event type | A type prefix on the subscription | All events of a conversation stream the same way. A consumer who wants the whole conversation should not fan in from several channels and re-order them. |
| Agent and version | Fields on the envelope (`agent`, `version`, `digest`) | Watching one version is a filter, not a channel that appears and disappears as agents change. |
| Tenant | An authorization scope | flow authenticates nothing (see [No transport](#no-transport)): the program composing it sets the tenant from the caller's credential, so a subscriber cannot ask for another tenant's traffic by spelling it. |

## Members wake, adapters carry

An agent is woken only by channels it is a member of. `flow/membership` records
`(tenant, channel prefix, agent, Rule)`; a `Rule` lists the type prefixes the
member wakes for and, by default, wakes it only when the event names it (a
mention, a review requested from it, an issue assigned to it, a direct
message). A store with no rows wakes nobody.

The host starts one run per member an ingress event wakes; flow ships no
dispatcher. A host drops the event for the subscriber that produced it, so an
agent's own output never wakes itself, and drops an event at the hop limit
(`bus.DefaultMaxHops`), so two agents cannot answer each other forever. The run
id is `runtime.RunKey(tenant, event id, member)`: delivery is at-least-once,
and a redelivered event joins the run it already started.

Surfaces are adapters on either side of the bus:

| Face | Role |
|------|------|
| `Trigger` (ingress) | Verifies a request, translates it into one typed event, publishes it on the surface's channel. It picks no agent. |
| `Sink` (egress) | Subscribes by channel and type prefix and renders what arrives onto its surface. |
| `Actor` | Contributes native verbs the agent can call as tools. |

| Connector | Ingress | Egress | Actor |
|-----------|:-------:|:------:|:-----:|
| slack | yes | yes | yes |
| github | yes | yes | yes |
| cron | yes | no | no |

A schedule has no surface to deliver to, so cron only publishes. Roles are derived by type
assertion in `connector.RolesOf` rather than declared, so a connector cannot
advertise a face it does not have.

No connector ever calls another connector. Ingress publishes, egress
subscribes, and the bus is the only thing in between. Adding a destination is
adding a subscriber.

The `Actor` face is what turns "the agent replied" into "the work got done".
Actions run under the agent's NGAC policy rather than freely, and each one that
fires emits a `run.action.taken.v1` event, so the work is auditable on the same
bus as the conversation that caused it.

## N:M delivery

One event fans out to many egress adapters; one adapter serves many agents.
Each adapter chooses its own types, so granularity is per destination.

A worked example:

```
Event:     github.issue_comment.created.v1 on github://acme/handbook/issues/42

Egress 1:  Slack sink    channels: github://acme/handbook/   types: run
Egress 2:  GitHub sink   channels: github://acme/handbook/   types: run.completed
```

Slack receives every delta and edits one message as the answer arrives.
GitHub receives exactly one event and posts when the work landed. Same run,
same bus, same events. Selectivity is a type prefix on the subscription, not a
separate channel and not a separate run, which is why adding a destination
costs nothing on the agent side.

Delivery is at-least-once, so sinks must be idempotent. The shape that makes
that natural is one message per run, edited as deltas arrive, keyed by run id.
Posting a message per delta would be correct and unusable.

Slack and GitHub accept `Config.DeliveryState` and `DeliveryScope`.
`deliverystate.NewPostgres(pool)` plus `Migrate(ctx)` shares each run's native
message handle, complete rendering snapshot, deduplication window and pending
edits between replicas. `NewMemory` supplies the same contract in process.
Use one stable scope per connector client; the connector adds its tenant and
surface name. Configuring shared state requires `Config.Tenant`, and delivery
refuses a different event tenant. GitHub comments and checks have distinct scopes.

The event and dirty document commit before any external API call. A second
transaction holds one row lock across the surface write and reloads the current
document, so a delayed old replica cannot overwrite a newer render. If the API
call times out and aborts that transaction, the earlier dirty snapshot remains
available for recovery. Coalesced or failed edits are saved before acknowledgement. Schedule connector `Flush`
with a bounded context: it recovers up to 256 dirty rows per call, including
rows a previous replica left behind even when no further run event arrives.
Hosts pass this store to every connector they construct.

External API creation and the database commit are separate operations. A crash
after the API creates a message but before the handle commits can still cause a
duplicate unless the surface supports recovery or idempotent creation. Stores
retain records until the host removes them; removing a record also removes its
redelivery protection. Without `DeliveryState`, handles remain local to the
connector's bounded cache.


Egress preserves ordering per `(run, target)` pair and parallelises across
targets and runs. Ordering within a pair matters concretely: a Slack message
edited out of order shows the wrong final text.

## Definitions are pinned per run

Agents are saige definitions (`.agent.md`) plus a flow `Policy` sidecar, kept
per tenant in an insert-only `definition.Source`. A version, once written,
never changes. At accept time a run resolves the highest version and writes a
`definition.Pin` into its durable input: the name, the exact version, saige's
resolved digest, the exact version of every sub-agent and the policy hash.

This is a correctness requirement, not bookkeeping. durable replays a workflow
from the top on every attempt. If a run resolved "latest" inside its workflow
body and someone published a new version between attempts, the replay would
run against a different prompt and diverge, which durable detects as
`ErrNonDeterministic`. Every attempt verifies the pin instead: it reloads the
pinned versions and recomputes both hashes, and any mismatch, missing row or
empty digest fails the run terminally. A newer prompt never runs under an old
pin.

Two consequences worth knowing:

- Retiring an agent stops new runs, not old ones. Every version stays readable,
  because a run pinned to one must remain replayable and explicable.
- Canary and A/B become subscription concerns. Watching one version is a filter
  on `version`, not a deploy.

## One Runtime seam

A turn executes through one interface, `turn.Runtime`:

```go
type Runtime interface {
    Turn(ctx context.Context, def Pinned, in Input, emit func(context.Context, event.Body) error) (Result, error)
}
```

It runs one turn of a pinned definition for the event in `Input.Trigger` and
emits catalog bodies. The host stamps run identity (channel, sequence, tenant,
version) on each body; the Runtime never sees it. `flow/turn/native` is the
default and wraps saige. `flow/turn/sandbox` wraps any Runtime and runs each
turn as a sandbox task under the pinned policy, so isolation does not depend
on which Runtime is inside. Pass your own with `flow.WithTurnRuntime`.

## Durability

durable owns durability; sandbox does not. sandbox's queue is at-most-once in
its current beta, so a node that dies mid-task drops it. That is acceptable here
only because the durable run wrapping the task is durable's, and durable
replays it.

Each run event is published twice, to two systems with different jobs:

- `durable.Emit` writes the durable per-run journal, which is what replay and
  cursor resume are built on.
- `bus.Publish` writes the live cross-run bus, which is what observers and
  delivery read.

The journal is truth; the bus is flow.

The event id is the run's idempotency key. Every surface redelivers on
timeout, so a Slack retry or a GitHub redelivery keeps its id and joins the run
that already exists rather than starting a second one and answering twice.

## Stopping a run

A person who presses stop wants two things: the agent to stop working, and to
be able to reply to what it had said so far. A run can be asked to stop at any
point, and a stopped run is its own outcome: not a failure, not a completion.

```
Runtime.Stop
  -> the run ledger                the request is recorded on the run (truth)
  -> control.stop.v1               then published on the run's stop channel (fast path)
     -> the executing attempt      reads the ledger before its turn, listens on the bus
                                   during it, and polls the ledger as a backstop
        -> saige stream.Stop       the model call and running tools are cancelled
           -> run.stopped.v1       terminal: the reason, who asked, the partial answer
```

### The guarantee

From the moment `Stop` returns nil for a run that exists, belongs to the
caller's tenant and has not ended:

- no attempt of the run starts its turn, so no new turn calls a model;
- a turn already running is asked to wrap up at once over the bus, or within
  the stop poll interval (`runtime.WithStopPoll`, two seconds by default) if
  the bus lost the request;
- the run ends with `run.stopped.v1` and is never retried, unless its turn
  finished its answer before the request took effect, in which case it ends
  completed.

"Stopped" means cut short. A turn that returned its whole answer completed,
even if a request was on its way, and the agent loop is the judge: saige
reports a turn it wrapped up, and does not report one that finished first.
Exactly one outcome is ever recorded, and the journal entry, the terminal
event, the ledger and `Result` are all built from it, so they always agree. A
host that wants to know whether it asked, independent of how the run ended,
reads `Runtime.StopRequested`, which reports the recorded request for a run of
its own tenant.

### Where the request lives

**The ledger is the truth.** `Stop` writes the request on the run as a
write-once event record (`event:fiber.stop#0`) in the durable ledger the
run already lives in: per run, scoped by the run's tenant, read in one round
trip when an attempt starts, and on Postgres the same tables as every other
record (`flow/durable`), so there is no new schema. It records only for a run
that exists in that ledger, is the caller's tenant's and has not ended, so the
ledger holds at most one stop record per run, and it lives exactly as long as
the run's other records. A request for anything else writes nothing and
publishes nothing. When a request was recorded before an attempt began, that
attempt's workflow body claims it, so durable does not report it as a record
no call read; one recorded during an attempt is not in that attempt's replay
set and needs no claim.

**The bus is the fast path.** After recording, `Stop` publishes
`control.stop.v1` on `event.ControlChannel(tenant, "stop", run)`:
`control://<tenant>/stop/<hash>`, where the hash covers the caller's tenant
and the run id. It does not go on the run's own events, because a run has one
writer: the attempt chooses its own sequence numbers and the bus keeps the
first write of a sequence. Egress never delivers a control channel, and a host
that lets callers publish must refuse the `control` scheme, so the only way to
write a request is `Runtime.Stop`, called with the tenant and asker the host
took from the caller's credential. Because the tenant is part of the channel,
one tenant's requests can never land in the channel another tenant's run
reads, so they cannot push that tenant's own request out of it.

| The request arrives | What happens |
|---|---|
| Before a worker has started the turn | The attempt reads it from the ledger and ends the turn without calling the turn runtime. |
| While the turn runs | It is delivered over the bus at once, or read from the ledger at the next poll. |
| Between two attempts | The next attempt reads it from the ledger before its turn. |
| After the process restarts | The ledger survived, and the next attempt stops. |
| After the run ended | Nothing is published. Nothing is recorded either, unless the run ends between `Stop`'s check and its write, in which case the request is recorded on the finished run, where no attempt reads it and `StopRequested` reports it. |
| While the run is parked on a person | Recorded, and the question it waits on is resolved as stopped. The run is woken and ends `run.stopped.v1` without resuming its turn: see [Parking a run on a person](#parking-a-run-on-a-person). |
| A second time | The first recorded request is the one the run honours. |

**Tenant isolation does not rest on secret ids.** Run ids are derived from the
tenant, the event id and the agent by an exported function, and event ids are
often predictable (a schedule's occurrence, a Slack event id), so a run id is not a
secret. Isolation comes from the checks: `Stop` records and publishes only for
the caller's own run, the stop channel is keyed by the caller's tenant, and an
attempt ignores any request not made under its run's tenant. `Stop` answers
the same nil (and a host's route the same 202) for an unknown run, a finished run
and another tenant's run, so its answer never says whether a run exists.

Its timing can. For an id the ledger holds, `Stop` reads the run and checks
its tenant before deciding to do nothing; for an unknown id it returns after
one missed read. Someone who can time the call closely can tell whether a run
with that id exists in some tenant, though not whose it is or what it holds.
Because run ids are computable, that can reveal whether a known event (a
schedule's occurrence, say) started a run in another tenant. Closing it needs
a ledger lookup keyed by tenant, which durable's ledger does not have.

**The decision is a recorded result.** durable replays the workflow body, and
whether somebody has asked for a stop changes between attempts, so the body
never asks. The memoized agent activity reads the ledger and listens, and
returns a stopped outcome instead of an error. The ledger records the run as
ended, and a replay reads the outcome and announces the same stop.

### What the thread and the surfaces show

A stopped run journals its exchange like a completed one: the request as the
agent renders it, then an assistant entry holding the partial answer followed
by a fixed marker line, or the marker alone when the turn was stopped before it
said anything.

```
The deploy failed because

[This turn was stopped before it finished.]
```

The marker is a constant and the reason is not written there, because the
journal is replayed byte for byte to every later run in the thread. The reason
and who asked travel on `run.stopped.v1`, on the run's output, and on
`Runtime.Status` (`stopped`, with `StopReason` and `StoppedBy`). A request
with no reason is given saige's default, "Stopped on request", once, so every
path reports the same words. Sinks render a stopped run as stopped: the shared
layout says so in the header and under the partial answer, and a GitHub check
concludes `cancelled`.

**Delegated turns are cancelled.** Stopping a parent cancels the tool calls its
turn has in flight. A delegated sub-task is one of them, and it is aborted
rather than left to finish. It is not wrapped up the way the parent is: its
partial answer is discarded with the tool call it was the result of. A host
that carries out delegations itself (`runtime.WithDelegator`) sees the context
it was called with end with a `*runtime.StoppedError` cause (matching
`runtime.ErrStopped`, with the reason and asker) when the parent was stopped,
and with the ordinary cause otherwise, so it can stop the run it started with
the same words.

**Stop requests are not agent output.** A control channel matches only a
subscription that names it exactly, so a consumer of every channel prefix (a
dashboard, a host relaying every event) never receives a `control.stop.v1`.
An audit of one run's requests subscribes to its control channel by name.

### How long the bus keeps a request

This only decides whether the fast path delivers a request; with the ledger it
never decides whether a run stops. A host that builds a runtime without
`runtime.WithStopLedger` has only the bus, and these are its exact bounds:

- A run's stop channel keeps the newest `bus.WithRetention` requests (1024).
  Only the run's own tenant writes there, so depth cannot lose a stop; it only
  decides which request's reason survives.
- The in-memory bus keeps the `bus.WithMaxRuns` rings written most recently
  (4096), run rings and channel rings alike. A pending request is lost once
  that many other rings have been written after it, the run's own among them.
- Postgres forgets an event `bus.WithIndexTTL` after it was published
  (24 hours by default), control requests included.

The control state the bus holds is bounded by the runs that were asked: a stop
channel exists only for a live run of the asker's tenant that was asked to
stop, at most one per (tenant, run), each at most `WithRetention` entries, and
it ages out with every other event.

## Compacting a run on request

A host can ask a running turn to summarize the older part of its history now,
instead of waiting for the deployment's budget (`Compaction.MaxInputTokens`). The
deployment has to take requests (`native.Compaction.OnRequest` on its agents
and `runtime.WithCompactRequests` on the runtime, both set by
`flow.WithCompaction` with `OnRequest`). A turn of a runtime that takes them holds a
second control subscription, beside the stop one, for as long as it runs.

```
Runtime.Compact
  -> control.compact.v1              published on the run's compact channel
     -> the executing attempt       listens on the bus for as long as its turn runs
        -> the next model call      is preceded by the compaction the budget would make
           -> run.context.compacted.v1  trigger "requested", who asked, tokens before and after
```

The compaction is saige's, with the strategy, the cut and the anti-thrash
rule a budget uses; a request is never held back by that rule. Requests made
before the next model call fold into one. A request is live only: it is not
recorded in the ledger, and one made while the run is queued, parked, between
attempts or over does nothing, because the turn it would compact is not the
one that runs. A turn with nothing older than `KeepTurns` turns has nothing to
summarize, and a request then does nothing either. `run.context.compacted.v1` is
published for every compaction, budgeted ones included, and is opt-in like
`run.usage`.

## Usage

Every run reports what it spent: tokens, the prompt cache's share, cost where
it can be known, and how full the context was. The figures are saige's;
flow forwards them, keys them, and adds them up without counting anything
twice.

### What is reported, and where

| Where | What |
|---|---|
| `run.usage.v1` | One model call, published the moment saige reports it: `accounting_id`, `saige_accounting_id` when saige gave one, the `agent` whose model made it, its delegation `depth`, `model`, `requests`, `prompt_tokens` (all input), `cached_prompt_tokens`, `cache_write_tokens`, `completion_tokens`, `cost`, `context_window` |
| `run.usage.child.v1` | A child run a host carried out for a delegation, once it ended: its run id, agent and whole total, read from the ledger |
| `run.completed.v1`, `run.stopped.v1` | The run's total as `usage` |
| `runtime.Output.Usage` | The same total, in the ledger with the rest of the output, so it survives a restart |
| `Runtime.Status` | `Run.Usage` (`RunUsage`): `own`, `children`, `total`, `context` |
| `runtime.RecordedUsage` | A failed run's total, from its ledger records |

The total is a `event.RunUsage`. `Own` is the run's own agent, `Children` is
everything it delegated at every depth, and `Total` is the two added: the
figure to hold against a limit. `Context` is the run's own agent at its latest
model call: the model, its context window, and how many tokens that call's
prompt was.

**Usage is opt-in.** `run.usage` is an opt-in family in the catalog: a
subscription receives it only with a type prefix that names it (`run.usage`,
or `run.usage.v1`), and a subscription to `run` never does. Receivers that knew
a fixed set of types would otherwise get an event per model call, and one they
refuse, or fail on and have retried in the run's ordered delivery lane, would
delay its `run.completed.v1`. "Every run event" does not cover accounting, as
no channel prefix covers a control channel.

Sinks that render a chat surface (Slack, GitHub) ignore usage even when
they name it: token counts and cost are for whoever pays, not for everyone in
a channel or on an issue. A sink of the host's own whose `Types` name
`run.usage` receives it. Only the attempt that spent the tokens writes them; a host that
lets callers publish must refuse the family.

### Cost and money

Cost comes from saige's price catalog and nowhere else. saige's adapters
resolve a model's rate card from the catalog (`ModelCapabilities.Pricing`,
with its `AsOf` date and currency), and flow prices each call with
`Pricing.Cost` over saige's billing tiers (`UsageFromDelta`). A model with no
rate card, a modality the card does not price, or a call whose provider
reported no tokens gets no cost: its tokens are counted, the call is counted in
`unpriced_calls`, and nothing is guessed. A call saige answered from its
response cache sent nothing to a provider and is reported with no tokens and
no cost.

Money is `event.Cost{Micros, Currency, AsOf}`: integer millionths of an ISO
currency, saige's `types.Cost`. One US dollar is 1,000,000 micros with currency
`USD`, the int64 micro-USD convention a host enforcing spend limits uses. Each
call's cost is rounded to the micro once, by saige, and a total is the integer
sum of its calls.

Money is never converted and never dropped. A total's `cost` is in the
currency of the first priced call it counted; calls priced in any other
currency are summed in `other_costs`, one entry per currency. saige's catalog
prices everything in USD, so `other_costs` is empty unless a deployment
supplies rate cards of its own. A host holding a USD limit reads `cost` when
its currency is `USD` and treats anything in `other_costs` as spend it cannot
compare. A total's `as_of` is the oldest date among its calls, and empty when
any was priced from a card with no date.

### Counted once, across retries and replays

durable retries a failed attempt and replays the workflow body from the top,
and a delivery can repeat any event. The rule:

- **Every model call any attempt made is counted, once.** Before the agent
  activity returns, failed or succeeded, it writes the calls it relayed to the
  run's ledger as a write-once event record,
  `event:fiber.usage.a<attempt>#0`. A failure is retried, and so is a
  success whose result durable never managed to record; either way the
  attempt that finally records an outcome reads the earlier attempts' records
  inside the agent activity, adds its own calls, and the total is memoized
  with the outcome. The workflow body claims the records the outcome counted,
  which it learns from the outcome, so a replay claims the same ones.
- **A replay counts nothing.** The agent activity is memoized, so a replay
  reads the recorded total and calls no model.
- **A stopped turn is counted.** saige settles the call a stop cut short and
  reports its usage when the provider had reported any, and the stopped
  outcome carries the total like a completed one.
- **Each report has a key.** `accounting_id` is always flow's,
  `<run>/a<attempt>[/<agent>.<sub>]/<n>`, which no other call of any attempt or
  any delegation can share. saige's own `AccountingID` is carried as
  `saige_accounting_id` and never counted by, because saige keeps it stable
  across a durable replay of its own. A child run is keyed `run:<child run
  id>`. `event.Tally` counts each key once, so a consumer summing the stream
  at least once gets the same total the run records.

The total is counted from the usage events the attempts relayed, never from a
figure a turn runtime returns. It equals the sum of the run's call events
whenever every attempt that called a model returned, which is every case but
two, both in Limitations: an attempt that dies without returning, and a
superseded attempt that records after the outcome was decided.

### Roll-up

What a run delegated is the run's spending too, and each call is counted in
exactly one run's `Own`.

- **In-turn sub-tasks** (the `tag_<agent>` tool without a host Delegator) are
  not runs. Their calls are relayed onto the parent run's stream as
  `run.usage.v1` one level deeper (`depth` 1), as they happen, and counted in the
  parent's `Children`, whether the sub-task answers, fails or is aborted. The
  relay carries any depth, but the sandbox gives a sub-task no spawn grant of
  its own, so in practice a sub-task's own delegation is refused by policy and
  in-turn delegation is one level deep.
- **Delegated runs** (a host's `runtime.WithDelegator`) are runs of their own
  with their own totals. The host rolls one up by calling
  `runtime.ReportChildUsage(ctx, childRunID)` with the context its Delegator
  was given. Only the run id comes from the host: the runtime reads the child
  from the ledger, refuses it with `runtime.ErrNotAChild` unless it is a run
  of the parent's tenant, not the parent, that has ended (another tenant's run
  is refused in the same words as one that does not exist), and takes its
  agent and total from the child's own record. The parent's stream carries a
  `run.usage.child.v1` and the parent counts it in `Children`, once per child run
  however many times it is reported or however many parent attempts delegated
  to it. A child's total already includes what it delegated, so three levels
  of runs roll up with each call counted once at the origin.
- **An event that wakes another agent** starts an unrelated run, which does not roll up.

## Parking a run on a person

A run can wait for a person: to approve a tool call, to choose among options,
or to reply in their own words. It parks while it waits: it holds no worker,
no goroutine and no lease, costs no retry budget, and survives any restart. A
host shows the question and answers it, and the run resumes with the answer.

### How a question is raised

| Question | Raised by | The answer |
|---|---|---|
| `approval` | A call to a tool the definition lists in `approval.ask`, or any tool the host's catalog wrapped with a saige marker (`types.MarkedTool`) | `approve` (optionally with edited arguments) or `deny` (optionally with a note the model is shown) |
| `choice` | The built-in `ask_person` tool with `options` | One of the options |
| `reply` | `ask_person` without `options` | Free text |
| `hold` | The host, through `Runtime.Hold`, not the agent (see [Holding a run for the host](#holding-a-run-for-the-host)) | `approve`, and the run goes on, or `deny`, and it is stopped |

The first three are saige's own vocabulary. An approval is saige's marker: saige stops
at the call and asks. `ask_person` is a tool the agent calls when its
definition grants it; its result is the answer. A definition may name the person or role it
wants an answer from (`asked_of`, a hint for routing, never an
authorization) and, for `ask_person`, a `default`.

The question is typed (`event.Question`): its id, kind, prompt, options,
for an approval the tool, the call id, the arguments and the tool's schema,
`asked_of`, when it expires and what it defaults to. It travels on
`run.parked.v1` (`Parked.Question`), and `Runtime.Status` reports a parked
run as `parked` with `Run.Question`, read from the run's records
(`Runtime.PendingQuestion`).

### Holding a run for the host

A host can ask that a run wait for a person without the agent asking
anything: a run that reached a spending limit, say. `Runtime.Hold` records a
`turn.Hold` (the host's id, a topic, a prompt, optional approvers, opaque
data and a wait) on the run in the ledger as the write-once record
`event:fiber.hold.<id>#0`, once it has checked that the run is the caller's
tenant's and has not ended (`ErrNotHeld` otherwise). Nothing is published:
the turn reads the run's holds from the ledger (`turn.WithHolds`) every time
the model sends a message with tool calls, so a hold made while the run is
queued, parked, between attempts or mid-turn is found at its next batch.

That batch parks on a question of kind `hold` carrying the hold
(`Question.Hold`), and nothing in it runs. From there it is a question like
any other: announced once, answered through `Answer`, expired at its deadline
and stopped by `Stop`. An approval continues the turn, which runs the batch,
and the hold is not asked again. A denial, or nobody answering in time (a
hold's default is a denial), ends the run `stopped` with the answer's note as
the reason, and the batch never runs. Who may answer is the hold's
`approvers` when it names any, else whoever the host lets answer. A turn that
answers without another tool call is never held: its last model call was
already made.

### Ending the turn at the question

The model's message can ask for several tool calls at once. When one of them
needs a person and has no answer, none of them runs: an unmarked call next to
the question would otherwise take effect before the person decided, and again
when the batch is continued. The turn ends at that message
(`native.Result.Parked`) with the question, the message itself, and the
conversation up to it exactly as saige holds it. What the turn had said so
far is published and kept.

### Durable and deterministic

```
agent activity (memoized)        returns the parked turn
  -> step fiber.question.<id>   announces run.parked.v1 once; the record is the question
  -> event fiber.answer.<id>    the resolution: absent, the body returns ErrParked and the run waits
Runtime.Answer                   checks the answer, writes the resolution (first write wins), wakes the run
  -> next attempt replays         the turn and the question from records, reads the resolution
  -> fiber.turn.resume          continues the turn with the answer (memoized)
```

The resolution is a write-once durable event record, and it is the only
thing the workflow body decides from. An answer writes it, a stop writes it,
an expiry writes it; each reads back whichever was first. That is what makes
an answer exactly-once under concurrent answers (every caller is told the
recorded answer; one is told it was theirs), and a replay decide the same way
whatever has been asked since. A replay announces nothing it announced before:
an attempt opens (`run.started.v1` or `resumed`) only when it has something
new to publish, so a parked run polled by the worker's janitor with no answer
publishes nothing at all. An attempt runs at most one turn: the attempt that
asked parks even when an answer lands a moment later, and the answer's
wake-up starts the next attempt, so each attempt's sequence band, usage
record and accounting keys stay its own.

`Runtime.Answer` is scoped to the
caller's tenant: a question of another tenant's run is reported exactly like
one that does not exist. Who answered comes from the host, taken from the
caller's credential, as for an event or a stop (see "Who may answer" below). An answer is checked against its question before it is recorded:
a decision is `approve` or `deny` and only an approval carries arguments,
which must satisfy the tool's schema (saige's validator); a choice must be one
of the options; a reply must have text. Answer wakes the run through the queue
(`runtime.WithWaker`); without it, or if that enqueue is lost, the janitor's
poll wakes it.

A tool can decide per call whether it needs approval by implementing
`native.Approving`: `Approval(call) (ApprovalRequest, bool)` sees the call's
name and arguments and returns the prompt, the `asked_of` hint, and
optionally the approvers and self-approval for that one call. A delegation
tool uses it to apply the callee's policy, asking only for the callees that
require it. saige is told through a tool gate, so the call stops and asks
exactly as a marked tool does: it runs only once approved, a denial or an
expiry reaches the model as a refusal, and a turn that cannot park refuses
it. A tool also listed in `approval.ask` is asked about once, with the per-call
question when there is one.

### Who may answer

Being in the tenant is not enough to approve a call. The rules are decided
from the run's pinned definition and policy, so a new version of an agent
does not change who may answer a question its run already asked:

| Question | Who may answer |
|---|---|
| `approval` | Anyone in the tenant except the requester, unless the policy sets `self_approval`. With `approvers` set, only those people: principals as the host names them (`ana`, `slack:U1`) or `role:<name>` entries, which the host resolves (`runtime.WithRoles`; without it a role admits nobody). |
| `approval` asked per call | When the tool that asked names approvers of its own on the question (`approvers`, `self_approval`), those replace the policy's: only they may answer, named as above, and the requester only with the question's `self_approval`. A question without them follows the row above. |
| `choice`, `reply` | Anyone in the tenant, the requester included. With `answerers` set, only those people, named as approvers are. |

The requester is the actor on the event that started the run, under its id
and its id qualified by its surface (`ana`, `github:ana`). The answerer is the
credential's subject, or, for a credential marked `ActAs` (a surface that
authenticated the person itself), the person it names. `ActAs` is trusted to
say who is answering and is checked like anyone else: a Slack surface may
approve on behalf of a colleague of the requester, never on behalf of the
requester. A credential is also never allowed to approve a call in a run it
raised as itself, whoever it names, because it could otherwise name anyone
(`AnswerRequest.Via` carries the credential's own subject for this check).
A refusal is `runtime.ErrNotAllowed`, `PERMISSION_DENIED` on the wire, and
records nothing. A question's `asked_of` is a routing hint and admits nobody.

A call that runs with the credential of the person the run acts for, a tool
of a person-scoped provider (`native.PersonScoped`), says so on its question
(`tool.acts_for_person`). Anyone who may approve it may approve or deny it as
asked, but only that person may approve it with edited arguments: the
actor's `user` when the event names one, and otherwise the requester. Anyone
else's edit would act as them, with their credential, on something they did
not ask for, so it is refused with `runtime.ErrNotAllowed` and records
nothing. A host shows such a question to anyone else without its edit.

### The conversation across the park

The resumed turn has the whole context because it is the same conversation.
The continuation rebuilds what the parked turn sent from what is pinned and
recorded: the thread's history up to the pinned sequence, the request exactly
as it was sent (retrieved passages included, never retrieved again), then the
messages the turn had added, then the parked message. Its first model call is
not sent: it is answered with the recorded message, so saige executes that
batch itself, with the answer, through its normal tool path (markers, hooks,
the schema check of edited arguments), and the model's next request is byte
for byte what an uninterrupted turn would have sent. The provider's prompt
cache therefore reads the whole conversation across the park. The recorded
call's deltas and usage are not published again. A turn that compacted before
it parked records its whole conversation instead of the part after the
request.

A batch with two questions parks twice, once per question; the second
continuation carries both answers, and the batch runs once, after both.

A parked turn's record stores the transcript it continues from. A run that
parks again stores only what its turn added since the previous park
(`native.Parked.Since`), and the workflow body rebuilds the whole from the
recorded parks before it continues, so a long conversation with many
questions is stored once. Each park still stores the message it waits on, and
a turn that compacted stores its whole conversation.

### Expiry and defaults

Every question has a deadline (`runtime.WithQuestionTTL`, 24 hours by
default). Once it has passed, the next attempt to find the question
unanswered records its default as the resolution, and the run continues with
it:

| Question | Default |
|---|---|
| `approval` | Denied, with "nobody answered before the question expired"; the tool does not run |
| `choice`, `reply` with a `default` | That default |
| `choice`, `reply` without one | No answer: the tool tells the model nobody answered, and it carries on |

An answer that arrives after the deadline records the default too, and is told
so (`expired`). A question is never expired in the attempt that asked it, so
a person always sees it first. The clock only decides whether to write the
default; what the run does is decided by the record.

### Stop, usage, and delegated runs

- **Stop ends a parked run without resuming it.** `Stop` records the request,
  then resolves the pending question as stopped and wakes the run, which ends
  `run.stopped.v1` with what it said before it asked and what it had spent.
  Its turn is not continued, no model is called, and the question then
  refuses answers (`ErrQuestionClosed`, `FAILED_PRECONDITION`). A stop
  recorded while the turn ran, before it parked, is found the same way.
- **Usage before the park is counted once.** The parked turn's calls are
  recorded on the run like any attempt's; the continuation counts them with
  its own, so the run's total is every call once, as usage counts across
  retries.
- **A delegation can park the parent on its child.** With
  `runtime.WithChildRuns`, a durable run's delegation (`tag_<agent>`) is a
  run of its own that the parent parks on: the turn ends at the call, the
  workflow body starts the child as a recorded step (its event id derived from
  the parent and the wait, so a retried start joins it; the parent's tenant,
  requester and files, no thread), and publishes `run.parked.v1` with a
  question of kind `delegation` naming the child run. The parent holds no
  worker while the child runs or waits on a person. When the child ends, it
  writes the parent's resolution (its answer, or why it has none) and wakes
  the parent, whose continuation runs the call with that answer and rolls
  the child's usage up as `run.usage.child.v1`. A child whose retries run out ends
  without writing it; the parent's janitor poll reads the child's record
  instead. Stopping the parent stops the child with the same words. A
  delegation question is not answered by a person: `Answer` refuses it.
- **A host Delegator holds a worker.** A Delegator (`runtime.WithDelegator`)
  that starts a run for a delegation and blocks until it ends keeps the
  parent's turn, and its worker, waiting. With N workers, N parents each
  waiting in a Delegator on a child hold every worker; the children, and a
  child resuming after its question is answered, never get one, and the
  parents' heartbeats keep their leases alive, so nothing recovers (a parked
  child stretches this to its question's deadline and beyond). Use
  `WithChildRuns` instead; it takes precedence over a Delegator. A Delegator
  that does wait can show the child's questions on the parent's stream with
  `runtime.WatchChildQuestions(ctx, childRunID)` (`run.child.parked.v1`,
  read once a second from the child's run row and its question records).

### What surfaces show

Sinks render a parked run as "waiting on a person", with the question, the
call an approval would run, the options, and who it is asked of, in plain
words: a waiting run is not working and must not look as if it were (nothing
animates, and calls in the parked batch read "waiting", not "running"). Once
it resumes, they show how it was answered ("answered: approved by ana", or
"nobody answered in time"). Answering happens on the host; a chat surface
shows the question and does not take the answer.

## Package DAG

Dependencies run one way. Leaves import only the standard library and know
nothing about transports, storage, or agents.

```
leaves
  flow/address        github://urmzd/fiber/issues/42, slack://T01/C02?thread=...
  flow/attachment     the typed reference to a file an agent was handed
  flow/event          the envelope, the sealed Body union, Unknown and the catalog
  flow/event/legacy   reads JSON written before the union, maps old kinds to types

seams
  flow/blob           content-addressed file bytes; flow/attachment/intake and resolve
  flow/bus            channel bus: Memory and Postgres, bustest
  flow/connector      Trigger / Sink / Subscriber / Actor faces, registry of sinks
  flow/definition     saige definitions, Policy, insert-only Source, Resolver and pins
  flow/session        an agent's continuing conversation; flow/thread, its journal
  flow/isolation      the seam to an isolated instance; isolate (on sandbox/isolate)
  flow/turn           the Runtime interface, Pinned, Input and Result

composition
  flow/turn/native    the default Runtime, on saige's agent loop; tools, citations, usage, parking
  flow/turn/sandbox   any Runtime inside sandbox/, under the pinned policy
  flow/runtime        the durable turn: accept, pin, stop, usage, parking
```

A host composes these in Go: `runtime.New(durable.New(ledger, queue),
resolver, bus, opts...)` with the `turn.Runtime` it chose, and every other
backend as a constructed value. Nothing reads a configuration file or a
`FIBER_*` variable (model providers still read their vendor's key, such as
`ANTHROPIC_API_KEY`, when an agent is built). Each backend family has a
conformance suite (`bustest`, `definitiontest`, `threadtest`, `blobtest`)
every implementation passes. Only `flow/turn/native` and `flow/definition`
reach saige's agent loop and providers; the rest import its types alone, and
`flow/turn/imports_test.go` keeps it that way.

## No transport

flow serves nothing. A program composing flow publishes on the bus, calls
`Runtime.Status`, `Stop`, `Compact` and `Answer` in process, subscribes with
`Bus.Subscribe`, and mounts each webhook connector's handler wherever it
serves HTTP; each one verifies its own signature. Authentication, and
stamping the tenant and actor on what it accepts, belong to that program.

## Security boundaries

Two, and changes near either require tests proving confinement still holds.

**`flow/signature`** (arriving with the connectors) is the only thing between the public internet and starting a
run. It verifies HMAC over the raw request body before that body is parsed.
Rules that follow: verify before parsing, hash the exact bytes received, compare
with `hmac.Equal` in constant time, and fail closed on every ambiguity including
a missing, empty, or duplicated header.

Slack signs a timestamp inside its base string, so a replay window is
enforceable and is enforced. GitHub does not, so a captured GitHub request stays
valid as long as the secret does. That defense lives downstream in event id
idempotency instead, and the package documents this rather than implying a
protection it cannot provide.

**The NGAC policy path** is sandbox's. An agent's sub-agents and its Policy's
workspace areas compile into a policy graph enforced by the sandbox with
default deny, covering both workspace areas and the spawn allowlist that
governs one agent delegating to another.

## Limitations

Verified against the code, and honest.

- **The Postgres bus serializes publishes per tenant.** flow's own
  `bus.Postgres` takes one advisory lock per tenant per publish, so a
  tenant's position (`tseq`) is commit order and readers never skip a late
  commit. Throughput is one short transaction per event for each tenant;
  different tenants do not wait on each other.
  The run queue flow builds on Postgres has no `DeltaLog`, so durable's
  per-run delta journal is not written; the bus carries every event.
- **sandbox's queue is at-most-once** in beta. Durability comes from durable.
- **Citations come from retrieval and from tools, not from the model.**
  Retrieved passages are extracted from RAG `AssembledContext` blocks and
  emitted as `run.delta.citation.v1` events before the agent starts. Sources a
  tool attributes its result to (saige's `CitationDelta`) follow, labelled
  with the citing agent's own markers; one that repeats a citation already
  published under the same marker and source is dropped. Citations a model
  produces itself (provider-native search) are not translated.
- **durable v1 workflow bodies are single-goroutine.** Parallelism belongs
  inside a sandbox task, not in a forked workflow.
- **The bus is at-least-once.** Subjects, filters, and cursors cover
  real-time consumers. A consumer that must act once per deployment reads
  through a consumer group (`Bus.Consume`): a lease and a committed cursor per
  (group, tenant), so one member at a time reads a tenant's events and the
  next resumes from the cursor. After a crash the events since the last commit
  are delivered again, and nothing older than the bus's retention is.
- **Definitions are saige's.** flow refuses the definition fields it cannot
  honor (dials, skills, memory, guardrails, compaction, harness tools, MCP allow
  lists) rather than ignoring them. See [flow-events.md](flow-events.md#7-definitions).
- **A stop is reliable only with the ledger.** `runtime.WithStopLedger` must
  be given the ledger the runs execute from (`runtime.WithStopLedger`). A host without it
  falls back to the bus alone, with the bounds in "How long the bus keeps a
  request". With an in-memory ledger, only the process that accepted a run can
  stop it, which is also the only process that can read it with `Runtime.Status`.
- **A crash between the wrap-up and its record loses the partial text from the
  thread.** If a worker dies after saige stopped the turn but before the
  stopped outcome was recorded, the next attempt finds the request in the
  ledger and stops before calling the model, with no text. The thread then
  holds only the marker, while the surface already showed what the first
  attempt streamed.
- **A turn holds one bus subscription while it runs**, so a process holds as
  many as the runs it executes at once. One shared subscription per process
  would remove this cost; it needs a way to subscribe to a prefix
  starting from now, which the bus does not have.
- **Stopping reaches a turn through the in-process sandbox.** The stop signal
  and the abort of an abandoned task cross from the waiting goroutine to the
  sandbox tool in memory. A remote sandbox node would need them carried over
  its own transport; only the in-process plane is supported and tested.
- **A request to compact is live only.** It is not
  recorded, so it reaches only the attempt running when it is made, judged by
  the asking process's clock against when that attempt began to listen; a
  request within a clock skew of an attempt starting can be dropped. The
  summary is model output, so a retried turn does not reproduce it.
- **Tool arguments are published.** The executing `run.delta.tool.call.v1` and
  `run.delta.tool.done.v1` carry the call's arguments as compact JSON, up to
  `event.MaxToolArguments` (8 KiB); larger arguments are left out whole and
  sized in `arguments_bytes`. They go to every subscriber, chat
  sinks render a short prefix of them, and only `native.WithToolArguments`
  scrubs them first. flow emits no tracing spans of its own.
- **A stop wraps up the turn, not its side effects.** Tool calls already
  finished stay done, and a tool that ignores its context delays the stop until
  the grace period (ten seconds by default) cancels the turn. A turn that is
  cancelled this way ends stopped with the text it had published.
- **A subscription that selects only `run.completed` is not told about a stop.**
  Select `run`, or add `run.stopped`, to hear every ending.
- **Earlier attempts' usage needs the ledger.** An attempt's calls are
  recorded on the run through `runtime.WithStopLedger`. Without it, the total
  counts the attempt that produced the outcome, and the `run.usage.v1` events
  still report every call. If the record cannot be written, it is logged, and
  if that attempt is executed again its calls are left out of the total.
- **A crash mid-turn loses that attempt's calls from the total.** An attempt
  that dies without returning (the process is killed, the machine goes away)
  writes no usage record. Its calls were published as they happened and are on
  the bus, but the run's total does not count them, so the total is then lower
  than its call events.
- **A superseded attempt may be counted late or not at all.** An attempt that
  lost its lease and keeps running until it notices may write its record after
  the attempt that took over has read the records and fixed the total. Its
  calls are then on the bus and in the ledger, and not in the total.
- **Rolling up a delegated run needs the ledger and the Sandbox.**
  `ReportChildUsage` reads the child from the run ledger, so a runtime without
  `WithStopLedger`, or a turn runtime other than `Sandbox`, refuses every report.
- **A call a turn never finishes may not be counted.** saige reports a call's
  usage when the call completes, or when a stop wraps it up. A call cancelled
  outright (a worker shutting down, a stop whose grace period ran out, a
  sub-task aborted with its parent) reports what its provider had sent, which
  may be nothing.
- **Cost is only as good as saige's catalog.** Rates are recorded on a date
  (`as_of`) and do not know about contract pricing, regions or batch calls, so
  a cost is an estimate of the list price, not an invoice. There is no
  conversion between currencies.
- **The context window is saige's declaration.** It is the model's input
  limit from the catalog, 0 when the catalog does not declare one, and the
  prompt size is the provider's count for the latest call. Nothing estimates a
  prompt before it is sent.
- **Roll-up of delegated runs is the host's call.** flow cannot see a run
  a Delegator started until the host reports it with
  `runtime.ReportChildUsage`, and the host must report it after it ended.
- **Parking needs the ledger.** A question and its answer are records on the
  run, so a runtime without `WithStopLedger` cannot park: its turns refuse
  approvals and tell the model that `ask_person` cannot be answered.
- **Polls read records, not transcripts, only on Postgres.** The stop poll
  (every two seconds while a turn runs), `Answer`, `PendingQuestion` and the
  child-question watch read the run's row and the records they need through
  `runtime.WithRecordReader` (on Postgres, `pgledger` reads records by
  prefix itself). Without it each of those reads loads every record of the run,
  transcripts included.
- **Expiry waits for a poll.** Nothing wakes a run at its question's
  deadline. The worker's janitor polls parked runs (every 30 seconds by
  default, and only runs older than one lease), so a default applies up to
  that long after the deadline, or at once if someone answers late.
- **An answer that lands while its run is parking waits for a wake-up.**
  `Answer` enqueues the run three times over about a second, because an
  enqueue of a run a worker still holds is dropped. If all three are lost the
  janitor's poll resumes it.
- **A blocking Delegator can exhaust the workers.** See "A host Delegator
  holds a worker" above; `WithChildRuns` does not.
- **A child whose retries run out wakes its parent late.** Its failure is
  read from the ledger at the parent's next janitor poll.
- **An in-turn sub-task cannot park.** A `tag_<agent>` call without a host
  Delegator is part of its parent's turn, not a run: approvals inside it are
  refused and `ask_person` tells it nobody can be asked.
- **A parked turn's record holds its conversation.** The continuation needs
  the messages the turn added, with any media a tool returned inline, so they
  are in the agent activity's record: once per run when it parks again, but
  the whole conversation, history included, for every park of a turn that
  compacted. Records are kept as long as the run's other records.
- **A question's tool arguments are re-read from JSON.** On the continuation
  the parked call's arguments come back from the record with numbers as
  `json.Number`, as saige's durable engine reads them; a tool that type-asserts
  `float64` sees the difference.
- **A stop cannot be made for a run before it is accepted.** Nothing is
  recorded or published for a run the ledger does not hold yet; ask again once
  `Accept` has returned the run id.
- **Per-person tools move the cached prefix between people.** A person-scoped
  group is expanded per person, so two people in one thread can be offered
  different tools and a different note, and the prompt cache of that thread
  misses when the person changes. For one person it is stable.

## Host-constructed invocation input

A turn's `turn.Input` can carry an explicit `History` of user and assistant
messages and trusted host-generated `Instructions`. The native Runtime preserves
their roles and order, appends the rendered trigger, and supplements the pinned
system prompt with the instruction. Neither value changes the pinned
definition or grants access. Hosts must freeze resolved input before durable acceptance and must not
use arbitrary caller text as privileged instructions.

`runtime.WithCallTool` lets a host replace the per-target `tag_<agent>` tools
with one catalog tool for agents with sub-agents. The host tool must check the
caller's pinned spawn grant. Without that option, flow retains its existing per-target tools.
The runtime still owns execution, streaming and sandboxing; the host owns its
input references, output contracts and answer delivery policy.

### Trigger context and private host data

An ingress body's `Request.Context` reaches the model as "Context from the
trigger", one line per key in sorted order, ahead of the request, as part of
the body's catalog rendering. A key that starts with `_` is never rendered: it
is the host's own, such as which knowledge sources ground the run, and is never
written into anything a model is sent (not the turn's request, not the request
a stopped turn records, and so not the thread journal later turns replay). A
context whose keys are all private sends no trigger context. Data a model and
an egress adapter must never see at all, such as a Slack bot user id or a
GitHub installation id, belongs in `Envelope.Private`, which is never
serialized.
