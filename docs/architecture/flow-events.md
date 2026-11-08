# Events, channels, definitions and the Runtime

> **Implemented.** This document specifies the event union, the channel bus,
> definitions and the `Runtime` seam as built. [flow.md](flow.md)
> covers how a run behaves on top of them. Moving data and hosts from the
> shapes before the union is in [Upgrade](#10-upgrade).

The design turns on four decisions:

1. Every event is one envelope with a typed body. `type` names the body.
2. One bus carries every event. Subscribers listen by channel and type prefix.
3. Agents are saige definitions plus a flow Policy, stored insert only and pinned per run.
4. A run executes through a small `Runtime` interface. saige is the default.

A run is started by an event: a chat message is `message.posted.v1` with a
`text` field, and the model reads the catalog rendering of that event.

## 1. Envelope

Every event, in every direction, is one envelope. The fields follow
[CloudEvents](https://cloudevents.io) naming.

| Field | Type | Meaning |
|-------|------|---------|
| `id` | string | Unique per source. Redelivery of the same thing keeps the same id. |
| `source` | URI | What produced the event: `slack://T01`, `github://acme`, `fiber://run/<run>`. |
| `type` | string | The only discriminator. Carries the version. See [Types](#2-types). |
| `subject` | string | The resource inside the source, such as `issues/42`. Optional. |
| `channel` | URI | Where the event is published. See [Channels](#5-channels). |
| `time` | RFC 3339 | When the source says the event happened. |
| `hops` | int | How many events led to this one. See [Rules](#6-rules). |
| `data` | JSON | The body for `type`, as inline JSON. Never base64. |

Run-scoped events (every `run.*` and `control.*` type) also carry:

| Field | Meaning |
|-------|---------|
| `tenant` | The tenant the run belongs to. Set by the host from the principal, never read from the wire. |
| `run` | The run id. |
| `seq` | The position in the run, banded as today (see [Rules](#6-rules)). |
| `agent` | The definition name that produced the event. |
| `version` | The pinned definition version. |
| `digest` | The pinned definition digest. |

Private host data, such as a Slack bot user id or a GitHub installation id,
goes in `Envelope.Private`. It is tagged `json:"-"`: it is never rendered to
the model, never stored on the bus as part of `data`, and never sent to an
egress adapter.

```json
{
  "id": "Ev01J9",
  "source": "slack://T01",
  "type": "message.posted.v1",
  "subject": "C02/1699.001",
  "channel": "slack://T01/C02",
  "time": "2026-10-28T14:03:11Z",
  "hops": 0,
  "data": {"text": "summarise this thread", "actor": {"id": "U07", "name": "ada"}}
}
```

## 2. Types

`type` is the only discriminator, and it carries the version. The grammar is:

```
type    = family *( "." name ) ".v" N
family  = name
name    = [a-z0-9_]+
N       = [1-9][0-9]*
```

A breaking change to a body is a new type (`.v2`), registered next to the old
one. Nothing else in the envelope says what `data` holds.

### Run events

| Type | Body | Notes |
|------|------|-------|
| `run.accepted.v1` | `Accepted` | |
| `run.started.v1` | `Started` | |
| `run.parked.v1` | `Parked` | Carries the question |
| `run.resumed.v1` | `Resumed` | |
| `run.completed.v1` | `Completed` | Terminal, carries the run's usage |
| `run.failed.v1` | `Failed` | Terminal, carries the error |
| `run.stopped.v1` | `Stopped` | Terminal, carries the reason, the asker and the partial answer |
| `run.child.parked.v1` | `ChildParked` | |
| `run.delta.text.v1` | `Text` | |
| `run.delta.thinking.v1` | `Thinking` | |
| `run.delta.tool.call.v1` | `ToolCall` | |
| `run.delta.tool.done.v1` | `ToolDone` | |
| `run.delta.citation.v1` | `Citation` | |
| `run.action.taken.v1` | `Action` | |
| `run.usage.v1` | `UsageCall` | Opt-in |
| `run.usage.child.v1` | `UsageChild` | Opt-in |
| `run.context.compacted.v1` | `Compaction` | Opt-in |
| `control.stop.v1` | `StopRequest` | Control channel only |
| `control.compact.v1` | `CompactRequest` | Control channel only |

Each run event has its own body, so `run.failed.v1` carries an error and
`run.parked.v1` carries the question, and neither carries fields that belong
to the other.

### Ingress types

| Type | Raised by |
|------|-----------|
| `message.posted.v1` | A Slack mention or direct message, a console message, any surface where a person writes to an agent |
| `slack.command.invoked.v1` | A Slack slash command |
| `slack.session.stopped.v1` | A person pressing stop on a Slack agent session |
| `github.issue_comment.created.v1` | A comment on an issue or pull request |
| `github.pull_request_review_comment.created.v1` | A review comment on a diff line |
| `github.pull_request_review.submitted.v1` | A submitted review |
| `github.issues.opened.v1` | A new issue |
| `github.issues.labeled.v1` | A label added to an issue |
| `github.pull_request.opened.v1` | A new pull request |
| `github.pull_request.ready_for_review.v1` | A draft marked ready |
| `github.pull_request.review_requested.v1` | A review requested from an agent's login |
| `github.pull_request.synchronize.v1` | New commits pushed to a pull request |
| `github.pull_request.labeled.v1` | A label added to a pull request |
| `jira.comment.created.v1` | A comment on a Jira issue |
| `jira.issue.created.v1` | A new Jira issue |
| `jira.issue.assigned.v1` | A Jira issue assigned to an agent |
| `cron.tick.v1` | A schedule firing |

GitHub types follow `github.<event>.<action>.v1`, using GitHub's own event
and action names, so a new GitHub event is added without a naming debate.

### References

Every ingress body that can start a run embeds `Request`. Its `Mentions` are
the agents it names, and they alone wake members. Its `References` are
everything the text refers to, in the order written and once each, one
routing language for every surface:

```go
type Reference struct {
	Kind    string `json:"kind"`              // agent, person, place or link
	Name    string `json:"name,omitempty"`    // the agent's name, or what a person saw: "@ada", "#12"
	ID      string `json:"id,omitempty"`      // native id: Slack U07 or C02, GitHub login, Jira account id
	Address string `json:"address,omitempty"` // canonical: slack://T01/C02?thread=1699.001, github://acme/web/pull/12
	URL     string `json:"url,omitempty"`     // a link as written, line ranges and all
}
```

A link is a URL the text carried; a place is one it named without a URL
(`#12`, a Slack channel). A person's id is native to the surface the event
came from, the scheme of its source. Address is set only when it is known:
a GitHub file or commit link, or a Slack permalink from a workspace the
connector was not told about, keeps only its URL. A host's own URLs, such as
a link to a session, are addressed by the host (`mention.Links.Address`).
flow keeps no directory of people: a host maps the id to its own.

The renderer shows the model the people, places and links after the request
text, one line each, and never an id alone where a name exists:

```
summarise this

References:
- person @grace: U09
- place #general: slack://T01/C03
- link the PR: https://github.com/acme/handbook/pull/42/files#r1 (github://acme/handbook/pull/42)
```

Agents are left out: their mentions were taken out of the text on purpose.
A body with no references renders exactly as before.

## 3. Go shape

```go
// Body is the typed data of one event type. It is sealed: only this
// module defines bodies.
type Body interface {
	Type() string
	isBody()
}

type Envelope struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Type    string    `json:"type"`
	Subject string    `json:"subject,omitempty"`
	Channel string    `json:"channel"`
	Time    time.Time `json:"time"`
	Hops    int       `json:"hops"`

	Tenant  string `json:"tenant,omitempty"`
	Run     string `json:"run,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`

	Data    Body    `json:"data"`
	Private Private `json:"-"`
}

// Unknown keeps a type this build does not know, byte for byte.
type Unknown struct {
	TypeName string
	Raw      json.RawMessage
}
```

`Envelope.UnmarshalJSON` reads `type` first, looks it up in the catalog and
decodes `data` with that entry's `New`. A type the catalog does not hold
decodes to `Unknown{Type, Raw}`. It is never guessed from the shape of
`data`, and it is never an error: a subscriber that did not ask for the type
never sees it, and one that did receives the raw bytes. Marshalling an
`Unknown` writes `Raw` back unchanged.

A type that is known but whose `data` does not decode is an error. Fail
closed: a known type with a broken body is never passed on as `Unknown`.

## 4. Catalog entry

Every type is registered once, in one catalog.

```go
type Entry struct {
	Type      string                        // "message.posted.v1"
	New       func() Body                   // a zero body to decode into
	Render    func(Envelope) (string, error) // the body as model input
	Sensitive bool
	OptIn     bool
}
```

- `Render` turns the event into the text the model sees. It is byte stable:
  the same event renders the same bytes on every replay.
- `OptIn` keeps a family out of subscriptions that did not name it. The
  `run.usage.*` and `run.context.*` families are opt-in, as `usage` and
  `context` were.
- `Sensitive` follows one rule.

**Sensitivity rule.** A body is sensitive when it carries human-authored free
text or private host data. A sensitive body is rendered to the model, but it
is redacted from egress logs and webhooks unless the subscriber opts in
explicitly. Every ingress body with user text (`message.posted.v1`, the
GitHub and Jira comment and issue types, `slack.command.invoked.v1`) is
sensitive. Structural run bodies (`run.started.v1`, `run.usage.v1`) are not.
The model's own output (`run.delta.text.v1`) is not human-authored, and is
not sensitive.

`TestCatalogComplete` walks every registered body and fails on a type with no
renderer, no `New`, or a name outside the grammar. Adding a type without a
renderer fails the build's tests.

## 5. Channels

A channel is where an event is published. Channels are URI shaped and built
from `address.Address`:

```
<scheme>://<workspace>/<path>[?params]

slack://T01/C02
slack://T01/C02?thread=1699.001
github://acme/handbook/issues/42
jira://acme.atlassian.net/PROJ-7
cron://default/nightly-report
```

**The control channel** carries `control.stop.v1` and `control.compact.v1`:

```
control://<tenant>/<verb>/<sha256(tenant, run)[:16]>
```

The last segment is a hash so a run id is never spelled in a channel a
surface might log. Only a subscription that names a control channel exactly
ever matches it. A prefix, an empty path or a wildcard never does.

**Subscriptions.**

```go
type Subscription struct {
	Tenant   string   // from the principal, never from the request
	Channels []string // channel prefixes
	Types    []string // type prefixes
	Run      string   // optional: one run only
	From     uint64   // optional: resume after this sequence in each run
}
```

An empty `Channels` or `Types` list matches nothing. Default deny: there is no
"everything" subscription, and a zero `Subscription` receives no events.

Prefixes are segment aware. `slack://T01/C0` does not match `slack://T01/C02`,
and `run.delta` does not match `run.deltas.v1`. Channel segments split on
`/`, type segments on `.`.

## 6. Rules

- **Hops.** Every event carries `hops`. An event produced in answer to
  another has `hops` one higher. `DefaultMaxHops` is 8. An event over the
  limit is refused at publish, so two agents cannot answer each other
  forever.
- **Self-wake.** When `event.Agent` equals the subscriber, the event never
  wakes that subscriber. An agent's own output does not start a run of the
  same agent.
- **At-least-once.** Delivery is at-least-once. Egress adapters are
  idempotent, as sinks are today: one message per run, edited as events
  arrive.
- **Run key.** `RunKey(tenant, event id, subscriber)` is the run id. A
  redelivered event computes the same key and joins the existing run. Two
  agents woken by one event get two runs. Child run ids are derived as they
  are today and do not change.
- **Sequence.** Per-run `seq` banding is kept: each attempt and each child
  writes in its own band, so ordering inside a run survives retries.
- **Membership.** An agent is woken only by channels it is a member of
  (`flow/membership`), for the type prefixes its membership `Rule` names, and
  by default only when the event names it. Membership is default deny.

## 7. Definitions

Agents are saige definitions (`saige/agent/definition`) plus a flow
**Policy** sidecar, both in `flow/definition`. The version is saige's semantic
version; flow keeps no counter of its own.

A definition is one `.agent.md` file: YAML frontmatter, then the system
prompt.

```markdown
---
apiVersion: saige/v1
name: docs-bot
version: 1.0.0
description: answers questions about how we ship
model:
  use: anthropic/claude-haiku-5-5
  fallback: [openai/gpt-5-mini]
tools:
  registry: [lookup_runbook]
  mcp:
    - server: issues
subagents:
  - ref: helper@^1
approval:
  ask: [lookup_runbook]
---
You answer questions about the deploy pipeline. Cite your sources.
```

The sidecar holds what saige does not own. Its zero value grants nothing, and
its hash (`Policy.Hash`, domain `fiber/policy/v1`) is part of every pin.

| Field (JSON) | Meaning |
|--------------|---------|
| `subscriptions` | Channels and type prefixes the agent listens on. Recorded and hashed; what wakes an agent today is its membership (`flow/membership`) |
| `workspace_areas` | Sandbox areas the agent may touch |
| `mcp_write` | `tools.mcp` servers whose write tools it may call |
| `knowledge` | Corpora it may retrieve from |
| `fallback_on` | When a run falls back to another model |
| `approvers` | Who may approve its `approval.ask` calls |
| `self_approval` | Whether the person who asked may approve |
| `answerers` | Who may answer a parked question |
| `tagged_only` | Keep the agent out of messages that do not name it |
| `choices` | Models (`provider/model`) and reasoning efforts a person may pick for one message. Absent offers none; the host applies a pick to its copy of the turn's spec (`turn.Spec.Provider`, `Model`, `Effort`) |

**Validation.** A definition passes saige's own checks, then flow's, which
fail closed: a model is required and is `provider/model` (no presets); the
provider is in the deployment's table; at most 4 fallbacks, each with a
credential when the deployment checks credentials; sub-agents delegate only;
`approval` supports `ask` only, for tools the agent holds. Fields flow cannot
honor (dials, skills, memory, guardrails, compaction, harness tools, MCP allow
lists) are refused, not ignored. Names follow saige's rule and are valid
channel segments (`event.ValidToken`). Budgets are converted to `int64`
micro-USD at load; an amount that cannot be represented exactly is refused.

**The pin.** A run pins, in its durable input:

| Field | Source |
|-------|--------|
| `Name` | The definition name |
| `Version` | The exact semantic version resolved at accept time |
| `Digest` | saige's `Resolved.Digest` (`sha256:<hex>`) |
| `Subagents` | The exact version of every subagent |
| `PolicyHash` | The hash of the Policy sidecar |
| `Hash` | One hash over all of the above |

**Replay** reloads the exact versions named in the pin. It fails terminally
when the reloaded digest or policy hash does not match, when a pinned row is
missing, or when the digest is empty. A newer prompt never runs under an old
pin.

**The definition Source** is scoped by tenant and insert only. A version, once
written, is never updated, so a pin stays resolvable for as long as the run
can be replayed. `Put` of identical content is a no-op and of other content
under a written version is `ErrExists`. Retiring an agent stops new runs and
new versions; `Get` still reads every version. `Memory` and `Postgres`
(`fiber_definitions`, `fiber_definition_retirements`, no UPDATE path) pass
`definitiontest.Run`. A Resolver builds a saige registry over exactly the
tree a root reaches, per tenant, so saige's reference and cycle checks apply.

**Local runs.** `flow run [flags] agent.agent.md MESSAGE` stores the file
(and `-policy policy.json`) in memory, publishes one `message.posted.v1` for it
and prints the run's events by type.

## 8. Runtime

A run executes one turn at a time through a small interface in `flow/turn`:

```go
// Runtime runs one turn of a pinned definition for one input and streams
// catalog event bodies back.
type Runtime interface {
	Turn(ctx context.Context, def Pinned, in Input, emit func(context.Context, event.Body) error) (Result, error)
}

type Pinned struct {
	Name    string
	Version string
	Digest  string // the content hash at Version
	Spec    Spec   // the definition's content
}

type Input struct {
	Trigger      event.Envelope // the event the turn answers
	History      []Message      // the conversation so far, oldest first
	Instructions string         // a trusted host return contract, never user text
	Tenant       string         // from the principal
	Accounting   string         // the prefix model calls are keyed under
	Park         bool           // the turn may end at a question
	QuestionTTL  time.Duration
	Resume       *Parked        // continue a parked turn with its answers
}
```

- Every body emitted is a registered catalog type. The host stamps run
  identity (channel, run, sequence, tenant, version, digest) on each body; a
  Runtime cannot know it and never sets it.
- A stop reaches the turn on its context (`turn.StopSignal`), as does a
  request to compact (`turn.CompactSignal`) and the actor the turn acts for
  (`event.ActorFrom`). A stopped turn returns `Result.Stopped` and no error.
- The **native** implementation (`flow/turn/native`) wraps saige: it builds
  the agent from the pinned definition, renders the trigger through the
  catalog and turns saige deltas into `run.delta.*` bodies. With
  `flow/definition`, it is the only package that reaches saige's agent loop;
  the rest of flow imports saige's types alone.
- Another implementation can run any agent loop, as long as it emits catalog
  bodies. Pass it with `runtime.WithRuntime`; `turn.Func` adapts a function.
- **Sandbox** (`flow/turn/sandbox`) is a decorator around any `Runtime`. It
  runs the wrapped turn as a sandbox task under the pinned policy, so
  isolation does not depend on which runtime is inside it.
- **durable** stays the durable layer. The turn is still an durable activity,
  and every emitted body is written to the journal before the bus.
- `flow/turn/runtimetest` holds any `Runtime` to the same contract.

## 9. Adapters

Surfaces are adapters. Delivery does not know which surface it serves.

- **Ingress adapters** (Slack, GitHub, cron, the console) verify the
  request, translate it into one typed event, and publish it on the bus.
  They pick no agent: membership and the host do.
- **Egress adapters** (Slack, GitHub, console SSE) subscribe by
  channel plus type prefix and render what arrives onto their surface.

Implemented in `flow/connector` (`Trigger.Ingest(ctx, publish)`,
`Subscriber`), `flow/membership` and `flow/egress`.

- **Ingress.** Slack publishes `message.posted.v1` (mentions from `Bots`),
  `slack.command.invoked.v1` and `slack.session.stopped.v1`. GitHub publishes
  `github.*.v1`, with `head_sha` pinned before publish when checks are on; a
  bot sender is dropped except for `synchronize`, whose id is
  `github:commit:<repo>:<number>:<sha>`. Jira publishes `jira.*.v1`. Cron
  publishes `cron.tick.v1` with id `<name>@<unix>` on `cron://<tenant>/<name>`,
  and joins the schedule's agent to that channel. A `Prepare` hook takes
  `*event.Envelope`.
- **Egress.** A sink subscribes to its own scheme's channels with the run
  types, or names its channels and types (`connector.Subscriber`). A sink
  renders only its own scheme: a target of another connector is a config
  error. Opt-in types are delivered only when named. Rendering state is kept
  per `(run, channel)`.

## 10. Upgrade

This is the one place the names from before the union appear.

**Removed packages and shapes.**

| Before | Now |
|--------|-----|
| `pkg/envelope` (`Event`, `Kind`, `Filter`, `Subscription`) | `flow/event` (`Envelope`, `Body`, the catalog) and `bus.Subscription` |
| `pkg/topic` (`agent`, `agent:<name>`, `agent:<name>:<run>`, `agent:_control:<verb>-<key>`) | URI channels and `control://<tenant>/<verb>/<hash>` (`event.ControlChannel`) |
| `pkg/agentspec` (`AgentSpec`, revisions) | saige `.agent.md` definitions plus `definition.Policy` |
| `pkg/registry` and `registrytest` | `definition.Source` (`Memory`, `Postgres`) and `definitiontest` |
| `pkg/agentrt` and `pkg/agentrt/payload` | `flow/turn/native` and the bodies in `flow/event` |
| `runtime.Executor` | `turn.Runtime`, with `flow/turn/sandbox` as a decorator |
| The router and `Route` kind filters | `flow/egress` subscriptions by channel and type prefix |
| `Tag`, with free text in `Tag.Text` and context in `Tag.Meta` | An ingress event; the text is `Request.Text`, the context `Request.Context`, private keys `Envelope.Private` |
| A connector's `Trigger` choosing an agent | Ingress publishes on the surface's channel; membership and the host choose |

**Old kinds to types.** Every `envelope.Kind` maps to one type, through
`legacy.TypeOf`:

| Old kind | Type |
|----------|------|
| `lifecycle.<state>` (`accepted`, `started`, `parked`, `resumed`, `completed`, `failed`, `stopped`) | `run.<state>.v1` |
| `lifecycle.child.parked` | `run.child.parked.v1` |
| `delta.<name>` (`text`, `thinking`, `tool.call`, `tool.done`, `citation`) | `run.delta.<name>.v1` |
| `action.taken` | `run.action.taken.v1` |
| `usage.call` | `run.usage.v1` |
| `usage.child` | `run.usage.child.v1` |
| `context.compacted` | `run.context.compacted.v1` |
| `control.stop`, `control.compact` | `control.stop.v1`, `control.compact.v1` |
| `control.created`, `control.revised`, `control.deleted` | dropped: they had no producer |

- **Activity names.** The durable turn registers `fiber.turn.v2`,
  `fiber.turn.agent.v2` and `fiber.turn.resume.v2`. A run recorded under the
  v1 names (`fiber.turn`, `fiber.turn.agent`, `fiber.turn.resume`) or before
  the rename from mandatum fails terminally on replay, with the reason
  "started before the event union; send the message again", instead of
  running a body it was not recorded with.
- **Bus tables.** The channel bus writes new tables. The old `fiber_bus_*`
  tables are read by nothing after the upgrade and are safe to drop after
  24 hours, when every row in them has aged out.
- **Legacy decoders.** Stored JSON written before the union is read through
  `flow/event/legacy`, which reads raw JSON and imports none of the old
  packages: `DecodeBusRow` and `DecodeEntry` for bus rows and journal events
  with a `kind`, and `DecodeTag` for an old run input. `DecodeTag` chooses the
  type from the tag's meta (`cron_*` keys make `cron.tick.v1`, `github_*` a
  `github.*` type, `jira_*` a `jira.*` type, a slash command
  `slack.command.invoked.v1`, anything else `message.posted.v1`) and renders
  exactly the request text the tag rendered.
- **Agents.** `definition.FromAgentSpec` converts a stored spec
  deterministically: revision N becomes version N.0.0, provider and model
  become `model.use`, fallbacks `model.fallback`, `access.spawn` becomes
  `subagents`, `approvals` becomes `approval.ask`, `mcp:<p>[:write]` becomes
  `tools.mcp` with the write bit in `Policy.MCPWrite`, and everything else goes
  to the Policy. A spec whose names break saige's name rule is refused with a
  `NameError` listing them. The registry's tables `fiber_agents` and
  `fiber_agent_revisions` are no longer read or written, and can be dropped
  once no run pinned to a revision is still replayable.
- **Subscribers to topics.** A host that let callers subscribe to
  `agent:<name>` or `agent:<name>:<run>` translates those to channel and run
  subscriptions itself. `legacy.Topic` spells a run's old address, and
  `legacy.RunChannel` is the channel its old events are read onto.
- **Deploy.** Deploy with a single replica, or stop old replicas briefly
  before starting new ones. Old and new replicas do not share a bus.

## 11. Kept stored names

These names are in stored data and do not change:

| Kind | Names |
|------|-------|
| Tables | `fiber_runs`, `fiber_run_records`, `fiber_run_queue`, `fiber_threads`, `fiber_thread_entries`, `fiber_knowledge_documents`, `fiber_knowledge_chunks`, `fiber_tool_providers`, `fiber_tool_snapshots` |
| Record keys | `fiber.question.<id>`, `fiber.answer.<id>`, `fiber.child.<id>`, `fiber.stop`, `fiber.usage.a<attempt>`, `fiber.knowledge.v1` |
| Activity names | `fiber.turn`, `fiber.turn.agent`, `fiber.turn.resume`, kept only so a v1 run is recognised and failed terminally |
