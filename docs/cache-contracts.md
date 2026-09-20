# Cache contracts and known limits

Different caches have different identities and failure rules.
A single cache switch cannot represent them safely.

| Cache | Stored data | Reuse identity | Current support |
| --- | --- | --- | --- |
| Response cache | A completed model response | Request, schema, tools, model, configuration, tenant scope | Local decorator with explicit shared identity |
| Embedding cache | An embedding vector | Complete serialized input and immutable embedding configuration | Local LRU in `rag/embeddingcache` |
| Tool cache | A tool result | Configuration, policy, arguments, declared context, scope | Local policy and store |
| Automatic prompt cache | Provider computation for a prefix | Provider-specific routing and an exact prefix | Provider behavior; OpenAI affinity and retention options |
| Prompt markers | A provider prefix boundary | Ordered tools, system blocks, messages, marker TTL | Anthropic final-system-block marker |
| Explicit context cache | A provider resource | Resource name, model, prefix, tools, expiry | Google create, bind, refresh, and delete |
| Durable step record | A completed operation result | Workflow and step identity | StepRunner; this is recovery state |
| Conversation journal | Ordered state changes | Run, revision, transaction | Tree WAL integration; this is audit state |

## Response cache identity

A model name does not identify a complete request configuration.
Temperature, reasoning effort, endpoint, credentials, tool policy, and deployment revision can change the result.
Tenant scope determines who can read the result.

A response-cache wrapper uses a private instance identity by default.
Set both `ScopeKey` and `ConfigKey` for reuse across instances.
`ScopeKey` identifies the tenant and authorization scope. `ConfigKey` identifies the immutable configuration revision.
Do not put raw credentials in either field.
The host must change these keys after a relevant configuration or permission change.
`KeyNamespace` alone does not enable shared reuse.

Place response caches inside routing profiles. Each cache then belongs to one complete configuration.
A response cache outside a router can bypass route selection and reuse a previous provider's answer.
The router cannot establish affinity on a cache hit that bypasses it.

Tool-call responses are not cached by default.
`CacheToolCalls` explicitly permits replay of model tool decisions.
Replay assigns new tool-call IDs and copies argument maps. Tool gates still run and tools execute again.
A cached decision does not prove that a write is still correct or authorized.

The recorder rejects errors, cancellation, empty responses, and unfinished content blocks.
Replay preserves citations and copies mutable maps.
Response caching changes sampling: repeated requests return an old sample.
Disable it for independent evaluation samples and stochastic judges.
There is no response-cache single-flight mechanism, so concurrent misses can all incur cost.

## Provider prompt caches

Prompt-cache reuse still makes a provider request. It must not set `UsageDelta.CacheHit`.
That field means a local response replay, with no new provider call.
`CachedPromptTokens` and `CacheWriteTokens` identify provider cache tiers.
`PromptTokens` includes all input tiers. `UsageFromDelta` separates them for billing.
Google output usage includes reported thought tokens.

OpenAI supports automatic prefix reuse with cache affinity and retention controls.
`WithPromptCache(key, retention)` exposes those controls. A hit is not guaranteed.
Retention availability still depends on the selected model and endpoint.
See [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching).

Anthropic uses cache boundaries on an ordered prompt prefix.
`WithSystemPromptCache("5m")` or `WithSystemPromptCache("1h")` marks the final system block.
The current helper does not place message-level or tool-level markers.
See [Anthropic prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching).

Google explicit caching creates a resource before use. The resource has retention and storage costs.
`CreateContextCache` creates it. `WithContextCache` binds it to an adapter.
`RefreshContextCache` returns a new handle. `DeleteContextCache` removes the resource.
No method automatically creates a cache after a miss.
See [Google context caching](https://ai.google.dev/gemini-api/docs/generate-content/caching?hl=en).

The Google binding compares the exact model, message prefix, and tool set.
It rejects expired handles and changed prefixes. It sends only the uncached suffix.
A changed handoff view or tool policy can invalidate the binding.
The host must choose whether to create a new resource or use an uncached profile.
Resource names belong to a provider account, project, and region. Do not copy them across those boundaries.
The current handle does not encode all those deployment dimensions; the host must enforce them.

## Defects corrected in this change

| Defect | Effect | Correction |
| --- | --- | --- |
| Shared response key omitted configuration and scope | One configuration could receive another configuration's result | Private default identity; explicit scope and configuration keys |
| Cached argument maps were shared | A read or tool could modify a later replay | Copies at record and replay boundaries |
| Cached tool-call IDs were reused | Durable step names could collide | New call IDs on each replay |
| Citations were not recorded | Cached answers lost source data | Citation deltas retained |
| A closed partial stream could enter the cache | A truncated response became a repeated answer | Content-block completion check |
| Tool order came from map iteration | Equivalent requests could have different prompt prefixes | Stable registry order; response keys preserve actual prompt order |
| Provider cache counters were dropped | Budgets could not distinguish cache reads and writes | Explicit cache counters in normalized usage |

## Tool and embedding cache ownership

Tool wrappers use a private namespace by default. Set `ConfigKey` for explicit shared reuse.
Change it when the implementation or deployment changes. Scope and policy also contribute to identity.
Serialization errors fail before tool execution. Unsupported mutable citation metadata also fails instead of being shared.
`MaxEntries` is rejected because a wrapper cannot enforce capacity on a shared store. Configure a dedicated bounded store instead.
Single-flight followers receive detached results. A successful refresh enters the store before followers resume.
Stale-on-error retains successful entries for `TTL + MaxStale`. Cancellation does not serve stale data.
When stale-on-error is enabled, failed refreshes do not replace the previous successful entry.

Embedding keys hash the full input, including bytes, MIME type, metadata, and existing embeddings.
`WithConfigKey` binds the embedder revision. Vectors are copied at storage and return boundaries.
Incorrect output counts fail. The host must not modify input concurrently with a call.
These contracts prevent read results from mutating another session's cache state.
They do not invalidate cached reads after an external write; include a world revision when freshness requires it.

## Remaining work and implications

| Gap | Why it matters | Required contract |
| --- | --- | --- |
| No mutation-based invalidation | A cached read can remain stale after a write | World revision or dependency version in the key |
| No durable cache resource manager | A crash can leave billable Google resources | Resource leases, cleanup queue, expiry reconciliation |
| Cache write and storage tariffs vary | Token totals do not equal the provider invoice | TTL-specific rates, storage time, native-tool fees, and price revisions |
| Hidden retries and provider fees can have unknown charges | Outer usage is not a provider invoice | Per-attempt accounting and invoice reconciliation |
| Capabilities describe models more broadly than adapters | A declared native tool might not be wired into requests | Separate model, adapter, and deployment capabilities |
| Sticky routing state is in memory | Restart can select a different model and cold prefix | Persist route ID and configuration revision with the session |
| Signed reasoning differs by provider | A cross-provider transcript can contain incompatible signed blocks | Explicit migration or fail closed after incompatible context |
| Prefix length and retention vary | A configured cache can have no useful hits | Provider cache metrics and measured admission thresholds |

Prompt caches improve reuse of computation. They do not make model decisions deterministic.
A seed also does not guarantee identical output across model versions, providers, or serving infrastructure.
Store the selected profile, policy revisions, input revision, and actual output for each sample.
Use a separate sample ID when an LLM judge must produce an independent assessment.
