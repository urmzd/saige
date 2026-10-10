# System hardening

This release hardens saige across the agent loop, providers, safety controls, storage, retrieval, evaluation, and the CLI. The goal is that a run never silently drops data, never runs a tool on partial or invalid input, never hangs on an unanswered decision, and never lets a model widen its own permissions.

## Summary

- **Agent loop**: tool panics, malformed arguments, truncated turns, and runaway loops are contained in tool results or clear errors. One run holds a branch at a time, and new input joins an active run only at safe points (queue, steer, interrupt, continue).
- **Structured output**: `Structured[T]` validates, repairs, and can escalate to a stronger model through an `OutcomePolicy`. Response schemas are never dropped silently.
- **Delegation**: sub-agents can be delegated, spawned in the background, or handed off. Every decision (approval, clarification, budget escalation) is an addressable interrupt that a durable engine can suspend on and resume.
- **Providers**: every adapter accepts request options and tool choice, errors are classified (auth, rate limit, transient, invalid request), and retry, fallback, routing, traffic splits, and caching compose without stacking SDK retries.
- **Safety**: placeholder-based privacy at the tool and provider boundary, capability gates, per-tool quotas, deferred tool disclosure, skills that only narrow permissions, host-scoped memory, and opt-in tool packs (`fs`, `exec`, `fetch`) that are read-only or approval-gated by default.
- **Storage and wire format**: tenant-scoped stores, versioned writes, persisted tree state, and a versioned delta wire format.
- **RAG and eval**: source sync with replace-by-URI, scoped retrieval, rank fusion, and an eval stack with gates, Wilson intervals, regression comparison, trajectory scorers, a results store, and manifest validation.
- **CLI**: `saige serve` (HTTP and SSE with approve and cancel), JSON output, strict approval prompts, `saige update`, and an MCP server that enforces approval for mutating tools.

Final local check: `gofmt -l .`, `go build ./...`, `go vet ./...` and `golangci-lint run ./...` were clean, `govulncheck ./...` found no vulnerabilities, and `go test -race -count=1 -p 2 ./...` passed (83 packages ok, 0 failed, 24 without tests).

## Work by area

Status values: **implemented** (done and covered by tests), **partial** (done with a known limit listed under [Remaining gaps](#remaining-gaps-and-known-issues)). No planned item was skipped.

### Agent loop

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Concurrent tool calls keep call ID and result paired | implemented | `agent/agent.go`, `agent/aggregator.go` | `concurrency_test.go`, `aggregator_test.go`, `aggregator_args_test.go` |
| Tool, gate, and handoff panics become error results | implemented | `agent/agent.go` | `run_panic_test.go`, `failure_test.go` |
| Malformed or schema-mismatched arguments rejected before the gate | implemented | `agent/types/toolargs.go`, `agent/types/schema_validate.go` | `toolargs_test.go`, `schema_validate_test.go`, `tool_safety_test.go` |
| Truncated turns committed with `TruncationContent`, their tool calls never run | partial | `agent/forcing.go`, `agent/types/delta.go` | `forcing_test.go`, `stream_test.go` |
| One-turn forced tool choice, stop-at-tools, `MaxIterForceFinal` | implemented | `agent/forcing.go`, `agent/types/request_options.go` | `forcing_test.go`, `toolchoice_test.go`, `limits_test.go` |
| Run guards: one run per branch, consecutive errors, repeated calls | implemented | `agent/runguard.go`, `agent/deadline.go` | `run_guard_test.go`, `limits_test.go` |
| Submissions: queue, steer, interrupt-replace, side, continue | partial | `agent/submit.go`, `agent/stream.go` | `submit_test.go`, `submit_safety_test.go` |
| Compaction by input tokens, pair-safe boundary, context-length retry | partial | `agent/overflow.go`, `agent/tree/compact.go`, `agent/types/compact_clear.go` | `overflow_test.go`, `compact_test.go`, `compact_clear_test.go` |
| Gate composition ranks deny over approval over allow | implemented | `agent/types/gate.go` | `gate_test.go` |

### Structured output and escalation

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| `OutputMode` native, tool (`final_answer`), and prompt | implemented | `agent/output.go` | `output_test.go`, `output_check_test.go`, `response_schema_test.go` |
| `Structured[T]` with recursive validation, caller `Validate`, and `Repair` | implemented | `agent/structured.go` | `structured_test.go` |
| `OutputAuto` respects capabilities withdrawn by the adapter | partial | `agent/agent.go` (`checkStructuredOutput`), `agent/types/capabilities.go` | `capabilities_test.go`, `output_check_test.go` |
| `OutcomePolicy` and escalation ladders recorded in the tree | implemented | `agent/outcome.go`, `agent/types/outcome.go` | `outcome_test.go`, `router_pin_test.go` |

### Delegation, interrupts, and durable runs

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Child approvals forwarded to the root stream with stable interrupt IDs | implemented | `agent/interrupt.go`, `agent/types/interrupt.go` | `interrupt_test.go`, `delegation_test.go`, `interrupt_capacity_test.go` |
| `ResolveMarkerErr` and idempotent replies, `ErrMarkerResolved` | implemented | `agent/stream.go`, `agent/interrupt_router.go` | `interrupt_router_test.go`, `stream_test.go` |
| Interrupt expiry: deny, fail, escalate | implemented | `agent/interrupt.go` | `interrupt_test.go` |
| Spawn mode with `await_subagent`, `search_subagent`, `read_subagent` | implemented | `agent/spawn.go`, `agent/subagent_tools.go` | `spawn_test.go` |
| Context modes: task-only, fork, filtered | implemented | `agent/subagent_context.go` | `subagent_context_test.go` |
| Sub-agent timeouts and response schemas | implemented | `agent/subagent.go`, `agent/subagent_result.go` | `subagent_result_test.go`, `subagent_schema_test.go` |
| Handoff round trips | implemented | `agent/handoff.go`, `agent/handoff_context.go` | `handoff_test.go` |
| Local durable engine suspends on child approval and resumes after `Decide` | implemented | `agent/durable/local/local.go`, `router.go`, `input.go` | `local_test.go`, `router_test.go`, `lifecycle_test.go`, `input_test.go` |
| duraturo engine: replayed steps, parked approvals, idempotent replies, reconciliation | implemented | `agent/durable/duraturo/duraturo.go`, `runner.go`, `interrupt.go` | `duraturo_test.go`, `runner_test.go`, `router_test.go`, `postgres_test.go` |
| Clarification tool (`ask_user`) as an interrupt | implemented | `agent/clarify.go` | `interrupt_test.go` |

### Providers and routing

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| `OptionsProvider` and tool choice on every adapter (Ollama emulates named and none) | implemented | `agent/provider/*/options.go`, `agent/provider/*/tools.go`, `agent/types/options_provider.go` | `options_test.go`, `tools_test.go`, `agent_toolchoice_test.go` |
| Error classification, including Gemini `API_KEY_INVALID` as auth and `RetryInfo` delay | partial | `agent/types/errors.go`, `agent/provider/google/adapter.go` | `errors_test.go`, `google/errors_test.go` |
| Retry without stacked SDK retries, `Retry-After` honored | implemented | `agent/provider/retry/retry.go` | `retry_test.go`, `hardening_test.go` |
| Classified fallback | implemented | `agent/provider/fallback/fallback.go` | `fallback_test.go`, `classified_test.go` |
| Router with failover, sticky and affinity policies, route locks | implemented | `agent/provider/router/router.go`, `state.go` | `router_test.go`, `state_test.go` |
| Traffic splits with sticky assignment, canaries, budgeted shadow arms | implemented | `agent/provider/split/` | `split_test.go` |
| Response cache with a persisted codec | implemented | `agent/provider/cache/cache.go`, `codec.go`, `record.go` | `hardening_test.go`, `options_test.go` |
| Model catalog and live model listing | partial | `agent/provider/catalog/` | `discovery_test.go` |
| OpenAI Responses API adapter | implemented | `agent/provider/openai/responses.go` | `responses_test.go` |
| Decorator conventions (`Unwrap`, `As`, `Members`, `Innermost`) | implemented | `agent/provider/wrapper/` | `wrapper_test.go`, `wrapper_conformance_test.go` |
| `provider.Build` factory | implemented | `agent/provider/provider.go`, `generate.go` | `provider_test.go` |
| Prompt and context caching | implemented | `anthropic/prompt_cache.go`, `google/context_cache.go` | `prompt_cache_test.go`, `context_cache_test.go` |

### Safety controls

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Placeholder privacy vault, `ToolRedactor`, `privacy.Provider`, stream restore | implemented | `agent/privacy/` | `privacy_test.go`, `tool_redaction_test.go` |
| Capability gate and per-tool quotas | implemented | `agent/types/capability_gate.go`, `agent/types/tool_quota.go` | `tool_quota_test.go` |
| Deferred tool disclosure through `tool_search` | implemented | `agent/selector/` | `selector_test.go`, `deferred_test.go` |
| Skills: hashed snapshots, `allowed-tools` narrowing only | implemented | `agent/skills/` | `skill_test.go`, `policy_test.go`, `tools_test.go`, `frontmatter_test.go` |
| Host-scoped memory with approval-marked writes and sensitive-content check | implemented | `agent/memory/` | `memory_test.go` |
| Content-addressed workspace and `Spill` for large results | implemented | `agent/workspace/` | `workspace_test.go` |
| `fs` pack: root confinement, symlink checks, approval for writes | implemented | `tools/fs/`, `tools/internal/` | `fs_test.go` |
| `exec` pack: sandbox, command and network policy, always approval-marked | implemented | `tools/exec/` | `exec_test.go`, `proc_unix_test.go` |
| `fetch` pack: blocks private, loopback, and metadata addresses, including redirects | implemented | `tools/fetch/` | `fetch_test.go` |
| MCP client: pooled sessions, safe retries, catalog drift, capability gate | implemented | `agent/mcp/` | `connection_test.go`, `e2e_test.go` |
| `saige-mcp` approval for mutating tools (elicit, host, deny) | implemented | `cmd/saige-mcp/bridge.go` | `bridge_test.go` |
| `saige-mcp` streamable HTTP: bearer tokens, per-token rate limit, TLS or loopback bind | implemented | `cmd/saige-mcp/http.go` | `http_test.go` |
| `saige-mcp` agent tool: inner marked calls decided like direct calls | implemented | `cmd/saige-mcp/agenttool.go` | `agenttool_test.go` |
| Research tools path safety | implemented | `tools/research/safepath.go` | `tools_test.go`, `longline_test.go` |

### Storage and wire format

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Tree state persisted through the store (branches, feedback, active branch) | implemented | `agent/tree/persist.go`, `agent/tree/tree.go` | `persist_test.go`, `agent_store_test.go` |
| Tenant scope and version conflicts in pgstore and memstore | implemented | `agent/pgstore/`, `agent/store/memstore/` | `scope_test.go`, `conformance_test.go`, `agent/store/storetest/` |
| WAL recovery | implemented | `agent/store/walrecover/`, `agent/store/filewal/` | `walrecover_test.go`, `recover_internal_test.go` |
| Versioned delta wire format and tree format version | implemented | `agent/types/wire.go`, `agent/tree/serialize.go` | `wire_test.go`, `wire_toolargs_test.go`, `serialize_test.go`, `codec_test.go` |
| Remote stream consumption (`NewRemoteStream`) and AG-UI mapping | implemented | `agent/stream.go`, `agent/agui/` | `stream_test.go`, `agui_test.go` |
| Postgres migrations stop on first failure, no forced `sslmode=disable` | implemented | `postgres/migrate.go`, `postgres/pool.go` | `migrate_test.go`, `pool_test.go` |

### RAG and knowledge graph

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| `SyncSource` replaces content at the same source URI | implemented | `rag/internal/pipeline/sync.go`, `rag/types/sync.go` | `sync_test.go` |
| Scoped retrieval across stores and retrievers | implemented | `rag/types/filter.go`, `rag/*/scope_test.go` | `scope_test.go`, `filter_test.go` |
| Rank fusion (RRF, weighted) and BM25 contract | implemented | `rag/fusion/`, `rag/bm25retriever/` | `fusion_test.go`, `contract_test.go` |
| Parent-context expansion | partial | `rag/parentretriever/retriever.go`, `rag/chunker/chunker.go` | `retriever_test.go`, `chunker_property_test.go` |
| Embedding purpose, batching, retry, validation | implemented | `rag/embedderregistry/` | `purpose_test.go`, `retry_internal_test.go`, `validation_test.go` |
| Source size limits and modified times | implemented | `rag/source/limits.go`, `rag/source/http.go` | `limits_test.go`, `modified_test.go` |
| Extractor fuzzing | implemented | `rag/extractor/` | `fuzz_test.go` |
| Observability for the pipeline and graph | implemented | `rag/otel/`, `rag/knowledge/observe.go`, `rag/internal/pipeline/observe.go` | `otel_test.go`, `observe_test.go` |
| Knowledge graph episode provenance and document deletion | implemented | `rag/knowledge/pgstore/`, `rag/knowledge/internal/engine/` | `graph_test.go`, `ingest_test.go` |

### Evaluation

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Thresholds and assertions set pass flags, Wilson intervals | implemented | `eval/assert.go`, `eval/check.go`, `eval/group.go` | `assert_test.go`, `check_test.go`, `group_test.go` |
| Regression comparison with McNemar test | implemented | `eval/compare.go`, `eval/analysis/` | `compare_test.go`, `analysis_test.go` |
| Results store (memory and JSONL files) | implemented | `eval/store/` | `eval/store/storetest/` |
| Trajectory scorers from a real agent run | implemented | `agent/eval/trajectory.go`, `agent/eval/collect.go` | `trajectory_test.go`, `collect_test.go` |
| Judge with structured output | implemented | `eval/judge.go`, `agent/eval/generator.go` | `judge_test.go`, `judge_structured_test.go` |
| Text quality scorers (token F1, ROUGE-L) | partial | `eval/textquality.go` | `textquality_test.go` |
| Manifest validation with JSON pointers | implemented | `eval/harness/manifest.go`, `cmd/saige/eval_validate.go` | `manifest_test.go`, `eval_run_test.go` |

### Observability

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| Run spans (`invoke_agent`), cache usage and outcome metrics, redaction | implemented | `agent/otel/` | `option_test.go`, `provider_test.go`, `tool_test.go`, `metrics_test.go` |

### CLI and servers

| Item | Status | Key files | Tests |
| --- | --- | --- | --- |
| `saige serve`: sessions, resumable SSE, approve, cancel, host check | implemented | `cmd/saige/serve.go`, `serve_server.go` | `serve_test.go` |
| JSON output for `ask` and `chat` | implemented | `agent/tui/output_json.go`, `cmd/saige/ask.go` | `ask_test.go` |
| Strict approval prompt (only y/yes or n/no, end of input denies) | implemented | `agent/tui/approval.go`, `agent/tui/runner.go` | `approval_test.go`, `runner_test.go` |
| `saige rag` against pgvector | partial | `cmd/saige/rag.go` | `rag_pg_test.go` |
| `saige update` with checksum verification, `install.sh` | implemented | `cmd/saige/update.go`, `install.sh` | `update_test.go` |
| `saige models` catalog view | implemented | `cmd/saige/models.go` | `provider_test.go` |

## Breaking changes and migration

Behavior changes are also listed in [upgrade notes](upgrade-notes.md).

| Change | Migration |
| --- | --- |
| Anthropic and OpenAI chat adapters set SDK retries to 0. | Wrap with `retry.New(adapter, retry.DefaultConfig())`, or pass `WithMaxRetries(n)` to a bare adapter. |
| `fallback.New` falls back on every error except cancellation, invalid model config, invalid request, and budget errors. | Set `FallbackOn: types.IsTransient` to keep transient-only fallback. |
| Decorators reject a response schema or request options the inner provider cannot enforce (`ErrSchemaUnsupported`, `ErrOptionsUnsupported`). | Use a provider that implements `StructuredOutputProvider` or `OptionsProvider`, or drop the schema or option. |
| A second `Invoke` or `Continue` on a busy branch fails with `ErrRunActive`. | Use `Agent.Submit` or `EventStream.Submit` to queue or steer into the active run. |
| Required and named tool choices apply to one turn, then revert to auto. | Set the choice again if a later turn must be forced. |
| Malformed tool arguments return an "invalid tool arguments" result instead of reaching the tool. A truncated tool call fails the turn with `ErrResponseTruncated`. | Treat these as turn failures, not empty answers. |
| A repeated decision for an answered marker returns `ErrMarkerResolved`; `saige serve` answers 409. | Prefer `ResolveMarkerErr` over `ResolveMarker` and treat 409 as already decided. |
| `AgentConfig.Store` without a `Tree` builds a store-backed tree, and failed store writes fail the run. | Build caller-supplied trees with `tree.WithStore(store)`. |
| Compacted branch IDs no longer nest (`compact-main-<id>`). | Update code that parses branch names. |
| `WithTracing` opens an `invoke_agent` span per run. | Remove a manual `StartAgent` around `Invoke` to avoid duplicate spans. |
| pgstore reads are scoped to the conversation; `SaveNode` returns `ErrVersionConflict` and `ErrConversationMismatch`. | Handle the errors instead of relying on silent skips. |
| `postgres.NewPool` no longer forces `sslmode=disable`. New migrations add columns and tables. | Set `sslmode` explicitly and run migrations before deploying. |
| The DBOS backend is removed. | Use the local engine or the duraturo adapter. |
| `eval.ToolCallRecord` gains `ID`, `ArgumentsError`, and `Exec`; `tool_success_rate` counts argument errors and unfinished calls as failures. | Re-baseline stored results that relied on the old rate. |
| Google embeddings send a retrieval task type from the embed purpose. | Re-index if query and document vectors must match an older index, or fix the type with `WithTaskType`. |
| CLI: `saige ask --raw` is removed; `install.sh` runs under `bash`; `ask` denies approval-marked tools by default. | Use `--template minimal`, pipe the installer to `bash`, and pass `--approve=allow` when unattended writes are intended. |

## Live validation

Scenarios were run against live providers on top of the unit and race tests. Where a scenario was re-run after fixes, the table shows the final result and notes the change.

| Scenario | Provider | Result | Evidence |
| --- | --- | --- | --- |
| Parallel tool-call pairing | OpenAI gpt-4o-mini | pass | 2 calls in turn 1, each result matched its own call ID's arguments |
| Parallel tool-call pairing | Ollama qwen3.5:4b | pass | Same pairs as OpenAI, 2 provider calls |
| Delta codec round trip | OpenAI, Ollama | pass | 36 and 29 live deltas re-encoded byte-identical, 0 failures |
| Tool panic contained | OpenAI, Ollama | pass | `Wait()=nil`, `ToolExecEnd.Error="tool explode panicked: boom-from-tool"` |
| Truncated JSON arguments rejected | OpenAI, Ollama | pass | "invalid tool arguments: unexpected end of JSON input", tool not run; OpenAI retried and succeeded |
| Schema-mismatched arguments rejected | OpenAI, Ollama | pass | "property \"city\" must be string, got number", 0 executions |
| Named tool choice forced for one turn | OpenAI | pass | `calls=[ping]` on turn 1, auto on turn 2 |
| Named tool choice | Ollama qwen3.5:4b | pass (after fix) | Was rejected with "tool_choice: not declared supported". Now the first turn sends only the named tool, `calls=[ping]`, `err=nil`. Emulation filters tools and cannot force a call |
| Required tool choice | OpenAI | pass | Forced once, then reverted |
| Required tool choice | Ollama | pass | Rejected before any request, as documented |
| Stop at tools | OpenAI, Ollama | pass | `StopToolCallID` set, result "submitted:Oslo", 1 provider call |
| `MaxIterForceFinal` | OpenAI, Ollama | pass | 1 tool run, final tool-free call; default policy returns "max iterations reached" |
| `Structured[T]` native | OpenAI, Ollama | pass | `response_format` / `format` schema sent, 1 attempt, typed value decoded |
| `Structured[T]` with tools (`final_answer`) | OpenAI, Ollama | pass | Auto chose tool mode, value used the tool result |
| `Structured[T]` repair after `Validate` rejects | OpenAI, Ollama | pass | 2 attempts, second answer accepted |
| `OutcomePolicy` escalation weak to strong | Router (Ollama, OpenAI) | pass | Switch on `schema_invalid` logged, `RouteDelta` emitted, OpenAI answer accepted |
| Escalation ladder exhausted | Router (Ollama) | pass | `errors.Is(err, ErrSchemaInvalid)=true` after 2 attempts |
| `OutputAuto` on a model that rejects forced tool choice | Anthropic claude-sonnet-5-5 | unit tested | Auto picks native output: the adapter sends the schema as `output_config.format` for a model that declares `forced_tool_choice: false` |
| Delegate with child approval via `ResolveMarkerErr` | OpenAI, Ollama | pass | Child marker on parent stream at depth 0, approved, 1 write, no hang |
| Durable suspend, `Decide`, resume | OpenAI, Ollama | pass | First run `ErrSuspended` with 1 interrupt; resume completed with 1 write |
| Spawn and `await_subagent` | OpenAI, Ollama | pass | Handle returned at once, awaited result "Canberra" |
| `search_subagent` over child transcript | OpenAI | pass | Returned the matching assistant line |
| Fork context vs task-only | OpenAI | pass | Fork saw the earlier code word, task-only did not |
| Handoff round trip | OpenAI | pass | triage to billing to triage, "TRIAGE DONE" |
| Handoff round trip | Ollama qwen3.5:4b | pass (2 of 3) | One run repeated handoffs until `MaxIter` stopped it cleanly; model behavior |
| Queue mid-run | OpenAI | pass | Held until the first answer finished, then answered "BANANA" |
| Steer between tool calls | OpenAI | pass | Landed after the tool result, pairing intact, model followed it |
| Interrupt-replace | OpenAI | pass | Partial text committed with `TruncationContent`, new message answered |
| Continue after output-limit truncation | OpenAI | pass | Continued without repeating text |
| Token-based compaction | OpenAI | pass | Branch moved to `compact-main-*`, summarized fact recalled; input stayed above the limit after one pass |
| Router: permanent error does not fail over | OpenAI to Ollama | pass | 404 invalid request, 0 fallback requests |
| Router: transient 503 fails over | OpenAI to Ollama | pass | Route reason `failover`, answer from Ollama |
| Retry count without SDK stacking | OpenAI, Anthropic, Google (forced errors) | pass | HTTP requests equal `MaxAttempts`; `Retry-After: 1` honored |
| Forced Google 429 | Google adapter | pass | `kind=rate_limit`, `retryAfter=7s` from `RetryInfo`; header-only `Retry-After` reads 0 |
| Invalid Google key classified as auth | Google gemini-2.5-flash | pass (after fix) | Was `InvalidRequest`; now `kind=auth`, also through `privacy.Provider` |
| Split sticky assignment | OpenAI, Ollama | pass | Same arm across turns and split instances, `RouteDelta` carries experiment and variant |
| Response cache across instances | OpenAI | pass | 1 HTTP request for 2 calls, `CacheHit=true`, metadata preserved |
| Model listing | Ollama, OpenAI | pass | 13 and 133 models listed |
| Privacy: `ToolRedactor` with stream restore | OpenAI | pass | Requests held only `<<EMAIL_1>>`, tool received the real address, stream restored |
| Privacy: shared vault with `privacy.Provider` | OpenAI | pass | No real address in any request body |
| Skills: load and `allowed-tools` narrowing | OpenAI | pass | Tools narrowed after load; `delete_note` ran 0 times |
| Memory: approval-gated write and recall | OpenAI | pass | Deny wrote nothing; approve stored the record; recall answered "Teal"; SSN content refused |
| Workspace spill | Ollama | pass | 60 KB result stored, 2 KB preview sent, tail sentinel never reached the provider |
| `fs` path traversal | OpenAI and direct calls | pass | `..`, absolute paths, and symlinks rejected; nothing written outside the root |
| `exec` approval and policy | OpenAI | pass | Deny ran nothing; network blocked by default; `sudo` denied by policy |
| `fetch` metadata address blocking | direct calls | pass | Decimal, hex, IPv6-mapped, DNS, and redirect forms all blocked |
| `saige-mcp` approval for `store_knowledge` | MCP stdio client | pass | Decline, missing elicitation, and `--approval=deny` all refused; accept ran once |
| RAG ingest and hybrid search | Ollama nomic-embed-text | pass | Vector and BM25 hits returned |
| Re-ingest at the same URI via `SyncSource` | Ollama | pass | Document UUID kept, old text gone |
| `saige rag ingest` and `search` | Ollama + pgvector | pass (after re-run) | First run had no Postgres; with pgvector, 2 hits ranked correctly |
| `eval.Run` gates and Wilson intervals | Ollama | pass | 3/3 interval [0.439, 1.000]; failing case flagged |
| Regression comparison | Ollama | pass | 2 regressions found, also after reload from the file store |
| Trajectory scorers | Ollama | pass | All trajectory scores 1.0 on a real run |
| Judge with structured output | OpenAI | pass | Schema path used, scores 1.0 and 0.0 as expected |
| `saige eval validate` | offline | pass | Field-specific errors with JSON pointers, exit code 2 |
| `go build ./cmd/...`, `saige models` | offline | pass | Both binaries built; catalog, JSON, and unknown-model output correct |
| `saige ask` and `chat` (text, stdin, JSON) | OpenAI | pass | JSONL envelopes on stdout, prompts on stderr |
| Approval prompt input validation | OpenAI | pass | Empty and invalid input re-prompt; end of input denies |
| `saige serve` SSE, approve/deny, cancel, delete | OpenAI | pass | Resume by `Last-Event-ID`, 409 on repeated decision, 415 and 403 checks |
| `saige-mcp` start and tool listing | offline | pass | Read-only tools listed, invalid regex returned `isError=true` |
| Parallel tool-call pairing | Anthropic claude-haiku-5-5 | pass | Each result paired with its own call ID |
| Named and required tool choice | Anthropic claude-haiku-5-5 | pass | `{"type":"tool"}` and `{"type":"any"}` accepted with adaptive thinking on, forced for one turn, then absent |
| Stop at tools | Anthropic claude-haiku-5-5 | pass | `StopToolCallID` set, 1 provider call |
| `MaxIterForceFinal` | Anthropic claude-haiku-5-5 | pass | 1 tool run, final tool-free call |
| `Structured[T]` auto, with tools, and repair | Anthropic claude-haiku-5-5 | pass | Auto without tools used the forced `structured_output` tool; with tools it chose `final_answer` with `tool_choice` absent; repair accepted the second answer |
| Delegate with child approval | Anthropic claude-haiku-5-5 | pass | Child marker approved, 1 write, no hang |
| Durable suspend, `Decide`, resume | Anthropic claude-haiku-5-5 | pass | First run `ErrSuspended`, resume completed |
| Spawn and `await_subagent` | Anthropic claude-haiku-5-5 | pass | Handle returned at once, result awaited |
| Handoff round trip | Anthropic claude-haiku-5-5 | pass | Control returned to the first agent |
| Continue after output-limit truncation | Anthropic claude-haiku-5-5 | pass | Continued through the continue-prompt path, since the model accepts no prefill |
| Submit queue and `InterruptReplace` | Anthropic claude-haiku-5-5 | pass | Queued message answered after the run; replace committed the partial turn |
| Prompt caching | Anthropic claude-haiku-5-5 | pass | First call wrote 2643 cache tokens, the second read 2643 |
| `web_search` server tool deltas | Anthropic claude-haiku-5-5 | pass | `ServerToolCallDelta` and `ServerToolResultDelta` streamed |
| Preset failover on 529 | Anthropic to OpenAI | pass | Forced 529 failed over to OpenAI; route names named the vendor and model, not the decorators |
| `ToolRedactor` | Anthropic claude-haiku-5-5 | pass | Request bodies held only placeholders |
| `saige ask` and `--format json` | Anthropic claude-haiku-5-5 | pass | Text answer, and JSONL envelopes ending in `usage` and `done` |
| Empty system prompt | Anthropic claude-haiku-5-5 | pass (after fix) | Was 400 "text content blocks must be non-empty"; `system` is now omitted when no text is left |
| Forced tool choice on an adaptive model | Anthropic claude-haiku-5-5 | pass (after fix) | Was rejected locally because thinking is on by default; only a manual thinking budget, or a model that declares `forced_tool_choice: false`, now rejects it |
| Tool call on Chat Completions | OpenAI gpt-6-luna | pass | The adapter sent `reasoning_effort: "none"` with the tools; `parallel_tool_calls: false` accepted |
| Tool call through the Responses API | OpenAI gpt-6.1-sol | pass | `provider.Build` served the model on `/v1/responses`; tool ran once; `parallel_tool_calls: false` accepted |
| One-call smoke | OpenAI gpt-6-luna, gpt-6.1-sol | pass | Answered "ok" on Chat Completions and Responses |
| `saige ask` default and `openai`, `openai-quality` presets | Anthropic claude-haiku-5-5, OpenAI | pass | The default preset ran claude-haiku-5-5; both OpenAI presets answered |
| Text answer | Google gemini-3.1-flash-lite, gemini-3.8-flash | pass | Answered "ok", 1 request each |
| Tool call | Google gemini-3.1-flash-lite, gemini-3.8-flash | pass (after fix) | Was 400 "Function call is missing a thought_signature" on the second turn. The adapter now returns the signature Gemini puts on the function call part; tool ran once, 2 requests |
| `Structured[T]` auto | Google gemini-3.1-flash-lite, gemini-3.8-flash | pass | Native `responseSchema` path, 1 request, typed value decoded |
| `saige ask --preset google` and `google-quality` | Google | pass | gemini-3.1-flash-lite and gemini-3.8-flash answered |
| Vertex AI text, tool call and `Structured[T]` | Google gemini-3.1-flash-lite on Vertex AI | pass | `GOOGLE_GENAI_USE_VERTEXAI=true` with the project and `global` location from the environment and no API key; Application Default Credentials attached; tool ran once; native schema path |
| Vertex AI embeddings | Google gemini-embedding-001 on Vertex AI | pass | `WithEmbedVertex`, 3072 dimensions |
| `saige --provider vertex ask` | Google gemini-3.1-flash-lite on Vertex AI | pass | Answered "ok" with no Gemini API key set |
| Live Google 429 | Google | not exercised | No cheap way to trigger one; covered by the forced 429 scenario |

## Remaining gaps and known issues

| Issue | Where | Effect |
| --- | --- | --- |
| Plain `Ingest` of changed content at the same URI creates a second document. | `rag/internal/pipeline/pipeline.go` | Documented: use `SyncSource` or `Update` for replace-by-URI. |
| A reply that lands while its duraturo run is still parking waits for the worker's janitor. | `agent/durable/duraturo/interrupt.go` | The run resumes on the next janitor pass instead of at once; set `worker.WithJanitorEvery` to bound the delay. |
| Spawn mode is unavailable under durable runners. | `agent/spawn.go` | By design: returns `ErrSpawnUnsupported`. |
