# Model capabilities, tools, and cost

This document describes what a model declares it accepts, how adapters enforce it, and the tool permission and cost controls built on those declarations. It ends with the known limits.

- [Capabilities and offerings](#capabilities-and-offerings)
- [Request validation and reasoning](#request-validation-and-reasoning)
- [Dials](#dials)
- [The category](#the-category)
- [Known limits](#known-limits)

## Capabilities and offerings

Every adapter in `agent/provider/` implements the same `types.Provider` interface: one method, `Stream(ctx, Request)`. That is the right seam for streaming, and it is why the agent loop does not care which vendor is behind it. It says nothing about what the model behind the adapter accepts, and models disagree in ways no interface hides:

| Question | Anthropic | OpenAI (reasoning) | OpenAI (chat) | Gemini 2.5 | Gemini 3 | Ollama |
|---|---|---|---|---|---|---|
| How is reasoning sized? | manual budget or adaptive effort | effort enum | not available | token budget | thinking level | on/off toggle |
| Does it take `temperature`? | per model and thinking mode | per model and effort | yes | yes | yes | yes |
| Schema output | forced tool call, or native on models that reject forcing | native | native | native | native | native |
| Reasoning signature round-trip | required | encrypted content on Responses | n/a | n/a | required | none returned |
| Server-side web search | yes | yes | no | yes | yes | no |
| Cost | per Mtok | per Mtok | per Mtok | per Mtok, tiered | per Mtok | free |

The [catalog](catalog.md) records these facts as data. An **offering** (`types.Offering`) is one model served through one endpoint: its parameter specification (`Params`, with ranges, defaults and constraints), its input and output modalities with their limits and the locators each reads, its features, structured output mode, server tools, pricing, service tiers and modality pricing.

`types.ModelCapabilities` is the projection of an offering that most code reads: a flat capability set, token limits, the reasoning vocabulary, the structured output mode, the media types it reads natively (`Media`), pricing and server tool kinds. `ModelCapabilities.Offering` points at the offering it came from, for the precise limits; it is nil for a declaration built by hand. Its zero value declares nothing and has `Known == false`, so "unsupported" and "never heard of it" stay distinguishable.

```go
caps, ok := catalog.Lookup(types.ProviderName("openai"), "gpt-6-luna") // the offering on the primary endpoint
off, ok := catalog.LookupOffering("openai-responses", "openai", "gpt-6-luna")
caps, ok = types.ProviderCapabilities(p) // what a provider stack reports
```

An adapter reports the offering of the endpoint it serves: a Google adapter on Vertex AI reports the Vertex offering, and the OpenAI Responses adapter the Responses offering. Decorators forward capabilities through `wrapper.Base`, and `wrapper.Describe(p)` reports what a whole provider stack declares. A fallback chain intersects its members' capabilities.

## Request validation and reasoning

Adapters enforce the declared controls before network I/O instead of silently dropping incompatible options. Call `adapter.Validate()` for an early check. `Stream` validates every request, including after `WithTarget`. Invalid settings return a permanent `ProviderError` wrapping `types.ErrInvalidModelConfig`, which a retry decorator does not retry. `New` checks the configuration it needs to build a client; Google also validates the controls at construction.

```go
p, err := openai.New(openai.Config{APIKey: key, Model: "o3"}, openai.WithTemperature(0))
err = p.Validate() // temperature: not declared supported for this model
if errors.Is(err, types.ErrInvalidModelConfig) { /* fix the configuration */ }

p, err = openai.New(openai.Config{APIKey: key, Model: "o3"}, openai.WithReasoningEffort("high"))
err = p.Validate() // nil
```

`Capabilities()` exposes the declared controls. `ReasoningRequired` distinguishes required reasoning from optional reasoning, and `DefaultReasoningEffort`, `ReasoningDefaultEnabled`, budget bounds, zero and dynamic budget support and `SamplingRequiresNoReasoning` describe conditional settings. `ValidateOptions(types.RequestOptions{...})` evaluates those rules for one request, and `Offering.Space().Validate` checks the offering's parameter space and constraints directly. A capability flag alone is not proof that every combination of controls is valid.

| Adapter | Enforced reasoning behavior | Visible reasoning |
|---|---|---|
| OpenAI Chat Completions | Effort enum per model; required reasoning cannot be disabled; unsupported sampling is rejected | Final text and usage, not raw internal reasoning |
| OpenAI Responses | As above, with reasoning returned as encrypted content | `ThinkingPart` with the encrypted content as its signature, and summaries with `WithReasoningSummary("auto")` |
| Anthropic | Manual budgets or adaptive effort where declared; a manual budget must fit below the output cap; thinking and sampling conflicts fail | Signed thinking parts, possibly empty depending on model and display |
| Google | Budget or level per family; positive budget bounds, dynamic `-1`, and forbidden disabling | Thinking parts and signatures returned by the API |
| Ollama | An explicit `think` must be declared; recognized sampling fields are validated from their JSON representation | Thinking chunks when the local model emits them |

GPT-5.1 and 5.2 allow temperature and top-p when reasoning is disabled; o-series and original GPT-5 models do not. Omitted effort preserves the provider default. [OpenAI parameter compatibility](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.2).

Gemini 2.5 Pro cannot disable thinking; Flash can. Budget `-1` means dynamic thinking, not off. The SDK's uppercase level constants are normalized for comparison with the catalog while keeping the SDK wire representation. [Google thinking controls](https://ai.google.dev/gemini-api/docs/generate-content/thinking).

Anthropic adaptive-only families reject manual budgets. `WithReasoningEffort` enables adaptive thinking and sends output effort. Manual thinking rejects non-default temperature, top-k, and top-p below 0.95. The adapter uses a forced tool for schema output, or `output_config.format` on a model that rejects forced tools. Manual thinking cannot use the forced tool. Adaptive thinking can use it when the model permits forced tools. [Anthropic thinking rules](https://platform.claude.com/docs/en/build-with-claude/thinking).

Ollama validates `top_k` as a nonnegative integer. Strings, fractions, and negative values fail before HTTP. This follows the [Ollama parameter types](https://docs.ollama.com/modelfile#valid-parameters-and-values).

An option the model does not accept fails; it is never ignored. Configure each fallback member for its own model, and do not share temperature across a chat and reasoning fallback and assume it was honored. Explicit zero, false and empty effort remain explicit; omitted values preserve defaults.

Request shape is checked too: every adapter requires streaming, tools when definitions are present, and structured output when `Request.Schema` is set. A provider says it applies a schema with `SupportsSchema()` and options with `SupportsOptions()` (`types.AcceptsSchema`, `types.AcceptsOptions`), and a decorator rejects either when the provider it wraps cannot apply it. An embedding-only model fails before a chat request is sent.

Media is checked the same way. Each attempt plans every media part against the serving offering's modalities: a part is sent natively, converted by an action the modality dial permits, or the request is rejected with `types.ErrModalityUnsupported`. See [modality conversion](modality-conversion.md).

**Tool choice per adapter.** Each adapter takes a tool choice at construction (`WithToolChoice`) and per request in `Request.Options`, which is how the agent forces a call for one turn. Anthropic maps auto, none, required (`any`) and named (`tool`); OpenAI maps auto, none, required and a named function; Google maps AUTO, NONE, ANY and ANY with `AllowedFunctionNames`. Ollama has no tool_choice field, so the choice is emulated by filtering the tools sent: none and named only, and required is rejected. Because of that emulation, the Ollama adapter reports `tool_choice` for every model that declares tool calling, even though the catalog does not.

Request rules were checked against the linked provider documents on 2026-09-20. Anthropic effort values follow its [effort availability table](https://platform.claude.com/docs/en/build-with-claude/effort).

## Dials

To share a setting across a chain, use a [dial](dials.md) instead of a raw option. A dial is an intent (`creativity`, `reasoning`, `max_output`, `tools`, `parallel`, `reproducible`, `cache`, `modality`) that is compiled per attempt against the serving offering. Raw options are still rejected, never stripped. A dial the model cannot honor exactly is mapped to the nearest declared value or dropped when it is advisory, lowered to the limit when it clamps, and rejected when it is contractual. Every decision is recorded in a `types.DialReport`.

| Adapter | `reasoning: {depth: high}` | `reasoning: {mode: off}` | `creativity: focused` |
|---|---|---|---|
| OpenAI, effort models | effort `high`; Chat Completions with tools on a model with the `openai.chat.tools` rule sends `none` | effort `none` when declared, else the lowest effort | `temperature` 0.3, `top_p` 0.9, dropped while reasoning is active on models that take sampling only without it |
| Anthropic adaptive | effort `high` | the lowest effort, since adaptive thinking cannot be turned off through this adapter | dropped: no sampling controls |
| Anthropic manual thinking | a budget, kept below `max_tokens` | nothing sent; thinking is off by default | dropped while thinking, applied otherwise |
| Google Gemini 3 | thinking level `HIGH` | the lowest level, since reasoning is required | `temperature` 0.3, `top_p` 0.9 |
| Google Gemini 2.5 | a budget by fraction of the declared range | budget 0 where declared, else the lowest depth | `temperature` 0.3, `top_p` 0.9 |
| Ollama reasoning models | `think: true`, recorded as mapped | `think: false` | `temperature` 0.3, `top_p` 0.9 |

The derived mapping comes from the offering's parameters, so user catalogs work unchanged. An offering's `dials` object overrides any part of it (see [the catalog guide](catalog.md#dials)).

```sh
saige models gpt-6-luna --provider openai
saige models gpt-6.1-sol --provider openai --format json
saige models gemini-3.1-flash-lite --provider google --format json
```

## The category

Six concepts in `agent/types`, plus two registries.

### 1. `Capability` and `ModelCapabilities`

`Capability` is a flat namespace covering both features ("this model reasons") and request knobs ("this model accepts `temperature`"), because callers ask the same question of both: may I rely on this, may I set this. In the catalog, features are an offering's `features` and knobs are its `params`.

`Intersect` is the operation fallback chains need: the capabilities a caller may rely on when any member might serve the request. It intersects the offerings too. Pricing is the exception: it takes the worse of the two, because a budget built on the cheaper member's rates under-counts exactly when the expensive fallback is in use.

### 2. `Citation` and `CitationRegistry`

One citation type for every producer: provider-native search, locally executed tools, MCP resource links, RAG retrieval. One registry per run assigns gap-free ordinals and deduplicates by source, so three producers citing the same page produce one footnote rather than three numbering schemes in one answer.

`ToolResult.Citations` is where a local tool contributes, and a tool result that carries citations reaches the model led by its sources and their markers (`[1] Title <uri>`), so the model can cite them. Model citations arrive as `CitationPart` parts. `CitationDelta` is how the consumer sees them arrive, already numbered.

### 3. `ServerTool` and `RemoteMCPServer`

A provider-neutral declaration of tools the provider runs: web search, code execution, remote MCP. A server tool has no local execution to gate, emits no `ToolExecStartDelta`, produces no durable step, and streams as `ServerToolCallPart` and `ServerToolResultPart` parts. `ValidateServerTools` turns an unsupported request into a startup error instead of a mid-stream 400.

### 4. `ToolGate`

Pre-gating that sees the resolved tool definition and the model's actual arguments. A `MarkedTool` marker is attached at registration, so it is all-or-nothing per tool. A gate allows a read and stops a write on the same tool, lets nine of an MCP server's tools through and gates the tenth, or clamps an argument instead of refusing the call.

`Gates(...)` composes them so the most restrictive verdict wins regardless of order: deny over approval over allow. A rewrite by an early gate is seen by later ones. When a person approves a call with edited arguments, the gate checks the edited arguments again before the tool runs.

### 5. `ToolContext` and `Deps`

Tools have two inputs that are not their arguments:

- **Context**: knobs a deployment or caller sets, such as a result limit, a root directory or a target length. They vary per run and sometimes per call, and the model never sees or sets them. They are immutable, because tool calls fan out and a shared mutable map would give parallel calls different configuration.
- **Dependencies**: services from upstream, such as a pool, an HTTP client or an embedder. They are wired once.

Both are addressed by a typed `types.Key[T]` (`types.NewKey[T]("name")`) and read with `types.Get` and `types.Dep`. `Configurable` binds both and returns a new tool, so one registered prototype produces differently configured instances per agent or tenant without shared state. Unmet dependencies fail at wiring time rather than on the first call.

### 6. `Pricing`, `Cost`, `BudgetPolicy`, `Budget`

`Cost` is integer micro-units, not float dollars: costs accumulate across thousands of calls and are compared against a hard limit.

Three states are distinct: **priced**, **free** (local inference, genuinely zero), and **unpriced** (no rate card). A budget refuses to run against unpriced usage rather than treating unknown as free. Usage of a modality the offering does not price (`modality_pricing`) makes a call unpriced too.

Enforcement is checked on every usage report, not once per iteration: one long-context call can cost more than the whole allowance, so a per-iteration check overshoots by an unbounded amount and a per-usage check by at most one call.

`BudgetRequireApproval` routes to the same human-in-the-loop path tool approvals use, so a consumer implements one resolution protocol. Approving buys a bounded `ApprovalGrant`, not an unlimited run.

### The two registries

`agent/registry` is append-only and revisioned. A registration adds a revision rather than replacing one; resolution takes the latest unless pinned; `Pin` freezes a name; `Rollback` steps back one revision.

- `agent/provider/catalog` is the **model registry**. Its models and offerings are installed from the embedded `data/default.json` and any layers a host adds (see [model catalog and presets](catalog.md)). They encode third-party facts that change without warning, so a price correction is a new revision, a deployment whose cost model was validated against a particular rate card pins it, and a bad correction is one `Rollback` away. Installed values own their maps and slices: editing a caller-held value cannot alter a recorded revision. Only the exact declared model name has `Known=true`; prefix, date-suffix, tag and namespace inference keeps the family metadata with `Known=false`, and `Lookup`'s boolean follows `Known`.
- `agent/registry.ToolSet` is the **tool registry**. `types.ToolRegistry` stays the hot path; `ToolSet` sits above it and answers which version is running, what it looked like before, and how to go back. `Snapshot()` materializes the resolving revision into a plain registry for an agent. It is a snapshot on purpose: a tool set that changed mid-run would change what the model may call after it was told.

Inspect the catalog from the CLI:

```
saige models                          # the whole table
saige models o3 --provider openai     # one model: flags, pricing, revisions, notes
saige models --format json
saige catalog show openai/gpt-6-luna  # endpoint, offering, modalities, tiers, options
```

## Known limits

**The catalog is curated, not discovered.** It is model metadata, not live discovery or a complete vendor compatibility matrix. Undeclared models report `Known=false` and inherit a family or an endpoint's baseline; applications that need verified support must reject unknown models or declare and pin a tested offering. Custom OpenAI-compatible endpoints can differ from OpenAI even with the same model name. New model variants can differ from their family; for example, Fable 5.1 rejects forced tools, which its offering declares. Validation covers the declared parameters and modalities, not every provider feature or undeclared numeric limit. `saige catalog reconcile` reports drift against vendor model lists (see [freshness check](catalog.md#freshness-check)).

**OpenAI server-side tools are declared but not sent.** Google search grounding and code execution (`google.WithServerTools`) and Anthropic web search and code execution (`anthropic.WithServerTools`) are sent, and their calls stream back as server tool parts. Anthropic remote MCP is rejected, because it needs the MCP connector. `provider.Build` serves the OpenAI models that need it through the Responses adapter (`openai.NewResponses`), but that adapter does not send server tools yet, so `provider.Build` rejects server tools for OpenAI and Ollama (`catalog.ExpressibleServerTools`).

**Cache accounting needs complete tariffs.** `UsageDelta` reports cache reads and writes, and OpenAI, Anthropic and Google populate them. The budget separates cache tiers, but cache storage and TTL-specific write prices still need a complete rate model. See [cache contracts](cache-contracts.md).

**Pricing coverage is incomplete.** The current model IDs are priced, but family prefixes (such as `claude-haiku-5` and `gemini-3-pro`), older models such as `gpt-5.2`, and every Ollama model are unpriced rather than guessed. A `Budget` refuses to run against them unless `AllowUnpriced` is set. Supply rates in a catalog layer or with `catalog.Register`; either appends a revision.

**Tiered pricing is flattened.** Gemini 2.5 Pro and gpt-6-luna charge more above a prompt length threshold; the catalog declares the lower tier, so long-prompt runs under-count.

**`types.Cache` has no scope-aware eviction.** `toolcache` namespaces keys by scope, but the backing store cannot evict a single tenant's entries. `CachePolicy.MaxEntries` is declared and not enforced.

**Ollama capabilities cannot be verified.** They depend on the pulled weights, and the daemon does not report whether a model supports tools or vision. Unrecognized local models resolve to the baseline with `Known == false`; there is no runtime probe.

**Server-tool calls are invisible to telemetry.** They emit no `ToolExec*` deltas, tool spans or tool metrics, so traces and metrics do not count them.

**Low-level clients do not validate.** Ollama's `Client` is a wire API, not the validating adapter; its `Options` struct omits zero values, whereas a map can send an explicit zero. Google generation config likewise uses zero as omission for its non-pointer output limit. The Anthropic effort option selects adaptive thinking and does not control legacy manual thinking.
