# flow

Part of [saige](../README.md). Import path: `github.com/urmzd/saige/flow`.

flow is the event layer and the durable turn on top of saige's agent loop.
Every event, from every surface and every run, is one typed envelope published
once on a channel; subscribers ask for channels and type prefixes. A run turns
an event into a durable turn on [`durable/`](../durable/): it pins the agent's
definition and thread history at accept, runs each turn through one seam
(`turn.Runtime`, natively on saige or inside [`sandbox/`](../sandbox/)), and
can be stopped, compacted, parked on a person and resumed, with its usage
counted once.

flow serves nothing and reads no configuration. A host composes it in Go:

```go
rt, err := runtime.New(durable.New(ledger, queue), definitions, broker,
	runtime.WithRuntime(turnRuntime),
	runtime.WithThreads(threads), runtime.WithSessions(sessions),
	runtime.WithChildRuns(), runtime.WithStopLedger(ledger), runtime.WithWaker(queue))
```

## Packages

| Package | Role |
|---------|------|
| `event`, `event/legacy` | The envelope, the sealed body union and its catalog; readers for older data |
| `address`, `attachment`, `blob` | Place URIs, file references and their bytes |
| `bus` | The channel bus, in memory or on Postgres |
| `connector` | The faces a connector implements |
| `connectors`, `connectors/slack`, `connectors/github`, `connectors/cron`, `connectors/mention` | The connector mesh and its Slack, GitHub and schedule peers; mentions and references |
| `membership`, `egress` | Who a channel event wakes; delivery of run events to sinks |
| `credential` | The token a connector or tool acts with, for an app or a person |
| `signature`, `deliverystate` | Webhook authentication; a sink's delivery state across replicas |
| `definition` | Agent definitions with a policy, pinned per run |
| `session`, `thread` | Continuing conversations and their journals |
| `turn`, `turn/native`, `turn/sandbox` | The turn seam and its two runtimes |
| `runtime` | The durable turn |
| `isolation` | Isolated instances for a run's tools |
| `wirecodes` | Wire codes for flow's sentinel errors |

## Documentation

- [Events and the durable turn](../docs/architecture/flow.md): delivery, durability, stopping, compaction, usage, parking
- [The event union](../docs/architecture/flow-events.md): the envelope, types, catalog, channels, definitions, the Runtime seam
- [Agent sessions](../docs/architecture/flow-sessions.md): keys, child calls, governance, budgets
- [Slack connector](../docs/architecture/flow-slack.md)
- [GitHub connector](../docs/architecture/flow-github.md) and [feedback](../docs/architecture/flow-github-feedback.md)
- [AGENTS.md](../AGENTS.md): the flow rules

## Development

```sh
go test -race ./flow/...
SAIGE_TEST_POSTGRES_DSN=postgres://... go test -race -p 1 ./flow/...   # the Postgres stores and the durable stop tests
```

## License

[Apache-2.0](../LICENSE)
