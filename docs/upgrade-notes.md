# Upgrade notes

These behavior changes can affect existing code. Each entry says what changed and what to do.

## Providers

| Change | What to do |
| --- | --- |
| The Anthropic and OpenAI chat adapters set SDK retries to 0 by default. | Wrap them in `retry.New(adapter, retry.DefaultConfig())`, or pass `WithMaxRetries(n)` for a bare adapter. The OpenAI embedder keeps the SDK default; pass `WithMaxRetries(0)` when a retry decorator wraps it. The `saige` CLI now wraps hosted providers in `retry.Provider`. |
| `fallback.New` uses `fallback.DefaultFallbackOn`: it falls back on every error except cancellation, `ErrInvalidModelConfig`, `ErrorKindInvalidRequest` and budget errors. | Set `FallbackOn` (for example `types.IsTransient`) to keep transient-only fallback. |
| Retry, fallback, cache, privacy and tracing decorators reject a response schema the inner provider cannot enforce, with an error matching `types.ErrSchemaUnsupported` and `types.ErrInvalidModelConfig`. Request options are rejected the same way (`types.ErrOptionsUnsupported`). | Use a provider that implements `StructuredOutputProvider`, or drop the schema. A fallback chain skips such a member and tries the next. |
| Every built-in adapter implements `types.OptionsProvider` (`ChatStreamWithOptions`), including the tool choice. Ollama emulates none and named choices by filtering tools and rejects required. | Forced tool choices from the agent now reach the adapters instead of failing with `ErrInvalidModelConfig`. |
| Adapters report an `ErrorDelta` for a stream that ends without a finish reason and for tool-call arguments cut off by the output token limit (`types.ErrResponseTruncated`). Complete but malformed tool-call arguments end the call with `ToolCallEndDelta.ArgumentsError`; the agent answers it with an "invalid tool arguments" result so the model can correct it. | Treat these as failures of the turn, not as an empty answer. |
| A Gemini error whose `google.rpc.ErrorInfo` reason is `API_KEY_INVALID`, `API_KEY_EXPIRED` or `ACCESS_TOKEN_EXPIRED` is `ErrorKindAuth` (`types.IsAuth`), not `ErrorKindInvalidRequest`, although Gemini sends it as HTTP 400. | Check `types.IsAuth` for a bad Google key. |
| `ModelCapabilities.Without(CapStructuredOutput)` also sets `StructuredOutput` to `StructuredOutputNone`. An Anthropic adapter that thinks by default therefore resolves `OutputAuto` to the `final_answer` tool instead of a forced-tool schema it would reject. | None. |
| The Ollama adapter reports `CapToolChoice` for models that declare tool calling, so `AgentConfig.ToolChoice` named and none reach its emulation. Required is still rejected before any request. | None. |
| The Ollama client has no total `http.Client` timeout; `WithStreamIdleTimeout` bounds silence between chunks instead. | Set a deadline on the context for a total bound. |
| Anthropic models that accept a trailing assistant turn declare `types.CapAssistantPrefill`, so `Agent.Continue` resumes the partial turn directly. Trailing whitespace is trimmed from that turn, which the API requires. | None. |
| The Google and Ollama embedders classify HTTP failures like chat errors, so a rate limit or overload is transient. The Google embedder sends `RETRIEVAL_QUERY` or `RETRIEVAL_DOCUMENT` from the embed purpose on the context; `WithTaskType` fixes one. | Embedding vectors from a Google model can change once a purpose is set; re-index if query and document vectors must match an older index. |

## Catalog and presets

| Change | What to do |
| --- | --- |
| The model table moved from Go code to the embedded `agent/provider/catalog/data/default.json`. Revisions it installs record the source `catalog/default.json`. Lookups are unchanged; a golden test freezes them. | Correct rows with a catalog layer or `catalog.Register`, as before. See [model catalog and presets](catalog.md). |
| Rows for `claude-haiku-5`, `claude-haiku-5-5`, `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-fable-5-1` and `claude-mythos-5-1` were added. The Claude 5 family and the 4.6, 4.7 and 4.8 rows declare a 1M context window and 128K output tokens. | Budgets and context-window routing see the larger window. |
| The router validates a request's options merged with the profile's own configured options (`types.OptionsReporter`), not the request options alone. A profile can be excluded that was eligible before, for example a `gpt-5.2` profile with a temperature and a request that sets a reasoning effort. | Intended: the request would have failed inside the adapter. The route reason is `options` when the group's first member was skipped. |
| `RouteDelta` carries `Preset`, `ConfigHash`, `CatalogRevision` and `Options`, and the agent attaches `RouteContent` to each committed assistant turn served through a router. | None; the fields are optional on the wire and in stored trees. |
| The span's `gen_ai.request.*` attributes come from the serving attempt's effective options when a route reports them. | Dashboards that read these attributes now see the options actually sent. |
| `OutputAuto` uses the native schema path only when the provider's reported capabilities include structured output, even for a model inferred from a family prefix. | None. |
| The CLI has no table of default models. Without `--preset`, `--model` or `--provider` it runs the first entry of the catalog's `default_preset` that can serve (a key is set, or the local Ollama server answers), as a one-entry chain. It no longer fails over across vendors by default. With no key and no Ollama it reports which variables to set. | Pass `--preset default` (or your own preset) for cross-vendor failover. Pass `--model` or `--preset` to pin one configuration. |
| An optional chain entry that needs no credentials (Ollama) is dropped at build time when its server does not answer `preset.Options.Probe` (default `preset.ProbeOllama`, a 2s version request). | Pass a `Probe` that returns nil to keep the old behavior. |
| `routing` gains `fail_threshold`, `reprobe_after` and `failover_on_auth`. The first two map to `router.Affinity`, so a session returns to the primary. An authentication failure still ends the request unless `failover_on_auth` is set. | None. |
| `--base-url` applies to the selected provider's entries on every path, including `--preset` and the default. A multi-vendor chain without `--provider` is an error. | Add `--provider` to say which entries the URL is for. |
| An untrusted project catalog is checked against an allowlist. Besides `base_url` and `api_key_env`, it may not set `mcp_server`, `routing.failover_on_content_filter`, `routing.failover_on_auth` or `inherit_default`. | Set `SAIGE_TRUST_PROJECT_CATALOG=1` or name the file with `--catalog`. |
| `types.ServerTool.Validate` requires a remote MCP server URL to be `https` with a host. | Use an https endpoint. |
| `HTTPSource` redacts user information and the query string from its name, which errors and installed revisions use. | Read the full URL from your own configuration, not from errors. |
| `RouteDelta.Provider` and `gen_ai.provider.name` name the adapter beneath decorators (`openai`, not `retry(openai)`). A routed span's `gen_ai.provider.name` and `gen_ai.request.model` come from the serving route. | Update dashboards or filters that matched `retry(...)`. |
| `StyledOutput.StreamDeltas` returns a terminal error without printing it; the caller reports it with `Output.Error`. `StreamVerbose` still prints it. | Call `Output.Error` with `VerboseResult.Err`. |

## Agent loop

| Change | What to do |
| --- | --- |
| With `AgentConfig.Store` set and no `Tree`, the default tree is built with `tree.WithStore`, so compaction branches, feedback, the active branch, archive state, rewinds and checkpoints persist. A failed store write now fails the change and the run. | Build a caller-supplied tree with `tree.WithStore(store)` too. `LoadTreeFromStore` with an empty branch reloads onto the saved active branch. |
| The tree stores `TruncationContent`, `SteerContent`, `ServerToolContent` and `RouteContent`. Server tool calls are recorded on the assistant message. | None; older trees still load. |
| Compaction moves its boundary so a tool result stays with its call, and compacted branch IDs no longer nest (`compact-main-<id>`). | Code that parsed nested branch names must read the new form. |
| `WithTracing` sets `AgentConfig.RunTracer`, so each run opens an `invoke_agent` span. The loop records cache token usage and the run outcome through `types.CacheUsageRecorder` and `types.AgentOutcomeRecorder`. | Remove a manual `NewAgentTracer(...).StartAgent` around `Invoke`, or the run gets two spans. |
| A marked tool is found through decorators that implement `Unwrap() types.Tool`, so it prompts for approval even when a decorator hides it. | None. |
| A repeated decision for an answered marker returns `agent.ErrMarkerResolved`; `saige serve` answers it with 409. | Treat 409 as already decided. |

## Storage

| Change | What to do |
| --- | --- |
| PostgreSQL 18 is required. `RunMigrations` reads `server_version_num` and returns `postgres.ErrUnsupportedServer` below 180000. | Upgrade the server to PostgreSQL 18, for example with the `paradedb/paradedb:0.26.1-pg18` image. See [deployment](deployment.md). |
| `RunMigrations` creates the `vector` and `pg_search` (ParadeDB) extensions and returns `postgres.ErrExtensionUnavailable` when either is not installed. It adds the BM25 index `idx_rag_variant_bm25` on `rag_variant.text`. | Install pg_search on the server, or use the ParadeDB image. pg_search is AGPL-3.0, licensed separately from saige. Building the index on a large `rag_variant` table takes time; run migrations before the rollout. |
| `rag.WithBM25` over a store that implements `types.KeywordSearcher`, such as `rag/pgstore`, searches through the store's BM25 index instead of an in-memory one. The `bm25retriever.Config` is ignored there, and BM25 scores come from pg_search. `pgstore.NewKeywordRetriever` exposes the same search as a retriever. | Drop the `rag.RebuildIndex` call after opening a pgstore pipeline; it is now a no-op for the BM25 arm. Retune a BM25 `MinScore` against pg_search scores. |
| `saige rag search` runs hybrid search (vector plus pg_search BM25), so keyword matches are found from a fresh process. | None. |
| `postgres.NewPool` with individual fields no longer forces `sslmode=disable`. pgx `prefer` encrypts without verifying the certificate and falls back to plaintext. | Set `sslmode` explicitly. |
| `RunMigrations` stops at the first failing statement and checks the embedding dimension when one is set explicitly. New migrations add `kg_episode.document_id`, `kg_relation_episode`, a relation fact search index, and `rag_document.scope` and `source_modified_at`. | Run migrations before starting the new release. |
| pgstore node reads are scoped to the conversation. `SaveNode` returns `ErrVersionConflict` for a stale version and `ErrConversationMismatch` for a node of another conversation. memstore also rejects stale versions. | Handle the errors instead of relying on silent skips. |
| The DBOS backend is removed. | Use the local engine or the duraturo adapter. |
| The module requires Go 1.26.4, the minimum of its duraturo dependency. | Build with Go 1.26.4 or later. |
