# Load and concurrency testing

The stress suite in `integration/stress/` checks that the SDK stays correct when many runs, subscribers, and workers share it at once. It sits behind the `stress` build tag, so `go test ./...` never runs it.

## Run the suite

In-process tests need nothing else:

```sh
go test -tags stress -race -count=1 -v ./integration/stress/
```

Set `SAIGE_TEST_POSTGRES_DSN` to also run the Postgres tests. Each test creates and drops its own database. The server needs pgvector and pg_search:

```sh
docker run -d --rm --name saige-stress -e POSTGRES_PASSWORD=test -p 5433:5432 paradedb/paradedb:0.26.1-pg18
SAIGE_TEST_POSTGRES_DSN='postgres://postgres:test@localhost:5433/postgres?sslmode=disable' \
  go test -tags stress -race -count=1 -v ./integration/stress/
docker rm -f saige-stress
```

Each test reports p50, p99, and throughput with `t.Log`, so run with `-v` to see them.

| Test | Needs | What it checks |
| --- | --- | --- |
| `TestConcurrentAgents` | | 200 agents run two-turn conversations with parallel tool calls. Every model request carries exactly the results of its own calls. No goroutines leak. |
| `TestSameBranchContention` | | Many `Invoke` and `Submit` calls on one branch. One run holds the branch at a time, a refused `Invoke` gets `ErrRunActive` and leaves no trace, and every accepted message is appended once. |
| `TestNotifierFanOut` | Postgres | 100 subscribers over 4 notifiers get each of 1,000 publishes exactly once, and again after every listening connection is killed and replaced. |
| `TestCacheCoherence` | Postgres | Four two-level caches over one `postgres.CacheStore` under concurrent get, set, and invalidate. Once writers stop, every process reads what the store holds. |
| `TestPgstoreIngestAndHybridSearch` | Postgres | Concurrent ingest while readers fuse vector and keyword searches. Afterwards each document ranks first for its own vector and keyword. |
| `TestDurableWorkersCompete` | Postgres | Four duraturo workers share one queue. Each run's tool and model steps execute once, and approval runs resume after `Decide`. |
| `TestLiveBurst` | `SAIGE_LIVE=1`, keys | 20 parallel calls each to `gpt-6-luna` and `claude-haiku-5-5` through `retry.Provider`. Reports 429s, retries, whether `Retry-After` was honored, and successes. |

Tunables:

| Variable | Default | Effect |
| --- | --- | --- |
| `SAIGE_STRESS_AGENTS` | 200 | Agents in `TestConcurrentAgents` |
| `SAIGE_STRESS_BURST` | 20 | Parallel calls per model in `TestLiveBurst` |

The live burst sends a one-word prompt with a 32-token output cap, so a run costs well under a cent. It reads `OPENAI_API_KEY` and `ANTHROPIC_API_KEY` and skips a model whose key is unset.

## Benchmarks

The benchmarks run on every P, so `-cpu` shows how the agent loop, the retry wrapper, and the router scale across cores:

```sh
go test -run '^$' -bench . -cpu 1,4,8 ./...
```

Narrow it to the concurrency benchmarks with `-bench 'ParallelTools|RetryOverhead|RouterOverhead' ./agent/ ./agent/provider/retry/ ./agent/provider/router/`.

## CI

`.github/workflows/stress.yml` runs the suite with the race detector and a ParadeDB service every night and on manual dispatch. It never runs on pull requests. The live burst runs only on a manual dispatch with the `live` input set, and only when the provider key secrets exist.
