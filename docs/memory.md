# Memory

Memory lets an agent keep facts, events, and procedures across conversations. The host owns the scope, writes need approval by default, and recalled text reaches the model as external input, never as instructions (D-33).

- [Stores](#stores)
- [Scope](#scope)
- [Writes](#writes)
- [Recall modes](#recall-modes)
- [Postgres store](#postgres-store)
- [Conversation recall](#conversation-recall)
- [Retention and deletion](#retention-and-deletion)
- [Post-run extraction](#post-run-extraction)
- [Testing a store](#testing-a-store)

## Stores

Every store implements `memory.Store`: `Remember`, `Recall`, and `Forget`.

| Store | Package | Recall | Use it for |
| --- | --- | --- | --- |
| `MemStore`, `NewFixture` | `agent/memory` | Term match | Tests and evals |
| `FileStore` | `agent/memory` | Term match | One machine; adds the `memory` file command tool under `/memories` |
| `KGStore` | `agent/memory` | Knowledge graph facts | Memories as graph episodes |
| `pgstore.Store` | `agent/memory/pgstore` | Hybrid: pgvector, BM25, and recency | Multi-process and multi-tenant hosts |

## Scope

`Policy.Scope` maps the calling agent's name to a `memory.Scope`. The model never names a tenant, and a call without a resolvable scope fails with `ErrNoScope`.

| Field | Holds |
| --- | --- |
| `Tenant` | The isolation boundary. Required. |
| `Subject` | The user or project inside the tenant. |
| `Namespace` | A `/`-separated sub-partition, such as an agent name. `Narrow` adds a segment. |
| `ReadOnly` | Rejects writes. Give sub-agents a read-only scope. |

A scope sees its own namespace and every namespace below it, never its parent's or a sibling's. To scope by user, set `Subject` to the user ID. To scope by project, set `Subject` to the project ID, or narrow a user scope by project. To scope by agent, narrow by the agent's name:

```go
policy := memory.Policy{
    Scope: func(ctx context.Context, owner string) (memory.Scope, error) {
        s := memory.Scope{Tenant: tenantFrom(ctx), Subject: userFrom(ctx)}
        if owner != "coordinator" {
            s = s.Narrow(owner).AsReadOnly()
        }
        return s, nil
    },
}
```

## Writes

`memory.Tools(store, policy)` returns `recall`, `remember`, and `forget`, plus `memory` for a `FileStore`. Write tools carry an approval marker unless `Policy.AutoApprove` is set.

Every write passes `Policy.CheckContent`. Without `Policy.Redact`, content in which the detector finds a sensitive value fails with `ErrSensitive`, so a missing redactor fails closed. Tags pass the same check. `Policy.Write` limits the kinds the model may write.

A tool write uses the tool call ID as its idempotency key. A replayed call, as in a resumed durable run, returns the first ID and stores nothing new. Host writes through `Policy.Remember` should set `Record.IdempotencyKey` from the step that writes.

## Recall modes

`Policy.Recall` chooses how memories reach the model.

| Mode | What happens |
| --- | --- |
| `RecallByTool` (default) | The model calls `recall` when it needs to. |
| `RecallByInjection` | `Policy.StartMessage` recalls for the query and returns a message to prepend to the run. |
| `RecallBySelector` | `Policy.StartMessage` offers recent memories to `Policy.Selector` and injects the ones it picks. The default selector is BM25 over content and tags. |
| `RecallDisabled` | No recall tool and no injection. |

Injected recall is a user-role memory block. The outer tag and each record's tag end in a digest of their content, so stored text cannot close them. The block is never system content. `Policy.InjectBudget` caps its size in tokens.

```go
policy.Recall = memory.RecallByInjection
policy.InjectBudget = 500
msg, ok, err := policy.StartMessage(ctx, store, "assistant", userText)
if err != nil { return err }
input := []types.Message{types.NewUserMessage(userText)}
if ok {
    input = append([]types.Message{msg}, input...)
}
stream := worker.Invoke(ctx, input)
```

## Postgres store

`agent/memory/pgstore` stores memories in `memory_record` on PostgreSQL 18 with pgvector and ParadeDB pg_search. `postgres.RunMigrations` creates the table. Set `MigrationOptions.MemoryEmbeddingDim` to your embedder's dimension; an existing column of another width fails with `ErrEmbeddingDimMismatch`.

```go
pool, err := postgres.NewPool(ctx, postgres.Config{URL: dsn})
if err != nil { return err }
if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{MemoryEmbeddingDim: 768}); err != nil {
    return err
}
store, err := pgstore.New(pool, pgstore.Config{
    Embedder:      ollama.NewEmbedder(ollama.NewClient(host, "", "nomic-embed-text")),
    MinSimilarity: 0.5,
})
if err != nil { return err }
tools := memory.Tools(store, policy)
```

`Recall` and `Search` run two searches inside the scope, each limited to `Config.Candidates` rows:

1. A pgvector cosine search over the embeddings. Matches below `Config.MinSimilarity` are dropped. Tune it for the embedding model, since unrelated texts score differently per model.
2. A pg_search BM25 search over the content and tags. The query is a bind parameter, never query syntax.

The two rankings are fused with Reciprocal Rank Fusion (`Config.FusionK`). A recency ranking of the same candidates, weighted by `Config.RecencyWeight`, breaks ties toward newer records. A record neither search found is never returned. The result is cut to the token budget. An empty query returns the most recent records.

`Search` takes a `pgstore.Query` with more filters: `Kinds`, `Since`, and `Until`. Scope, expiry, kind, and time filters run in SQL, so another tenant's rows never reach the process. The vector search enables pgvector iterative scans, so a selective scope still returns enough rows.

Records are embedded with `types.PurposeDocument` and queries with `types.PurposeQuery`, so asymmetric embedders pick the right task type. A write with an idempotency key that already exists returns before it calls the embedder, and concurrent repeats store one row.

## Conversation recall

The Postgres store can also index past conversations kept by `agent/pgstore`, so an agent can answer "what did we discuss about X" in a later session. It is opt-in: nothing is indexed until the host calls `IndexConversation`.

```go
conv, err := agentpg.NewScopedStore(pool, tenant, sessionID, nil)
if err != nil { return err }
// ... run the agent with AgentConfig.Store = conv ...
n, err := store.IndexConversation(ctx, scope, conv.ConversationID())
```

- Each user and assistant turn becomes an episodic record. Tool calls, files, archived nodes, compaction summaries, and injected memory blocks are skipped.
- Each turn is redacted with `Config.Redact` before it is embedded or stored. The default is `privacy.Redact`, which writes `[REDACTED:LABEL]`. Vault placeholders such as `<<EMAIL_1>>` mean something only in the session that issued them, so they become `[REDACTED:EMAIL]`.
- A conversation ID from `agentpg.ScopedConversationID` must belong to the scope's tenant, or the call fails with `ErrForeignConversation`.
- Turns already indexed are not embedded again, so calling it after every run indexes only new turns.
- Turns and memories share the table but are never recalled together.

Expose turns in one of two ways:

| Way | How |
| --- | --- |
| Tool | `pgstore.ConversationTool(store, policy)` adds `recall_conversations(query, budget)`, a read tool. |
| Inject at start | `policy.StartMessage(ctx, store.Conversations(), owner, query)` with `RecallByInjection` or `RecallBySelector`. `InjectBudget` caps it. |

## Retention and deletion

| Need | Use |
| --- | --- |
| Expire new memories | `Policy.Retention` sets `ExpiresAt`. Expired records are never recalled. |
| Expire indexed turns | `pgstore.Config.ConversationRetention` |
| Reclaim expired rows | `store.Purge(ctx)`, on a schedule |
| Delete one memory | `forget` tool or `Store.Forget` |
| Delete a conversation's turns | `store.ForgetConversation(ctx, scope, conversationID)` |

## Post-run extraction

Automatic extraction is opt-in and runs after the run, never inline. Implement `memory.Extractor`, for example with a model that lists durable facts, and call `Policy.ExtractAfterRun` once the run has ended:

```go
stream := worker.Invoke(ctx, input)
// ... drain stream.Deltas() ...
if err := stream.Wait(); err != nil { return err }
msgs, err := worker.Tree().FlattenBranch(worker.Tree().Active())
if err != nil { return err }
ids, err := policy.ExtractAfterRun(ctx, store, extractor, "assistant", runID, msgs)
```

Each proposed record goes through `Policy.Remember`, so it passes the kind, content, and redaction checks. The scope comes from `Policy.Scope` and replaces any scope the extractor set. Idempotency keys come from the run ID, so calling it again for the same run stores nothing new.

To run it from the agent itself, add `memory.ExtractionHook(store, policy, extractor)` with `agent.WithHooks`. It calls `ExtractAfterRun` from a `RunStop` hook after each run that finished normally. See [run hooks](hooks.md).

## Testing a store

`agent/memory/memorytest.RunConformance` is the contract every store meets: scoped recall, sub-namespace visibility, tenant isolation, idempotent and concurrent replays, budgets, retention, read-only scopes, and `Forget`. `MemStore`, `FileStore`, and the Postgres store run it.

The Postgres tests need a server:

```bash
docker run --rm -e POSTGRES_PASSWORD=test -p 5433:5432 paradedb/paradedb:0.26.1-pg18
SAIGE_TEST_POSTGRES_DSN=postgres://postgres:test@localhost:5433/postgres go test ./agent/memory/...
```

`TestLiveCrossSessionRecall` also runs a real model and embedder: it saves a fact in one session and recalls it in later sessions, through the recall tool and through injected conversation turns. Set `SAIGE_TEST_OLLAMA_HOST` (with `nomic-embed-text` pulled) and `ANTHROPIC_API_KEY`.
