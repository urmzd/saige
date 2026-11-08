# durable

Part of [saige](../README.md). Import path: `github.com/urmzd/saige/durable`. saige's own durable agent runs (`agent/durable/anchor`) are built on it; see [durable execution](../docs/architecture/durable-execution.md).

> **Beta.** durable is pre-1.0 and under active development. Interfaces are stabilizing but may change between minor versions, and the Postgres adapter is young. Crash recovery and graceful shutdown are correct today: a cancelled worker persists in-flight outcomes and hands interrupted runs back without burning retry budget. Workflow bodies are single-goroutine in v1; distributed activities, forking and child runs, and durable timers are on the roadmap.

## Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Usage](#usage)
- [Development](#development)
- [Documentation](#documentation)
- [License](#license)

## Features

- **Wrap, don't rewrite**: `Activity`, `Step`, and `Event` wrap the code you have. Outside a run every wrapped function is a plain Go call, so tests, scripts, and half-migrated codebases keep working unchanged.
- **Interfaces first, no owned schema**: `ledger.Ledger` and `queue.Queue` are pure interfaces, the core packages import only the standard library, and the in-memory implementations are complete single-process systems.
- **Your tables, validated, never migrated**: the Postgres adapter maps onto tables you already own via a declarative `Mapping`. `Validate` introspects the live schema and errors with guidance; `RecommendedDDL` suggests DDL and never executes it.
- **Postgres-only works**: `pgqueue` (`FOR UPDATE SKIP LOCKED`) runs the queue in the same database, with streaming via `LISTEN/NOTIFY`. The queue is disposable: the janitor rebuilds it from the ledger.
- **Activities are the ABI**: records are keyed `name#occurrence`, so code rolls forward across releases with no version or patch APIs. Divergence fails loudly (`ErrNonDeterministic`); a breaking input/output change is a rename (`charge-payment.v2`).
- **Fenced attempts, budgeted failures**: `(runID, attempt)` fences every claim, and retry budget moves only when an execution genuinely fails, so waiting and shutdown interruptions are free. The happy path costs two run-level ledger writes plus one per record.
- **Waiting and streaming are native**: an `Event` with no record parks the run off the queue at zero cost, and activities `Emit` progress deltas that checkpointed activities never re-stream.
- **Errors that mean it**: retryable failures get replay-cheap retries, `NonRetryable` memoizes the failure, panics are retryable, `MaxAttempts` bounds failed executions, and `IdempotencyKey(ctx)` makes at-least-once effective-once downstream.

## Installation

```sh
go get github.com/urmzd/saige
```

The Postgres adapters (`durable/adapters/postgres/pgledger`, `pgqueue`, `pgkit`) are packages in the same module; Go only links the ones you import.

## Quick Start

You have an order pipeline:

```go
func process(ctx context.Context, o Order) (Receipt, error) {
	chargeID, err := charge(ctx, o)
	if err != nil {
		return Receipt{}, err
	}
	if _, err := reserve(ctx, o); err != nil {
		return Receipt{}, err
	}
	if _, err := confirm(ctx, o); err != nil {
		return Receipt{}, err
	}
	return Receipt{OrderID: o.ID, ChargeID: chargeID, Amount: o.Amount, IssuedAt: time.Now()}, nil
}
```

It works, until the process dies between the charge and the email. Wrap the pieces once, at package level:

```go
var (
	chargePayment    = durable.Activity("charge-payment", charge)
	reserveInventory = durable.Activity("reserve-inventory", reserve)
	sendConfirmation = durable.Activity("send-confirmation", confirm)
	processOrder     = durable.Activity("process-order", process)
)

func process(ctx context.Context, o Order) (Receipt, error) {
	chargeID, err := chargePayment.Call(ctx, o)
	if err != nil {
		return Receipt{}, err
	}
	if _, err := reserveInventory.Call(ctx, o); err != nil {
		return Receipt{}, err
	}
	if _, err := sendConfirmation.Call(ctx, o); err != nil {
		return Receipt{}, err
	}
	// Step records inline non-determinism: the first value is the value forever.
	issuedAt, err := durable.Step(ctx, "issued-at",
		func(context.Context) (time.Time, error) { return time.Now(), nil })
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{OrderID: o.ID, ChargeID: chargeID, Amount: o.Amount, IssuedAt: issuedAt}, nil
}
```

Then wire a ledger, a queue, and a worker. The worker can be your own process:

```go
// Ledger: your Postgres, your tables. durable only validates, never migrates.
pool, _ := pgxpool.New(ctx, "postgres://localhost:5432/app")
lgr, err := pgledger.New(pool, pgledger.DefaultMapping())
if err != nil {
	log.Fatal(err)
}
if err := lgr.Validate(ctx); err != nil {
	log.Fatal(err)
}

// Queue: disposable flow, in the same database. Wiping it loses no runs; they live in the ledger.
q, err := pgqueue.New(pool, pgqueue.DefaultMapping())
if err != nil {
	log.Fatal(err)
}

c := durable.New(lgr, q)

// The worker is a pull loop, not a service: run it on a goroutine.
w := worker.New(lgr, q)
go w.Run(ctx)

receipt, err := durable.Exec(ctx, c, processOrder,
	Order{ID: "1042", Email: "ada@example.com", Amount: 2400},
	durable.WithRunID("order-1042")) // the run ID is the submit idempotency key
```

Now kill the worker after `charge-payment` records. The lease lapses, the next claim takes attempt N+1, and the workflow re-executes from the top: `charge-payment` returns instantly from the ledger instead of executing (no double charge), `reserve-inventory` likewise, and execution resumes for real at the first unrecorded call. [`examples/agent/durable`](../examples/agent/durable/) runs a saige agent the same way on Postgres.

## Usage

### Concepts

| Concept | What it is |
|---------|------------|
| ledger | Durable truth: runs and their memoized records, in tables you already own (`ledger`) |
| queue | Flow and clock: delivery of run IDs under fenced, time-bounded claims; disposable, rebuildable from the ledger (`queue`) |
| worker | A pull loop with no inbound surface: claims, replays to the frontier, writes one outcome per claim (`worker`) |

### Bring Your Own Tables

`DefaultMapping` targets the suggested `durable_runs` / `durable_records` tables, but a mapping can point at tables you already have, including tables shared with other rows:

```go
lgr, err := pgledger.New(pool, pgledger.Mapping{
	Runs: pgledger.RunsMap{
		Table:    "orders",
		RunID:    "order_ref",
		Status:   "workflow_status",
		Envelope: "workflow_state",
	},
	Records: pgledger.RecordsMap{
		Table:    "order_events",
		RunID:    "order_ref",
		Key:      "event_key",
		Envelope: "payload",
		// Scope: this table also holds other rows; see only ours.
		Scope: pgledger.Scope{Column: "source", Value: "durable"},
		// Defaults: fill your own NOT NULL columns on insert.
		Defaults: func(rec run.Record) map[string]any {
			return map[string]any{"tenant_id": "acme"}
		},
	},
})
if err != nil {
	log.Fatal(err)
}
if err := lgr.Validate(ctx); err != nil {
	log.Fatal(err) // reports exactly what is missing; RecommendedDDL suggests the fix
}
```

Two validation rules worth knowing up front: mapped identifiers are lower_snake_case only, and the uniqueness the ledger depends on must be a plain unique index or unique constraint (the runs table on its run-ID column, the records table on its run-ID and key pair). Partial or expression unique indexes are rejected, because the adapter's `ON CONFLICT` writes cannot use them.

### Wait for a Human

Waiting is the absence of a record. An `Event` with no record parks the run: settled off the queue, still pending, costing nothing. Anything that writes the record and enqueues the run resumes it:

```go
// In the workflow: park until someone approves.
approval, err := durable.Event[Approval](ctx, "approval")

// Anywhere else, any process: approve and wake the run.
err = c.Signal(ctx, "order-1042", "approval", Approval{By: "ada"})
```

`Signal` is sugar for "write the event record, enqueue the run". A row inserted into your own table plus an enqueue does the same, and the janitor re-enqueues parked runs as a backstop. Parking costs nothing: a run can wait days and resume with its full retry budget intact. Call `Event` from the workflow body, not inside a nested activity: record keys are parent-scoped, and `Signal` targets top-level event keys.

### Stream Progress

```go
// Inside an activity: emit deltas.
_ = durable.Emit(ctx, progress{Stage: "charged"})

// Anywhere: catch-up, then live tail.
stream, _ := c.Watch(ctx, "order-1042", "")
for {
	d, _, err := stream.Recv(ctx)
	if err != nil {
		break
	}
	fmt.Printf("%s\n", d.Payload)
}
```

Deltas live on the queue side behind the optional `DeltaLog` capability (an append-only table plus `LISTEN/NOTIFY` in `pgqueue.NewDeltaQueue`), split by attempt and seal dividers so consumers can discard superseded partial output. Checkpointed activities never re-stream.

Postgres-only streaming swaps `pgqueue.New` for `pgqueue.NewDeltaQueue` and one more table (apply `pgqueue.RecommendedDeltaDDL` with your own migration tooling):

```go
q, err := pgqueue.NewDeltaQueue(pool, pgqueue.DefaultMapping(), pgqueue.DefaultDeltaMapping())
if err != nil {
	log.Fatal(err)
}
if err := q.Validate(ctx); err != nil {
	log.Fatal(err) // checks both the queue and the delta table
}
```

### Postgres Plumbing

`pgkit` holds the two pieces every Postgres-backed component needs. `Apply` runs idempotent steps in one transaction under an advisory lock named by the caller, so replicas that start together migrate one at a time:

```go
err := pgkit.Apply(ctx, pool, "orders.durable",
	pgledger.RecommendedDDL(pgledger.DefaultMapping()),
	pgqueue.RecommendedDDL(pgqueue.DefaultMapping()))
```

`Listener` multiplexes `LISTEN` over one dedicated connection, held only while something is subscribed. Every subscription starts with a `Resync`, and gets another after a reconnect, meaning "re-read your state, notifications may have been missed":

```go
l, err := pgkit.NewListener(pool)
notes, err := l.Listen(ctx, "orders") // closed when ctx ends
for n := range notes {
	if n.Resync {
		// re-read what you watch
		continue
	}
	fmt.Println(n.Payload)
}

// Share it: one connection for the delta log and everything else.
q, err := pgqueue.NewDeltaQueue(pool, qm, dm, pgqueue.WithListener(l))
```

## Development

```sh
just test-race          # every package, race detector on
just integration-up     # Postgres for the adapter tests
just test-postgres      # includes the durable adapters' integration tests
```

## Documentation

- [Engine overview](../docs/architecture/durable-engine.md): concepts, packages, adapters, capabilities
- [Replay design](../docs/architecture/durable-replay.md): the replay algorithm, the claim protocol, and the failure-mode table
- [AGENTS.md](../AGENTS.md): conventions and the engine's rules
- [Contributing](../CONTRIBUTING.md): development workflow and commit convention

## License

[Apache-2.0](../LICENSE)
