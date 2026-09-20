# Model capabilities, tools, and cost: the category and the gaps

This document describes model capabilities, tool permissions, and cost controls.
It explains the contracts and records known limits.

## Request validation and reasoning

Reasoning support already existed. Adapters now enforce configured controls before network I/O
instead of silently dropping incompatible options. Call `adapter.Validate()` for an early check;
both plain and schema streaming paths revalidate, including after `WithModel`. Invalid settings
return a permanent `ProviderError` wrapping `types.ErrInvalidModelConfig`. A retry wrapper does
not retry them. Constructors keep their existing signatures; Google also validates at construction.

```go
p := openai.NewAdapter(key, "o3", openai.WithTemperature(0))
err := p.Validate() // temperature: not declared supported for this model
if errors.Is(err, types.ErrInvalidModelConfig) { /* fix the configuration */ }

p = openai.NewAdapter(key, "o3", openai.WithReasoningEffort("high"))
err = p.Validate() // nil
```

`Capabilities()` exposes declared controls. `ReasoningRequired` distinguishes required reasoning
from optional reasoning, and `DefaultReasoningEffort`, `ReasoningDefaultEnabled`, budget bounds,
zero/dynamic budget support and `SamplingRequiresNoReasoning` describe conditional settings.
`ValidateOptions(types.RequestOptions{...})` evaluates those rules for a particular request.
A capability flag alone is not proof that every combination of controls is valid.

| Adapter | Enforced reasoning behavior | Visible reasoning |
|---|---|---|
| OpenAI Chat Completions | Effort enum per cataloged model; required reasoning cannot be disabled; unsupported sampling is rejected | Final text and usage, not raw internal reasoning |
| Anthropic | Manual budgets or adaptive effort where declared; manual budget must fit below output cap; thinking/sampling conflicts fail | Signed thinking blocks, possibly empty depending on model/display |
| Google | Budget versus level per family; positive budget bounds, dynamic `-1`, and forbidden disabling | Thinking parts and signatures returned by the API |
| Ollama adapter | Explicit `think` must be declared; recognized sampling fields validated from their actual JSON representation | Thinking chunks when the local model emits them |

GPT-5.1/5.2 allow temperature and top-p when reasoning is disabled; o-series and original GPT-5
rows do not. Omitted effort preserves the provider default. These newer rows are deliberately
unpriced rather than inheriting an older rate. [OpenAI parameter compatibility](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.2).

Gemini 2.5 Pro cannot disable thinking; Flash can. Budget `-1` means dynamic thinking, not off.
The SDK's uppercase level constants are normalized for comparison with the catalog while retaining
the SDK wire representation. [Google thinking controls](https://ai.google.dev/gemini-api/docs/generate-content/thinking).

Anthropic adaptive-only families reject manual budgets. `WithReasoningEffort` enables adaptive
thinking and sends output effort. Manual thinking rejects non-default temperature, top-k, and
top-p below 0.95. This adapter uses a forced tool for schema output.
Manual thinking cannot use this path. Adaptive thinking can use it when the model permits forced tools. [Anthropic thinking rules](https://platform.claude.com/docs/en/build-with-claude/thinking).

Ollama validates `top_k` as a nonnegative integer. Strings, fractions, and negative values fail
before HTTP. This follows the [Ollama parameter types](https://docs.ollama.com/modelfile#valid-parameters-and-values).

**Compatibility change:** a previously ignored option now fails. Configure each fallback member
for its own model. Do not share temperature across a chat/reasoning fallback and assume it was
honored. Explicit zero, false and empty effort remain explicit; omitted values preserve defaults.

```sh
go run ./cmd/saige models o3 --provider openai
go run ./cmd/saige models gpt-5.2 --provider openai --format json
go run ./cmd/saige models gemini-2.5-pro --provider google --format json
```

Request shape is checked too: all four adapters require streaming, tools when definitions are
present, and structured output when a schema is supplied. An embedding-only model now fails
before sending a chat request. This check applies to both plain and schema entry points.

Catalog registration, baseline registration, returned registrations, history, lookup and rollback
own their maps and slices. Editing a caller-held value cannot alter a recorded revision. To change
capabilities, register a new revision. Only the exact declared model name has `Known=true`;
prefix, date-suffix, tag and namespace inference retains the family metadata with `Known=false`.
`Lookup`'s boolean follows `Known`, not whether any family prefix matched. Exact registrations
for a variant or tagged/namespaced local model override inferred family metadata.

**Limits:** the catalog remains curated model metadata, not live discovery or a
complete vendor compatibility matrix. Undeclared models report `Known=false` and may inherit a family or use a baseline;
applications requiring verified support must reject unknown models or register and pin a tested
entry. Custom OpenAI-compatible endpoints can differ from OpenAI even with the same model name.
Fallback intersections retain the stricter reasoning restrictions, but each adapter remains the
final validator. Ollama's low-level `Client` is a wire API, not the validating adapter; its legacy
`Options` struct omits zero values, whereas a map can send an explicit zero. Google generation
config likewise uses zero as omission for its non-pointer output limit. Validation covers exposed
controls, not every provider feature, media combination, or undeclared numeric limit.
The Anthropic effort option selects adaptive thinking. It does not expose independent effort
control for legacy manual thinking. New model variants can differ from an inferred family;
for example, Fable 5.1 rejects forced tools. Register an exact declaration without `CapToolChoice`
before using such a variant through this adapter.

Request rules were checked against the linked provider documents on 2026-09-20.
Anthropic effort values follow its [effort availability table](https://platform.claude.com/docs/en/build-with-claude/effort).
These checks do not refresh the price table or certify every endpoint.

## Why a category was needed

Every adapter in `agent/provider/` implements the same `types.Provider`
interface. That is the right seam for streaming, and it is why the agent loop
does not care which vendor is behind it. But it says nothing about what the
model behind the adapter will accept, and the models disagree with each other
in ways no amount of interface uniformity hides:

| Question | Anthropic | OpenAI (reasoning) | OpenAI (chat) | Gemini 2.5 | Gemini 3 | Ollama |
|---|---|---|---|---|---|---|
| How is reasoning sized? | manual budget or adaptive effort | effort enum | not available | token budget | thinking level | on/off toggle |
| Does it take `temperature`? | per model and thinking mode | per model and effort | yes | yes | yes | yes |
| Schema output | emulated via forced tool call | native | native | native | native | native |
| Reasoning signature round-trip | required | n/a | n/a | n/a | required | none returned |
| Server-side web search | yes | yes | no | yes | yes | no |
| Cost | per Mtok | per Mtok | per Mtok | per Mtok, tiered | per Mtok | free |

Before this branch none of that was expressed anywhere. Code either assumed one
shape and broke on another, or asked the only question available -- "does the
adapter implement `StructuredOutputProvider`?" -- which answers what the
*adapter* can encode, never what the *model* will accept.

## The category

Six types, in `agent/types`, plus two registries.

### 1. `Capability` and `ModelCapabilities`

`Capability` is a flat namespace covering both features ("this model reasons")
and request knobs ("this model accepts `temperature`"), because callers ask the
same question of both: *may I rely on this / may I set this*.

`ModelCapabilities` is the declared surface for one `(provider, model)` pair:
the capability set, hard limits, reasoning vocabulary, structured-output mode,
native media types, pricing, and server-tool kinds. Its zero value declares
nothing and has `Known == false`, so "unsupported" and "never heard of it" stay
distinguishable and callers that must fail closed can.

`Intersect` is the operation fallback chains need: capabilities a caller may
rely on when *any* member might serve the request. Pricing is the exception --
it takes the worse of the two, because a budget built on the cheaper member's
rates under-counts exactly when the expensive fallback is in use.

### 2. `Citation` and `CitationRegistry`

One citation type for every producer: provider-native search, locally-executed
tools, MCP resource links, RAG retrieval. One registry per run assigns gap-free
ordinals and deduplicates by source, so three producers citing the same page
produce one footnote rather than three conflicting numbering schemes in one
answer.

`ToolResult.Citations` is where a local tool contributes. `CitationDelta` is
how the consumer sees them arrive, already numbered.

### 3. `ServerTool` and `RemoteMCPServer`

A provider-neutral declaration of tools the *provider* runs: web search, code
execution, remote MCP. The distinction from a local tool is operational, not
cosmetic: a server tool has no local execution to gate, emits no
`ToolExecStartDelta`, produces no durable step, and leaves no trace but its
citation metadata. `ValidateServerTools` turns an unsupported request into a
startup error instead of a mid-stream 400.

### 4. `ToolGate`

Pre-gating that sees the resolved tool definition **and the model's actual
arguments**. The pre-existing `MarkedTool` mechanism cannot: a marker is
attached at registration, so it is all-or-nothing per tool. A gate allows a
read and stops a write on the same tool, waves through nine of an MCP server's
tools and gates the tenth, or clamps an argument instead of refusing the call.

`Gates(...)` composes them so the most restrictive verdict wins regardless of
ordering, and a rewrite by an early gate is seen by later ones.

### 5. `ToolContext` and `Deps`

Tools have two inputs that are not their arguments, and conflating them is what
made them hard to reuse:

- **Context** -- knobs a deployment or caller sets: a result limit, a root
  directory, a target length. Data, varies per run and sometimes per call, and
  the model must never see or set it. Immutable, because tool calls are fanned
  out and a shared mutable map gives parallel calls different configuration
  with no ordering guarantee.
- **Dependencies** -- services from upstream: a pool, an HTTP client, an
  embedder. Wired once, do not vary per call.

`Configurable` binds both and returns a *new* tool, so one registered prototype
produces differently-configured instances per agent or tenant without shared
state. Unmet dependencies fail at wiring time rather than nil-panicking on the
first call.

### 6. `Pricing`, `Cost`, `BudgetPolicy`, `Budget`

`Cost` is integer micro-units, not float dollars: costs accumulate across
thousands of calls and are compared against a hard limit, and a budget that
stops at 99.9997% because of float drift is not a budget.

Three states, deliberately distinct: **priced**, **free** (local inference --
genuinely zero), and **unpriced** (no rate card). A budget refuses to run
against unpriced usage rather than treating unknown as free.

Enforcement is checked on **every usage report**, not once per iteration: one
long-context call can cost more than the whole allowance, so a per-iteration
check overshoots by an unbounded amount and a per-usage check by at most one
call.

`BudgetRequireApproval` routes to the same human-in-the-loop path tool
approvals use, so a consumer implements one resolution protocol. Approving buys
a bounded `ApprovalGrant`, not an unlimited run.

### The two registries

`agent/registry` is append-only and revisioned. A registration adds a revision
rather than replacing one; resolution takes the latest unless pinned; `Pin`
freezes a name; `Rollback` steps back one revision.

- `agent/provider/catalog` is the **model registry**, backed by it. Rows encode
  third-party facts that change without warning, so a price correction is a new
  revision, a deployment whose cost model was validated against a particular
  rate card pins it, and a bad correction is one `Rollback` away.
- `agent/registry.ToolSet` is the **tool registry**. `types.ToolRegistry`
  stays the hot path; `ToolSet` sits above it and answers which version is
  running, what it looked like before, and how to go back. `Snapshot()`
  materialises the resolving revision into a plain registry for an agent, and
  is deliberately a snapshot: a tool set that mutated mid-run would change what
  the model may call after it was already told.

Inspect any of it from the CLI:

```
saige models                      # the whole table
saige models o3 --provider openai # one model, flags, pricing, revisions, notes
saige models --format json
```

## Gaps found

### Fixed on this branch

**1. Decorators silently erased capabilities.** `retry`, `fallback` and `cache`
did not forward `ContentNegotiator`, so wrapping an adapter made the file
pipeline believe the model supported no media natively and extract every image
and PDF to text. `cache` also did not implement `ModelSwitcher`, so a
`ConfigContent` model switch under a cache was dropped *and* the request was
answered from the original model's cache entries. `otel` had the same gap.
All four now forward content support, model switching and capabilities;
`fallback` intersects rather than reporting its primary's.

**2. Google adapter discarded all reasoning.** It read `resp.Text()`, which
skips thought parts, and never inspected `Part.Thought`. Every reasoning token
from Gemini 2.5 and 3 was billed and thrown away before reaching the agent
loop. `ThoughtSignature` was never round-tripped either, which degrades
multi-turn function calling on Gemini 3. Both fixed; thinking is now emitted as
`ThinkingContentDelta` and signatures survive the round trip.

**3. Google adapter had no options at all.** No Vertex backend (no project,
location, or ADC), no thinking config, no temperature/topP/topK/seed/stop/
maxOutputTokens, no safety settings, no HTTP client. Added, including
`WithVertex`, both thinking-sizing forms, and search grounding with citation
extraction.

**4. Google errors were always permanent.** Every failure was classified
`ErrorKindPermanent`, so a 429 or 503 from Gemini defeated the retry and
fallback decorators entirely -- they act on transient errors by default. Now
classified from the HTTP status.

**5. Text deltas were emitted per chunk.** The Google adapter sent a full
Start/Content/End triple per network chunk, so downstream aggregators saw many
separate text blocks instead of one. Now bracketed across the stream like the
other adapters.

**6. Sub-agents silently dropped most of the parent's config.**
`registerSubAgent` passed only name, prompt, provider, tools, sub-agents,
max-iterations and step runner. A delegated child ran with **no LLM timeout, no
tool timeout, no metrics, no logger, no compaction, and no file resolvers or
extractors**, whatever the parent was configured with. Now everything
operational is inherited (`inheritConfig`), a nil provider inherits the
parent's, and `SubAgentDef.Options` is the per-child escape hatch.

**7. Handoff members could not inherit a provider.** A `HandoffDef` with no
provider was rejected outright, even though the entry agent had one to
inherit. Now inherited; the error is raised only when no provider exists
anywhere in the group, and names the entry agent as the real problem.

**8. OpenAI and Anthropic had almost no options.** OpenAI exposed only
`WithBaseURL` -- no max tokens, temperature, seed, stop, penalties, reasoning
effort, or parallel-tool control. Anthropic exposed only max tokens and
thinking. All added. The original implementation dropped unsupported knobs. Request validation now
rejects them before network I/O, so callers can see that their requested configuration was not
valid. Configure `gpt-4o`/`o3` fallback members separately.

**9. No MCP client existed.** The repo could *serve* MCP (`cmd/saige-mcp`) but
not *consume* it. `agent/mcp` adds both transports, with the differences made
explicit rather than hidden: a local server is a child process this package
owns and must `Close`, a remote one is a connection whose tool list can change
between calls and whose trust boundary is someone else's. `Pool` unwinds them
together and routes per-server gate policy.

**10. Citations existed only inside RAG.** `rag/types` had provenance and
context blocks; nothing at the agent or provider level. Provider-returned
grounding was dropped. Now unified through `CitationRegistry`, fed by tool
results, MCP resource links, sub-agent streams, and Gemini grounding metadata.

**11. Gating was all-or-nothing per tool.** Only `*types.MarkedTool` could
pause, only via a human round trip, and only for every call to that tool.
`ToolGate` adds per-argument decisions, programmatic denial, and argument
rewriting, running ahead of the existing marker path.

**12. No cost model or spending control.** Nothing costed a run, nothing
stopped one. `Pricing`/`Budget` add both, with the run-wide budget shared by
pointer into sub-agents -- a per-child copy would let a run with four
sub-agents spend four times its ceiling.

**13. No tool result caching.** `agent/provider/cache` memoized LLM responses;
tool calls had nothing, so a long run re-ran the same search and the same file
read repeatedly. `agent/toolcache` adds policy-driven caching with scope
isolation, context-varying keys, staleness bounds, and single-flight collapse.

### Still open

**A. Stale CLI defaults.** `cmd/saige/provider.go` still defaults Google to
`gemini-2.0-flash`, which the vendor has deprecated, and OpenAI to `gpt-4o`.
Left unchanged deliberately: changing a default model changes behaviour for
every existing user and is your call, not a side effect of this branch. The
catalog flags `gemini-2.0` as deprecated so `saige models` shows it.

**B. Anthropic and OpenAI server-side tools are declared but not wired.** The
capability table says both support web search and remote MCP, and
`types.ServerTool` describes them, but only the Google adapter translates them.
OpenAI's require the Responses API, which the adapter does not use. Until
wired, `ValidateServerTools` will pass and nothing will happen -- the one place
in this design where a declaration outruns the implementation.

**C. Cache accounting needs complete tariffs.** `UsageDelta` now reports cache
reads and writes. OpenAI, Anthropic, and Google populate those fields.
The budget separates cache tiers, but cache storage and TTL-specific write
prices still need a complete rate model. See [cache contracts](cache-contracts.md).

**D. Pricing coverage is incomplete.** Claude 5 and Gemini 3 rows are
deliberately unpriced rather than guessed. A `Budget` refuses to run against
them unless `AllowUnpriced` is set. Supply rates with `catalog.Register`, which
appends a revision.

**E. Tiered pricing is flattened.** Gemini 2.5 Pro charges more above a prompt
length threshold; the table declares the lower tier. Long-prompt runs will
under-count.

**F. `types.Cache` has no scope-aware eviction.** `toolcache` namespaces keys
by scope, but the backing store cannot evict a single tenant's entries.
`CachePolicy.MaxEntries` is declared and not yet enforced.

**G. Ollama capabilities cannot be verified.** They depend on the pulled
weights, and the daemon does not report whether a model supports tools or
vision. Unrecognised local models resolve to the baseline with `Known == false`;
there is no runtime probe.

**H. Server-tool calls are invisible to telemetry.** Provider-executed tools
emit no `ToolExec*` deltas, so a run that used web search five times looks
identical to one that used it none. Only citations hint at it.

## Verification

```
go build ./...
go vet ./...
go test ./...
```

All pass on this branch. New tests cover capability intersection, catalog
resolution and revisioning, decorator passthrough, sub-agent and handoff
inheritance, citation deduplication, gate composition, budget enforcement and
approval grants, cache policy and scope isolation, and MCP schema conversion
and per-server gating.
