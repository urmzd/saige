# Upgrading to this release

This release changes how messages, model output and media are represented, and unifies the constructors. Code that builds messages, reads the stream, writes a provider or decorator, or constructs adapters and stores needs edits; the compiler finds most of them. Data written by earlier releases (conversation trees, durable journals, caches, wire version 1 streams and version 1 catalogs) still reads.

- [Breaking changes](#breaking-changes)
- [Migration steps](#migration-steps)
- [Details by area](#details-by-area)
- [Earlier releases](#earlier-releases)

## Breaking changes

- **Messages are ordered typed parts.** Message fields are `Parts`, the content types are parts (`TextPart`, `ToolCallPart`, `ImagePart`, `DocumentPart`, ...), and media is a part with a `Source`. See [message parts](parts.md).
- **One provider method.** `Provider` is `Stream(ctx, Request)`; the `ChatStream` triplet is gone. A provider declares schema and options support with `SupportsSchema` and `SupportsOptions`.
- **Model output streams as part deltas** (`PartStart`, `PartDelta`, `PartEnd`). The text, thinking, tool-call and server-tool deltas are removed.
- **The wire format is version 2.** Version 1 envelopes still decode, and an encoder or `saige serve` still writes version 1 on request.
- **Constructors take a `Config` and return an error**, `Close` takes a context, and every name deprecated in earlier releases is removed.
- **Typed IDs and targets.** `ModelID`, `ProfileID`, `PresetName` and `ProviderName` replace strings; `WithTarget(Target)` replaces `WithModel(string)`.
- **The catalog file is version 2**: models, endpoints and offerings. Version 1 files load with a warning.
- **Media a model cannot take is rejected by default**, never dropped or replaced by a placeholder. Permit an action per modality to convert it.
- **Untrusted clients send media only as inline bytes, an `https` URL or a `saige-artifact://` reference.** `http` URLs and vendor file IDs are refused.
- **Stores write new formats** for node messages, durable steps and cache entries. There is no downgrade.

## Migration steps

Work through these in order.

### 1. Snapshot stored data

Nothing writes the earlier message format, and there is no downgrade. If you might roll back, snapshot the PostgreSQL database, file WALs, tree documents and durable journals first.

### 2. Rename symbols and fix constructors

Most renames are mechanical:

```sh
gofmt -r 'agent.AgentConfig -> agent.Config' -w .
gofmt -r 'agent.AgentOption -> agent.Option' -w .
gofmt -r 'types.ConfigContent -> types.ConfigPart' -w .
gofmt -r 'eval.ExperimentResult -> eval.Comparison' -w .
```

Constructors that now return an error need an edit at each call site; the compiler lists every one. A rename keeps the arguments unless the replacement shows them.

| Removed | Replacement |
| --- | --- |
| `agent.NewAgent(cfg, opts...) *Agent` | `agent.New(cfg, opts...) (*Agent, error)` |
| `agent.AgentConfig` | `agent.Config` |
| `agent.AgentOption` | `agent.Option` |
| `bind.Bound.NewAgent(opts...) *agent.Agent` | `bind.Bound.NewAgent(opts...) (*agent.Agent, error)` |
| `anthropic.NewAdapter(key, model, opts...)` | `anthropic.New(anthropic.Config{APIKey: key, Model: model}, opts...)`, which returns an error |
| `openai.NewAdapter(key, model, opts...)` | `openai.New(openai.Config{APIKey: key, Model: model}, opts...)`, which returns an error |
| `openai.NewResponsesAdapter(key, model, opts...)` | `openai.NewResponses(openai.Config{APIKey: key, Model: model}, opts...)`, which returns an error |
| `openai.NewEmbedder(key, model, opts...)` | `openai.NewEmbedder(openai.Config{APIKey: key, Model: model}, opts...)`, which returns an error |
| `google.NewAdapter(ctx, key, model, opts...)` | `google.New(ctx, google.Config{APIKey: key, Model: model}, opts...)` |
| `google.NewEmbedder(ctx, key, model, opts...)` | `google.NewEmbedder(ctx, google.Config{APIKey: key, Model: model}, opts...)` |
| `ollama.NewClient(host, model, embedModel, opts...) *Client` | `ollama.NewClient(ollama.Config{Host: host, Model: model, EmbeddingModel: embedModel}, opts...) (*Client, error)` |
| `ollama.NewAdapter(client, opts...) *Adapter` | `ollama.New(ollama.Config{Client: client}, opts...)`, or `ollama.New(ollama.Config{Host: h, Model: m}, opts...)`; both return an error |
| `retry.New(inner, cfg) *Provider` | `retry.New(inner, cfg, opts...) (*Provider, error)`; `retry.Provider{Inner: p}` literals become `retry.New` |
| `cache.New(inner, cfg) *Provider` | `cache.New(inner, cfg, opts...) (*Provider, error)`; `cfg.Cache` is required |
| `fallback.New(providers...) *Provider` | `fallback.New(fallback.Config{Providers: providers}, opts...)` or `fallback.Of(providers...)`, which return an error |
| `privacy.NewProvider(inner, vault)` | `privacy.New(inner, privacy.Config{Vault: vault}, opts...)`, which returns an error; `Media` and `AllowAudioOut` move into `privacy.Config` |
| `otel.NewTracedProvider(inner, tracer, opts...) *TracedProvider` | the same call, which returns an error |
| `convert.New(inner, policy, layers...)` | `convert.New(inner, convert.Config{Policy: policy, Layers: layers}, opts...)`, which returns an error |
| `convert.NewBatch(inner, policy, layers...)` | `convert.NewBatch(inner, convert.Config{Policy: policy, Layers: layers}, opts...)`, which returns an error |
| `router.New(cfg)`, `split.New(cfg)` | the same calls with optional `opts...`; errors wrap `types.ErrInvalidConfig` |
| `types.Provider.ChatStream`, `ChatStreamWithSchema`, `ChatStreamWithOptions` | `Stream(ctx, types.Request{Messages, Tools, Schema, Options})`, with `SupportsSchema() bool` and `SupportsOptions() bool` |
| `types.ModelSwitcher` | `types.TargetSwitcher` |
| `p.WithModel(m)` | `p.WithTarget(types.ModelTarget(m))`, which returns an error |
| `types.ProviderWithModel(p, m)` | `types.ProviderWithTarget(p, types.ModelTarget(m))` |
| `types.ProviderName(p)` (the function) | `types.NameOf(p)`; `types.ProviderName` is now the typed vendor name |
| `types.ConfigPart.Model` | `Target types.Target`, for example `types.PresetTarget("fast")` |
| `types.ContentNegotiator`, `ContentSupport()` methods, `types.ProviderContentSupport` | `convert.Target(p).Modalities`, or `ModelCapabilities.Offering` |
| `types.Closer.Close() error` | `Close(ctx context.Context) error` |
| `types.CloseProvider(p)` | `types.CloseProvider(ctx, p)` |
| `types.Dep[T](deps, "name")` | `types.Dep(deps, types.NewKey[T]("name"))` |
| `FallbackError.Unwrap() []error` | `FallbackError.Unwrap() error`, the last attempt; all attempts stay in `Errors` |
| `pgstore.NewStore(pool, conv, logger)` (agent) | `pgstore.New(pgstore.Config{Pool: pool, ConversationID: conv, Logger: logger})`, which returns an error |
| `pgstore.NewScopedStore(pool, scope, conv, logger)` (agent) | `pgstore.New(pgstore.Config{Pool: pool, Scope: scope, ConversationID: conv, Logger: logger})` |
| `pgstore.NewStore(pool, logger, opts...)` (rag) | `pgstore.New(pgstore.Config{Pool: pool, Logger: logger}, opts...)`, which returns an error; an invalid search option fails here instead of on every search |
| `pgstore.NewStore(pool, logger, opts...)` (knowledge), `pgstore.StoreOption` | `pgstore.New(pgstore.Config{Pool: pool, Logger: logger}, opts...)`, `pgstore.Option` |
| `pgstore.New(pool, cfg)` (memory) | `pgstore.New(cfg, opts...)` with `cfg.Pool` set |
| `pgstore.New(ctx, pool, tenant)` (eval) | `pgstore.New(ctx, pgstore.Config{Pool: pool, Tenant: tenant})` |
| `filewal.New(path)` | `filewal.New(filewal.Config{Path: path})` |
| `batch.NewRunner(p, store, opts...) *Runner` | `batch.NewRunner(batch.RunnerConfig{Provider: p, Store: store}, opts...) (*Runner, error)` |
| `rag.NewPipeline(opts...)` | `rag.New(rag.Config{}, opts...)`; set fields on the `Config` or pass options |
| `knowledge.NewGraph(ctx, opts...)` | `knowledge.New(knowledge.Config{}, opts...)` |
| `harness.Runner{...}` literal | `harness.New(harness.Config{...}, opts...) (*Runner, error)`; a `Client` is required |
| `mcp.Client.Close()`, `mcp.Pool.Close()`, `bind.Bound.Close()` | `Close(ctx)` |
| `notify.Hub.Close()` | `notify.Hub.Close(ctx) error` |
| `agui.Mapper.Close() []Event` | `agui.Mapper.Flush() []Event` |
| `UserMessage.Content`, `AssistantMessage.Content`, `SystemMessage.Content` | `.Parts` |
| `types.TextContent`, `ToolUseContent`, `ThinkingContent`, `ToolResultContent` | `types.TextPart`, `ToolCallPart`, `ThinkingPart` (text field `Text`), `ToolResultPart` (ID field `CallID`) |
| `types.SystemContent`, `UserContent`, `AssistantContent` | `types.SystemPart`, `UserPart`, `AssistantPart` |
| `types.NewSystemMessage(s)`, `NewUserMessage(s)`, `NewAssistantMessage(s)` | `types.SystemMsg(types.Text(s))`, `types.UserMsg(types.Text(s))`, `types.AssistantMsg(types.Text(s))` |
| `types.NewToolResultMessage(rs...)`, `NewUserToolResultMessage(rs...)` | `types.ToolResults(rs...)`, `types.UserToolResults(rs...)` |
| `types.NewFileMessage(uri, mt)` | `types.UserMsg(types.Media(types.URL(uri, mt)))` |
| `types.NewUserMessageWithFiles(text, files...)` | `types.UserMsg` with a `types.Text` part followed by `types.Media` parts |
| `types.ConfigContent`, `RouteContent`, `SteerContent`, `TruncationContent`, `HandoffContent`, `FeedbackContent`, `ApprovalContent`, `GuardrailContent`, `CompactionContent` | `types.ConfigPart`, `RoutePart`, `SteerPart`, `TruncationPart`, `HandoffPart`, `FeedbackPart`, `ApprovalPart`, `GuardrailPart`, `CompactionPart` |
| `types.RouteContentFrom(d)` | `types.RoutePartFrom(d)` |
| `types.IsMetadataContent(c)` | `types.IsMetadata(p)` |
| `types.FileContent{URI, MediaType, Data, Filename}` | `types.Media(types.Bytes(mt, data).With(types.Source{URI: uri, Filename: name}))`, or a `Source` |
| `ToolResult.Text` (field), `ToolResultPart.Text` (field) | `ToolResult{Parts: ...}` or `TextResult(s)`; read `r.Text()` |
| `types.ToolResultBlock`, `ToolResultBlockKind`, `ToolResultBlockText`, `ToolResultBlockImage`, `ToolResultBlockFile`, `ToolResultBlockJSON` | `types.ToolOutputPart`: `types.Text`, `types.JSONPart`, `types.Image`, `types.Document`, `types.Audio`, `types.File` |
| `types.TextStartDelta`, `TextContentDelta`, `TextEndDelta`, `ThinkingStartDelta`, `ThinkingContentDelta`, `ThinkingEndDelta`, `ToolCallStartDelta`, `ToolCallArgumentDelta`, `ToolCallEndDelta`, `ServerToolCallDelta`, `ServerToolResultDelta` | `types.PartStart`, `types.PartDelta`, `types.PartEnd` |
| `types.UpgradeV1Stream(ch)` | `types.NewDecoder().Decode(env)` per envelope, or `types.NewV1Upgrader()` per decoded delta |
| `eval.RunExperiment(...)` | `eval.Compare(...)`; pass `eval.WithName("experiment")` to keep the old default name |
| `eval.ExperimentResult`, `ExperimentConfig`, `ExperimentOption` | `eval.Comparison`, `eval.Config`, `eval.Option` |
| `eval.WithExperimentName`, `WithExperimentLogger` | `eval.WithName`, `eval.WithLogger` |
| `eval.WithRunOptions(opts...)` | pass the options directly |
| `eval.WriteExperiment`, `ReadExperiment` | `eval.WriteComparison`, `eval.ReadComparison` |
| `harness.Experiment`, `harness.FilterExperiments` | `harness.Script`, `harness.FilterScripts` |
| `catalog.Lookup(string, string)` and the string IDs of presets, routers and `provider.Config` | `types.ProviderName`, `types.ModelID`, `types.ProfileID`, `types.PresetName`: `catalog.MustLookup(types.ProviderName(name), model)` |

### 3. Move message and stream code to parts

- Build messages with `types.UserMsg(types.Text("hi"), types.Image(types.Bytes(types.MediaPNG, b)))` and friends. Replace `FileContent{URI: u}` with `types.Media(types.URL(u, mediaType))`.
- Read model output from `PartDelta.Text` and finished parts from `PartEnd.Part`, or rebuild a turn with `types.NewPartAssembler()`. Key any per-part state by `Index`: deltas for different parts may interleave.
- Return tool output as parts: `types.ToolOK(id, types.Text(s))`, `types.TextResult(s)`, or media parts for a tool that returns images or documents.
- Keep `ThinkingPart`s, including empty signed ones, in stored turns: vendors need them back.

### 4. Update providers and decorators you wrote

- Implement `Stream(ctx, types.Request)` and emit part deltas (`types.PartDeltas` and `types.MessageDeltas` build a well-formed sequence from finished parts). Add `SupportsSchema` and `SupportsOptions` when the provider applies `Request.Schema` or `Request.Options`.
- Implement `WithTarget(types.Target) (types.Provider, error)` instead of `WithModel`, and `Close(ctx)`.
- Embed `wrapper.Base` in a decorator (`wrapper.NewBase(inner, rewrap)`) and override only what it changes; it forwards every optional interface.
- Use `wrapper.As[*anthropic.Adapter](p)` instead of a type assertion on what `provider.Build` returns: the adapter now sits behind a conversion decorator.

### 5. Stored data

Nothing is required. Trees, `pgstore` rows, `filewal` logs, durable journals and cache entries written by earlier releases read unchanged; caches miss once and record again.

- `saige tree migrate TARGET --write` rewrites older node messages in place. It is optional; stop writers of a WAL while it runs.
- Attach a workspace (`agent.WithWorkspace`) so media bytes in a conversation survive a reload. Stores never hold bytes; without a workspace a stored media part keeps only its digest and size.
- Run `postgres.RunMigrations` before the rollout, as for every release.

### 6. Wire clients

- Upgrade readers before producers. A reader older than this release rejects the new kinds with `ErrUnknownWireKind`.
- A client that reads only version 1 asks `saige serve` for it with `?wire=1` or `Accept: application/vnd.saige.events+json;v=1`, or you write it with `types.NewEncoder(types.EncodeOptions{Version: 1})`. Output with no version 1 form arrives as an `error` event with code `wire_unrepresentable`.
- Send turns as `{"parts": [...]}`; `{"message": "..."}` still works and is deprecated. Upload media over 256 KiB with `POST /v1/sessions/{sid}/artifacts` and send the ref it returns. Download produced media from `GET /v1/sessions/{sid}/artifacts/{sha256}`.
- AG-UI clients handle the `CUSTOM` events `saige.media` and `saige.refusal`.

### 7. Catalogs

- Run `saige catalog migrate --write <file>` on each catalog layer you keep. Version 1 files still load, with a `v1_catalog` warning.
- Batch rates move from `pricing.batch_discount` to the batch service tier (`tiers.batch`); `migrate` moves them.
- Chain entries may name an offering: `"offering": "<vendor>/<model>@<endpoint>"`. The `provider` and `model` form still works.
- gpt-5.6 and gpt-6 rows declare prompt cache retention `24h` only. Drop `in_memory` retention on them.

### 8. Behavior changes to check

| Change | What to do |
| --- | --- |
| A media part the serving model cannot take is rejected with `types.ErrModalityUnsupported` that names the part. Before, the agent sent a notice such as `[file ... could not be loaded]` and adapters dropped the part or sent `[File: name]` text. | Permit an action: `omit` keeps the notice behavior, `extract` sends a document's text. See [modality conversion](modality-conversion.md#configuring). |
| Client media sources are checked by one rule, `types.CheckClientSource`: inline bytes, an `https` URL, or a `saige-artifact://` reference. `http` URLs, vendor file IDs, other schemes and bare names are refused (`types.ErrUntrustedLocator`) by `saige serve`, `saige acp` and `agenthost.ClientParts`. | Send `https` URLs, or upload the media and send its ref. Call the check where untrusted parts enter your own host. |
| `FallbackError` unwraps to its last attempt only. | Read `FallbackError.Errors` for earlier attempts. |
| Every exported sentinel keeps its identity across the wire, so `errors.Is` holds on a decoded error. | Register your own sentinels with `types.RegisterWireSentinel` if they cross the wire. |
| Cache keys and entries use new versions, and a converted view never shares an entry with the original. | None. Entries miss once. |
| Usage of a modality the rate card does not price makes the call unpriced (`types.ErrUnpriced` under a cost limit). | Declare `modality_pricing` for the model, or set `AllowUnpriced`. |
| `Pricing` and `TokenUsage` hold maps and are no longer comparable with `==`. | Compare them with `reflect.DeepEqual`. |

## Details by area

Each table says what changed and what to do.

### Messages, streams and providers

Messages are ordered lists of typed parts, model output streams as part deltas, and providers take one request value.

| Change | What to do |
| --- | --- |
| Message fields are named `Parts` (`UserMessage.Parts`, and so on), and the content types are parts: `TextContent` is `TextPart`, `ToolUseContent` is `ToolCallPart`, `ThinkingContent` is `ThinkingPart` (its text field is `Text`), `ToolResultContent` is `ToolResultPart` (its ID field is `CallID`), and the role interfaces are `SystemPart`, `UserPart` and `AssistantPart`. The metadata types are `ConfigPart`, `RoutePart`, `SteerPart`, `TruncationPart`, `HandoffPart`, `FeedbackPart`, `ApprovalPart`, `GuardrailPart` and `CompactionPart`; the `*Content` aliases are removed (see [step 2](#2-rename-symbols-and-fix-constructors)). | Rename the fields and types. Build messages with `UserMsg(Text("hi"))`, `SystemMsg`, `AssistantMsg`, `ToolResults` and `UserToolResults`; the string constructors such as `NewUserMessage` are removed. |
| Media is a part with a `Source`: `ImagePart`, `AudioPart`, `VideoPart`, `DocumentPart` and `FilePart`, built with `Image`, `Document`, `Media` and the like from `Bytes`, `URL`, `Artifact` or `VendorFileID`. `FileContent` is no longer a message part. | Replace `FileContent{URI: u}` with `Media(URL(u, mediaType))`. `FileContent` is removed. |
| A tool result holds `Parts []ToolOutputPart` (text, JSON, images, documents, audio, files). `ToolResult.Text` and `ToolResultPart.Text` are methods that join the text and JSON parts. `ToolResultBlock` is removed. `ToolExecEndDelta`, `AfterToolEvent` and `StepResult` carry parts instead of blocks. | Return `ToolResult{Parts: []types.ToolOutputPart{types.Text(s)}}` or `TextResult(s)`. Read `r.Text()` instead of `r.Text`. |
| `Provider` has one method, `Stream(ctx, Request)`. `Request` carries the messages, tools, an optional `Schema` and optional `Options` (`*RequestOptions`). `ChatStreamWithSchema` and `ChatStreamWithOptions` are gone; a provider declares that it applies a schema or options with `SupportsSchema() bool` and `SupportsOptions() bool`, and `types.AcceptsSchema` and `types.AcceptsOptions` test for them. `Generate(ctx, prompt)` stays as a text convenience. | Rename `ChatStream` to `Stream` and read `req.Messages` and `req.Tools`; fold the schema and options methods into it and add the matching `Supports` method. The built-in adapters apply a schema and options sent together. |
| Model output streams as `PartStart`, `PartDelta` and `PartEnd`, each with an `Index` that is the part's position in the final message. Deltas for different parts may interleave. A tool call's `PartEnd` carries the complete `ToolCallPart`. Server tool calls and their results are separate parts (`PairServerTools` joins them), and model citations arrive as `CitationPart` parts, numbered by the agent and stored with the turn. The text, thinking, tool-call and server-tool deltas of the previous release are removed; a version 1 envelope still decodes, through `types.NewDecoder`. | Switch consumers to the part deltas: read `PartDelta.Text` for text and `PartEnd.Part` for finished parts, or build the turn with `types.NewPartAssembler`. Emit part deltas from a provider. |
| The wire format is version 2: model output uses the kinds `part.start`, `part.delta` and `part.end`, and media bytes in one field are limited to 256 KiB (`ErrWireInlineTooLarge`). Version 1 envelopes still decode. | Use `types.NewEncoder(types.EncodeOptions{Version: 1})` for a client that reads only version 1. Output with no version 1 form, such as a refusal or a generated image, becomes an error envelope with the code `wire_unrepresentable`. |
| The response cache and the tool cache store entries in a new format (codec version 2). | None. Entries written by an earlier release miss and are recorded again. |
| Durable engines record steps, run input and final messages in a new versioned format (`types.StepFormatVersion` 2), with messages and parts in the shared part codec. Journals an earlier release wrote still decode and are upgraded on replay. | None. In-flight runs resume across the upgrade. |

### Constructors, decorators and errors

Constructors take a `Config` and options and return an error, decorators share one base, errors keep their identity across the wire, and every name deprecated in earlier releases is removed. Stored data is unaffected: trees, journals, caches and wire version 1 envelopes written by earlier releases still read.

| Change | What to do |
| --- | --- |
| Every constructor in the [rename table](#2-rename-symbols-and-fix-constructors) validates its configuration and returns an error wrapping `types.ErrInvalidConfig` instead of panicking or failing on first use. `agent.New` returns that error for a negative `LLMTimeout`, `ToolTimeout` or `InterruptTTL` and for an invalid handoff group, where `NewAgent` panicked. | Handle the error. Tests can use a small helper that fails the test on it. |
| A decorator embeds `wrapper.Base`, which forwards every optional provider interface (name, model, capabilities, effective options, schema and options support, `WithTarget`, `NewSession`, `Close`) and rebuilds the decorator around a re-targeted or isolated provider. `wrapper.Describe(p)` reports what a provider stack declares in one `ProviderInfo`. `wrapper.NoClose(p)` lends a provider to a second owner. The attempt deadline of a preset entry now forwards `WithTarget`, so a model switch reaches an entry built with `attempt_timeout`. | Embed `wrapper.Base` in your own decorators and override only what they change; build it with `wrapper.NewBase(inner, rewrap)`. |
| `types.Closer.Close` takes a context, and so does every `Close` of a type that owns resources: providers and decorators, `router.Router`, `split.Split`, `preset.Bundle`, `mcp.Client`, `mcp.Pool`, `bind.Bound`, `filewal.WAL`, `notify.Hub`, `notify.Memory`, `notify.Cache` and `postgres.Notifier`. `types.CloseProvider` takes the context first. | Pass a context; it bounds waits such as an MCP server shutting down. |
| `FallbackError` unwraps to its last attempt only. `errors.Is`, `errors.As`, `KindOf`, `IsTransient` and `RetryAfter` all answer for the attempt that ended the chain; an earlier rate-limited attempt no longer makes the failure match `ErrRateLimited`. `FallbackError.Errors` still lists every attempt. | Read `Errors` to inspect earlier attempts. |
| `KindOf` and `IsTransient` read the first `types.KindReporter` (`ErrorKind() ErrorKind`) in an error's chain; `ProviderError`, `RemoteError`, `ResponseTruncatedError`, `FallbackError` and `RetryError` implement it. `RetryAfter` finds a `ProviderError` behind any wrapper, including a decoded error. | Implement `KindReporter` on your own error types to classify them. |
| Every exported sentinel error in the module has a stable wire code (`types.RegisterWireSentinel`, `types.WireSentinels`), so `errors.Is` holds after an error crosses a process boundary, for example `agent.ErrToolErrorLimit`, `agent.ErrHandoffLimitExceeded` or `router.ErrNoEligibleProfile`. `types.EncodeError` and `types.DecodeError` expose the codec as `types.EncodedError`. | Register your own sentinels from `init` if they cross the wire. |
| The router reports a request no profile can serve as `router.ErrNoEligibleProfile` (joined with each candidate's reason), and a policy that names an undefined profile as `router.ErrUnknownProfile`. | Match the sentinels instead of the message text. |
| `ModelSwitcher`, `WithModel(string)` and `ProviderWithModel` are removed. Every adapter and decorator implements `TargetSwitcher`. `types.TargetModel` is the rule for a provider that serves one model. | Call `p.WithTarget(types.ModelTarget(m))` or `types.ProviderWithTarget(p, types.ModelTarget(m))`, and implement `WithTarget(types.Target) (types.Provider, error)` instead of `WithModel`. |
| Interrupt payloads are typed: `types.ClarificationPayload`, `Interrupt.Clarification`, `Interrupt.ToolCall`, `types.InterruptPayload[T]`, `types.ReplyAnswer[T]`, `types.Answer` and `types.ModifiedArgsAs[T]`; a payload of the wrong shape is `types.ErrInterruptPayload`. The wire form is unchanged. | Replace hand-written JSON decoding of `Payload` and `Answer`. |
| `types.Key[T]` names a typed tool context knob or dependency: `NewKey`, `Get`, `Put`, `WithDep` and `Dep` take one. A zero `types.ToolRegistry` is ready to use. | Replace `Dep[T](deps, "name")` with `Dep(deps, nameKey)` where `nameKey = types.NewKey[T]("name")`. |
| `agui.Mapper.Close` is `Flush`: it returns the closing events of an unfinished run and owns no resource. | Rename the call. |

### Catalog offerings and typed IDs

| Change | What to do |
| --- | --- |
| The catalog file format is version 2: `models` (keyed `"<vendor>/<prefix>"`, holding tier, limits and modalities), `endpoints` (surface, auth by secret reference, location, transport, capacity, data handling, files, batch mode, model ID mapping, overrides), `offering_templates` and `offerings` (a model on an endpoint: features, `params` with ranges and defaults, keyed `constraints`, `modalities` with limits and source forms, pricing, service `tiers`, fallback hints, dials). Version 1 files still load through `catalog.UpgradeV1`, with a `v1_catalog` warning. | Run `saige catalog migrate --write <file>` on each catalog layer you keep. Code that built `catalog.ModelSpec` rows uses `ModelSpec` (model facts) and `OfferingSpec` (request knobs) now; the version 1 types are `CatalogV1` and `ModelSpecV1`. |
| Batch pricing moved from `pricing.batch_discount` and `batch_cached_input_per_mtok` to the batch service tier: `"tiers": {"batch": {"transport": "batch", "discount": 0.5, "cached_input_per_mtok": ...}}`. A batch tier needs the endpoint's `modes.batch`. `ModelCapabilities.Pricing` still reports the batch fields. | Move hand-written batch rates under `tiers.batch`; `migrate` does it for you. |
| `ModelCapabilities.Offering` carries the offering a declaration came from: the parameter space (`Space().Validate`), modality limits, tiers and endpoint. `ModelCapabilities.Intersect` intersects offerings too. `catalog.LookupOffering`, `Catalog.Offering` and `Catalog.ResolvedOfferings` read offerings on any endpoint. | None. New code reads the offering for precise limits. |
| Chain entries accept `"offering": "<vendor>/<model>@<endpoint>"` or `"endpoint"` with `"model"`. The legacy `provider`/`model` form still works: `vertex` selects the `google-vertex` endpoint, and `base_url` or `api_key_env` an endpoint named `<preset>/<id>`. `ResolvedEntry` gains `Endpoint` and `Offering`. | None. An entry on a non-primary endpoint has a different `ConfigHash`. |
| `gpt-6-luna` and `gpt-6.1-sol` declare prompt cache retention `24h` only: a preset entry or `provider.Config.PromptCache` with retention `in_memory` is rejected before any request. | Drop the retention or set `24h`. |
| `qwen3.5` and `qwen3.5:4b` declare inline JPEG and PNG input. | None. |
| Typed IDs: `types.ProviderName`, `types.ModelID`, `types.ProfileID` and `types.PresetName` replace strings in the catalog, preset, router and `provider.Config` APIs. `catalog.Lookup`, `MustLookup` and `Describe` take a `ProviderName` and any string-kinded model. The function `types.ProviderName(p)` is now `types.NameOf(p)`. | Convert at the boundary, for example `catalog.MustLookup(types.ProviderName(name), model)`. |
| `types.ConfigPart.Model` is replaced by `Target types.Target` (exactly one of `Model`, `Profile`, `Preset`). Stored records with `"Model"` read as model targets. `types.TargetSwitcher` (`WithTarget(Target) (Provider, error)`) and `types.ProviderWithTarget` report an unknown target as an error matching `types.ErrUnknownTarget`; the router session implements it in place of `WithModel`, and the retry, cache, otel, privacy, fallback and split decorators forward it. | Write `types.ConfigPart{Target: types.PresetTarget("fast")}` instead of `{Model: "fast"}`. A turn whose target the router does not define now fails instead of failing on the next request. |
| `preset.Build` takes `types.PresetName`s, and `Bundle.ConfigKey` takes a `types.Target`. Router `Profile.ID`, groups, policies and `RouteState` profiles are typed. | Convert names at the call site. |
| `catalog.RegisterSurface` records what an adapter can send on an endpoint surface; without a registration the built-in rules apply. | None. |

### Modality conversion

A part the serving model cannot take natively is now fitted to it per attempt by the [conversion policy](modality-conversion.md), and is rejected unless you permit an action.

| Change | What to do |
| --- | --- |
| A media part the serving model cannot take is rejected with an error matching `types.ErrModalityUnsupported` (and `types.ErrInvalidModelConfig`) that names the part. Before, the agent replaced a part it could not load or extract with a text notice such as `[file ... could not be loaded: ...]`, and adapters dropped or rejected the rest. | Permit an action for the modality: `omit` keeps the old notice behavior (`types.Dials{Modality: &types.ModalityDial{Default: types.ActOmit}}` with `agent.WithDials`), and `extract` sends a document's text (`agent.WithConversion` with `convert.Documents()` and `{document: [extract]}`). See [Configuring](modality-conversion.md#configuring). |
| `agent.WithExtractors` registers an extract converter per media type and permits `extract` for their modalities. An extractor that fails rejects the request instead of sending a notice. | None. Add `omit` after `extract` (`{document: [extract, omit]}`) to send a notice when extraction fails. |
| A `gs://` URI (or another scheme with no `Resolver`) is no longer turned into a "no resolver" notice. It is sent to an endpoint that reads it, such as Vertex AI, and planned as not native elsewhere. A resolver error marks the source unavailable (`types.ErrMediaUnavailable`). | None, or register a `Resolver` for the scheme so the bytes can be sent inline. |
| `types.ContentNegotiator`, the `ContentSupport()` methods of the adapters and decorators, and `types.ProviderContentSupport` are removed. `ModelCapabilities.Media` remains as a projection of the offering's input modalities. | Read `convert.Target(p).Modalities` (or `ModelCapabilities.Offering`) for what a model takes, with its limits and locators. |
| `provider.Build` returns the adapter behind a conversion decorator (`convert.Provider`), which reports the adapter's name, model and capabilities. `provider.Config.Conversion` sets its policy. | Use `wrapper.As[*anthropic.Adapter](p)` (or the adapter's interface) instead of a type assertion on the result. |
| `types.Dials` has a `modality` dial. It is spent by the conversion decorator and never reaches an adapter, so it needs no request options. The agent sends it outside the request options. | None. |
| A router removes a member whose conversion plan rejects the request, with that reason, and plans again on failover. A fallback chain moves past a member that rejects the request's parts. `RouteDelta.Conversions`, `types.ConversionDelta` and `RoutePart.Conversions` report what was planned and done. | None. |
| The agent records the provider that produced each reasoning part (`ThinkingPart.Origin`) and leaves reasoning signed by another provider, or unsigned, out of requests to the Anthropic Messages API. | Set `ConversionPolicy.Thinking` to `types.ThinkingAsText` to send readable reasoning as text instead. |
| A conversion runs as a durable step (`types.StepKindConvert`, named `convert:<digest>:<converter>@<version>`), and an LLM step records the receipts of the conversions it ran (`StepResult.ConversionReceipts`). A budget with a per-call bound reserves the conversions' estimate with the call (`Budget.ReserveWith`), and each conversion takes its share (`Budget.Carve`). | None. |

### Privacy and the response cache over parts

| Change | What to do |
| --- | --- |
| `privacy.Provider` and `ToolRedactor` tokenize text-bearing media: a `DocumentPart` or `FilePart` of `text/*`, CSV or JSON with inline bytes. The sent copy holds only the tokenized bytes, with their own digest; its URI, workspace reference and vendor uploads are dropped, because they reach the original text. Refusals, citation quotes and audio transcripts are tokenized and restored too. | None. |
| `privacy.Provider.Media` governs media the vault cannot tokenize: `pass` (the default), `refuse` (the default when the vault is `Sensitive`, set with `MemoryVault.SetSensitive`) and `require_text`. A refused request fails with `privacy.ErrMediaRefused` and is not sent. Under a `Sensitive` vault, audio output ends the stream with `privacy.ErrAudioOutRefused` unless `AllowAudioOut` is set. | Set `Media: privacy.MediaPass` on a sensitive vault that must still send images. |
| The privacy decorator sets a `types.Egress` boundary on the context of its calls. The conversion decorator tokenizes a converter's text output with it, and under `require_text` rejects a view that still carries media and a model converter whose endpoint lacks `data.pii_ok`. `types.ConvertEnv` gains `Vault`. | None. |
| Streamed restoration is kept per part index and field, so interleaved text, argument, refusal and transcript fragments never share held text. | None. |
| A conversion of a part reached only by a URI is memoized only under a named `ConversionPolicy.Scope`. Parts with a digest are memoized as before. | Set `Scope` to keep memoizing URI-only parts. |
| `types.CheckClientSource` and `types.CheckClientParts` refuse locators an untrusted client may not supply: vendor files, `gs`, `s3` and `file` URIs, bare file names, and URLs on vendor file stores (D-47). | Call them where parts from a client enter your host. |
| The response cache key is version 2 (`cache.KeyVersion`). It hashes every part, media by digest rather than locator, and the planned conversion report (`cache.KeyWithConversions`), so a converted view and the original never share an entry. Entries written by earlier releases miss once. | None. |

### Agent loop: estimates, compaction, citations, paused turns and batches

| Change | What to do |
| --- | --- |
| `types.EstimateTokensFor(messages, offering)` prices media by the offering's token rules (image tiles, pixels or a flat count, document pages, audio and video seconds), capping image pixels at the modality's `MaxPixels`. Text documents and files count their characters. `types.EstimatingTokenizer` has an `Offering` field, and the loop's input-pressure estimate uses the active provider's offering. `EstimateTokens` keeps the flat 1000 tokens per media part. Server tool calls and results now count toward the estimate. | None. Declare `tokens` rules on an offering's modalities to price media precisely. |
| `UsageDelta` has `PromptByModality` and `CompletionByModality`, filled from Gemini's token details and OpenAI Chat Completions' text, image and audio token counts, and `Requests` for usage that covers several vendor requests. The wire codec carries them; usage envelopes written before decode with nil maps. | None. |
| A compaction summary names each media part by a reference line (`[image png 1024x768 sha256:9f1c2b7a4e01… saige-artifact://…]`) and never sees its bytes or vendor file IDs. `types.MediaReference` renders the line. `types.MessagesToText` also renders server tool calls and results, refusals and generated media. | None. |
| A rich tool's citations are on `ToolExecEndDelta.Citations` and `ToolResultPart.Citations`, numbered by the run's registry. They are recorded with the tool step (`StepResult.ToolCitations`), so a durable replay streams and stores them too. `CitationDelta` is now sent after the tool step finishes. | None. |
| The Anthropic adapter numbers parts in the order their content blocks start and sends a text block's citations right after the block, instead of at the end of the response. A stream that fails later keeps them. Batch results order citations the same way. | Read citations by their anchor, not by their position at the end of the turn. |
| A turn the Anthropic API pauses (`pause_turn`) is continued: the adapter sends the partial response back and streams the rest of the turn, up to 8 continuations. Usage covers every request, with `Requests` set. The paused-turn error remains for a turn that stays paused. | None. |
| Batch requests are planned against the serving offering at submit. `agent.BatchProviderFor` and `RunBatch` put the vendor batch provider behind `convert.Batch`, which converts each request with the agent's policy and rejects the whole batch, before upload, when a request's media cannot be served. `convert.NewBatch` and `(*convert.Provider).Batch` wrap a batch provider directly. | Permit an action (such as `extract`) for media you batch, or expect `types.ErrModalityUnsupported` from `RunBatch`. |

### Citations, stored media, client locators and modality pricing

| Change | What to do |
| --- | --- |
| A tool result that carries citations reaches the model led by its sources and their markers (`[1] Title <uri>`), so the model can cite them as `[1]` and the `cites` scorer resolves the marker. The stored result and `Execute`'s text are unchanged. | None. |
| A media part whose only bytes are a `saige-artifact://` reference, as a stored conversation reads back, gets its bytes from the agent's workspace, or else from a `Resolvers["saige-artifact"]` resolver, before the request is planned. A reference neither holds, or whose bytes do not match its digest, marks the part unavailable, and the conversion plan rejects it (`types.ErrMediaUnavailable`) or omits it when the dial permits. Committed bytes whose reference the workspace does not hold are stored in it. | Attach the workspace the conversation was stored with, or register a resolver for the host's artifact store. |
| `types.CheckClientSource` is the one rule for client locators: inline bytes, an `https` URL, or a `saige-artifact://` digest reference. `http` URLs and any other reference are refused (`types.ErrUntrustedLocator`). `agenthost.ClientParts`, and so `saige serve` and `saige acp`, apply it and keep their codes (`ErrVendorFile`, `ErrURIScheme`, `ErrArtifactNotFound`). | Send `https` URLs, or upload the media and send its ref. |
| `types.Pricing.Modal` prices non-text modalities, projected from an offering's `modality_pricing`, and `types.TokenUsage` carries `InputByModality` and `OutputByModality` from the usage the vendor reports. Settlement bills each priced modality at its rate. Usage of a modality the card does not price is recorded at the text rates as an uncertain lower bound and is an unpriced call: `types.ErrUnpriced` under a cost limit unless `AllowUnpriced`, after the paid turn is kept. The default catalog prices modalities for the Gemini rows whose pricing page states them; OpenAI chat rows report no per-modality usage for images, and declare none. `Pricing` and `TokenUsage` are no longer comparable with `==`. | Compare them with `reflect.DeepEqual`. Declare `modality_pricing` for a model whose vendor reports audio, video or document usage, or set `AllowUnpriced`. |
| `router.Candidate.EstimatedTokens` is the prompt estimate priced by the candidate's offering, and `RouteContext.EstimatedTokens` is the largest of them. `router.Affinity` fits each candidate by its own estimate. | None. |
| `gpt-5.6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-6-sol` and `gpt-6-astra` declare prompt cache retention `24h` only, as the API rejects `in_memory` for them. | Drop the retention or set `24h`. |
| The TUI filter badge counts only the entries the template draws. | None. |

### Stored trees and durable journals

Node messages and durable records are stored as typed parts. Everything an earlier release stored stays readable.

| Change | What to do |
| --- | --- |
| Tree documents are format version 2 (`tree.TreeFormatVersion`). Each node message names its own format: `{"v":2,"parts":[...]}` in the shared part codec (`tree.MessageFormatVersion`). `tree.UnmarshalMessage` reads both this and the earlier `{"content":[...]}` form, so trees, `pgstore` rows and `filewal` logs migrate on read. A server tool block reads as a `ServerToolCallPart` and a `ServerToolResultPart`; a tool result's blocks read as parts, and media whose bytes were never stored reads as elided, which adapters reject. | None. A host that reads stored messages by hand should call `tree.UnmarshalMessage`. |
| Nothing writes the earlier message format, and there is no downgrade. | Snapshot the database, WAL files and tree documents before upgrading if you might roll back. |
| `saige tree migrate TARGET` counts, and with `--write` rewrites, older node messages in a PostgreSQL database (every conversation), a tree document or a file WAL. `pgstore.MigrateAllMessages`, `Store.MigrateMessages` and `filewal.WAL.MigrateMessages` do the same from code; `tree.MigrateMessage` rewrites one message. Rewriting is optional. | Stop writers of a WAL while it is rewritten. |
| With a workspace attached (`WithWorkspace`), the agent stores the bytes of a committed message's media in it and records a `saige-artifact://` reference and the digest on the source, so a stored conversation keeps its media reachable. Without one, the stored source keeps its digest and size only. | Attach a workspace to keep media across a reload. |
| `types.StepResult.V` reports the format a durable step was read from: 2, or 1 for a step an earlier release recorded. Steps keep their media bytes on replay. | None. |

### Serve, AG-UI, ACP and the TUI

`saige serve` takes and streams typed parts, the AG-UI mapping reports media and refusals, and the TUI renders every part kind.

| Change | What to do |
| --- | --- |
| `POST /v1/sessions/{sid}/turns` takes `{"parts": [...]}` in the shared part codec. `{"message": "..."}` still works and is deprecated. | Send `parts`. |
| The event stream writes wire version 2 (`part.start`, `part.delta`, `part.end`) by default and sets `Saige-Wire-Version`. | A client that reads only version 1 adds `?wire=1` or `Accept: application/vnd.saige.events+json;v=1`. It then gets the version 1 kinds, and an `error` event with code `wire_unrepresentable` for output with no version 1 form. |
| Media over 256 KiB in a run's output is stored in the session and sent as its `saige-artifact://` ref instead of inline bytes; a streamed data chunk over the limit is split. | Download refs from `GET /v1/sessions/{sid}/artifacts/{sha256}`. Version 1 clients find the ref in the block's `uri`. |
| `POST /v1/sessions/{sid}/artifacts` uploads media and returns its ref. Turn parts may carry inline `data` up to 256 KiB, an `https` URI, or a ref. Vendor file IDs, other URI schemes, tool results and unknown refs are refused with a `400` and a `code`. | Upload large media and send its ref. Never send provider file IDs. |
| AG-UI: produced media and media in a tool result are `CUSTOM` events named `saige.media` (`agui.MediaEventName`, value `agui.Media` with a `url`, or `data` when there is no link); a refusal is `saige.refusal` (`agui.Refusal`). `agui.NewMapper` takes options (`agui.WithMediaLink`). | Handle the two event names; other parts are unchanged. |
| `saige acp` passes prompt parts through the same checks as serve (without the inline limit): an image URI must be `https`, an `http` resource link stays a text reference, and an embedded blob is sent as bytes named by its file name rather than by its `file://` URI. Produced media and tool media without inline bytes are sent as resource links, other inline media as embedded blobs. | None. |
| `tui.JSONOutput` writes wire version 2 envelopes with media bytes inline whatever their size, instead of failing on media over 256 KiB. | None. |
| The TUI shows reasoning collapsed, citations as numbered footnotes, refusals, and media as labelled placeholders. It filters the transcript (`Ctrl-F`, or `/` in `StreamModel`), follows the tail until scrolled up, and animates unless `Runner.NoAnimation`, `saige chat --no-animation`, `NO_COLOR` or `SAIGE_REDUCED_MOTION` turns it off. `StreamModel.WithMotion` turns animation on for that model. The interactive runner captures the mouse for wheel scrolling. | Hold Shift (or your terminal's modifier) to select text with the mouse. |

### Evals and RAG

Eval runs record the typed parts of an answer, eval cases can carry media, and the RAG tools return typed parts with citations. Results, units and promoted cases written by earlier releases read unchanged.

| Change | What to do |
| --- | --- |
| `agent/eval.AgentRun` gains `Parts` (the final provider call's assistant parts), `Media`, `Conversions` (executed decisions) and `Citations` (tool sources). `RouteRecord.Conversions` holds the planned report. `AnnotateObservation` records them under `agent.parts`, `agent.media`, `agent.conversions` and `agent.citations`, without media bytes. `AddProvenance` records how media reached the models (`Provenance.Conversions`, `AddConversion`), and `ConfigDrift` reports a kind handled differently. | None. |
| New deterministic scorers read parts: `CitesScorer` (`cites`), `RefusedScorer` (`refused`), `HasPartScorer` (`has_part`) and `NoConversionScorer` (`no_conversion`). They decline observations recorded before parts existed. | None. |
| An eval input may take the parts form, `{"parts": [...]}` in the shared part codec, with media referenced by `uri` (such as `file:images/a.png`, relative to the dataset with `eval.DirResolver`) or `ref`. Inline bytes need `"inline": true` and stay under 64 KiB per part. `eval.DecodeInput`, `EncodeInput` and `ResolveInput` read, write and resolve it. A JSON string input is the text form, as before. | Keep media out of dataset rows; reference it by path or URI. |
| A judge whose case input carries media sends it with the prompt through `eval.PartsGenerator`; `agent/eval.Generator` and `online.BudgetedGenerator` implement it. The media goes through the conversion policy (`agent/eval.WithConversion`, `BudgetedGenerator.Conversion`), so a judge model that cannot take it rejects the score unless an action is permitted. A judge on another generator fails a media case with `eval.ErrJudgeMedia`. `eval.WithJudgeResolvers` fetches media referenced by uri. | Pass a judge model that takes the media, or permit an action. `saige eval online --judge-media extract\|omit` does this for the CLI judge. |
| Online records read typed parts from conversation trees: `Record.InputParts`, `Parts` and `Conversions`. A record whose input carries media becomes an observation with the parts form as its input, and promoted cases keep the media references. | None. |
| `rag/types` adds `ContentVideo` and `ContentDocument`, `ContentVariant.Part()` and `VariantFromPart`. | None. |
| `rag/extractor.Auto` is the one extractor registry: `PartExtractor()` and `Extractors()` expose it to the agent (`convert.DocumentExtractor` uses it), `RegisterParts` adds an agent-side extractor, and `RegisterImages` ingests images by a describer's text (`convert.AsExtractor(convert.Describe(p))`). Images are not ingested without a describer. | Call `auto.RegisterImages(...)` to ingest images. |
| `rag_search` and `rag_lookup` implement `RichTool`: each hit is a retrieval citation anchored to its chunk (document, section and variant UUIDs, section index, highlight spans, and for a lookup a quote with its offsets), and media variants come back as media parts, within `tool.WithMediaLimits` (default 4 MiB, 4 parts). `Execute` returns the same text as before. | None. A model that cannot take a returned image follows the agent's conversion policy. |
| `saige rag ingest` takes the MIME type from the file extension when `--mime` is not set, and `--describe-images` ingests an image by a description from `--provider` and `--model`. | Pass `--mime text/plain` to keep reading a file as plain text. |

### Anthropic adapter

| Change | What to do |
| --- | --- |
| The adapter maps typed parts natively. Images (JPEG, PNG, GIF, WebP) and documents (PDF, `text/plain`) go out by Files API ID, https URL (images and PDFs) or bytes, in that order of preference. `DocumentMeta` title, context and citations are sent. A tool result carries text, JSON, images and documents. An opaque `FilePart` goes out as a `container_upload` when code execution is enabled. | None. |
| A part the API cannot take fails the request before anything is sent, with an error that names the part (`part 0.1 (audio audio/wav)`) and matches `types.ErrModalityUnsupported` or `types.ErrMediaUnavailable`. This covers audio and video, other image and document types, non-https URIs, unresolved or elided sources, and generated media in an assistant turn. Before, non-native file bytes were sent as `[File: name]` text and unsupported tool output became a placeholder. | Convert such media first, or remove it. Text documents other than `text/plain` (CSV, Markdown) need extraction to text. |
| A Files API ID is sent only when its `VendorFile.Endpoint` equals the adapter's endpoint, set with the new `WithEndpoint` (empty by default). An ID recorded for another endpoint, or an unscoped ID on a scoped adapter, is never sent: the part falls back to its URL or bytes, or is rejected. An expired upload is skipped. | Record uploads with `types.VendorFileID("anthropic", endpoint, id, mediaType)` for the endpoint that will read them, and set `WithEndpoint` to the same name. |
| The stream emits part deltas at Anthropic's content block index. Redacted thinking is a `ThinkingPart` with `Redacted` set and the data in `Signature`, and is replayed as `redacted_thinking`. Text citations (char, page, content block, web search and search result locations) are `CitationPart`s anchored to their text part, emitted after the content blocks. A `refusal` stop adds a `RefusalPart` with the category and explanation before the content-filter error. Code execution output files are `FilePart`s in `ServerToolResultPart.Outputs`. Batch results map the same way. | Read citations and refusals from the parts. |
| Server tool calls and results are replayed natively in later turns when the request still offers that tool; otherwise they are left out as before. | None. |
| A request may carry a schema and options together; both are applied. | None. |

### OpenAI adapters

| Change | What to do |
| --- | --- |
| The Chat Completions and Responses adapters map media parts natively and reject the rest before the request, with an error matching `types.ErrModalityUnsupported` or `types.ErrMediaUnavailable` (both match `types.ErrInvalidModelConfig`) that names the message, part, kind and media type. Chat takes jpeg, png, gif and webp images (URL or bytes), PDF documents (bytes or an OpenAI file ID) and wav or mp3 audio (bytes). Responses takes images by file ID, URL or bytes, and documents by file ID, URL or bytes for PDF, text and code, office documents, spreadsheets and presentations. Responses has no audio input. Neither takes video. Unsupported files are no longer sent as `[File: ...]` text. | Send media in a form the surface takes, or use the Responses adapter for non-PDF documents. A workspace reference must be resolved to bytes or a URL first. |
| Chat Completions tool messages carry text only. Behind the conversion layer (`provider.Build`, or any agent), an image in a tool result is sent in a user message after the tool messages, and the tool message keeps the text with a notice. The chat adapter alone rejects a tool result with media, and a document in a tool result is rejected unless the modality dial converts it. The Responses adapter sends such a result natively as a `function_call_output` list of `input_text`, `input_image` and `input_file`. | Use `openai.NewResponses` for tools that return documents. |
| A refusal is a `RefusalPart`, and the stream then ends with a content-filter error. Before, the chat adapter dropped it and the Responses adapter streamed it as text. | Read `RefusalPart` from the turn; error handling is unchanged. |
| The adapters stream part deltas themselves. Responses numbers parts in output order (`output_index`, then `content_index`); Chat numbers them in the order they start. URL and file citations arrive as `CitationPart`s anchored to their text part. | None. |
| On models that reason, the Responses adapter asks for `reasoning.encrypted_content` and returns each reasoning item as a `ThinkingPart` whose `Signature` is the encrypted content. Such a part is sent back as a reasoning item in later turns. `WithReasoningSummary("auto")` adds summaries (`ThinkingPart.Summary`); the chat adapter rejects it. | Keep thinking parts in stored turns. |
| `WithAudioOutput(voice, format)` asks an audio model on Chat Completions for speech. The answer is an `AudioOutPart` with the bytes, transcript, server ID and expiry; a later turn references the audio by ID while it lives, then by its transcript. The Responses adapter rejects the option. | None. |
| Both adapters accept a request with a schema and options together, and the batch path uses the same mapping. | None. |

### Google adapter

| Change | What to do |
| --- | --- |
| The Google adapter maps media parts natively: images (PNG, JPEG, WebP, GIF, HEIC, HEIF), audio, video and documents (PDF and text) as inline bytes or file data. It picks the adapter's own upload first (a Files API name or URI on the Gemini API, a `gs://` URI on Vertex AI), then a URI the backend reads (`https`, YouTube for video, and `gs://` on Vertex AI), then inline bytes up to 20 MB. A URI-only part is sent as file data instead of being dropped. `VideoMeta.ClipStart`, `ClipEnd` and `FPS` become `videoMetadata`, in whole seconds. | None. |
| A part the adapter cannot send fails before the request with an error that names the message, part, kind and media type. An opaque `FilePart`, an unsupported media type, a locator the backend cannot read, inline bytes over 20 MB and a generated video match `types.ErrModalityUnsupported`; a source with no usable locator matches `types.ErrMediaUnavailable`. Both match `types.ErrInvalidModelConfig`. Before, such parts were dropped. | Upload large media, pass a readable URI, or convert the part before the request. |
| Tool results carry images, PDFs and plain text as parts of the function response on Gemini 3 models (inline bytes; Cloud Storage URIs on Vertex AI too). Earlier models reject media in a tool result instead of receiving it as a separate user turn. A function response names the function it answers. | On Gemini 2.5 models, return text from tools or convert media before the request. |
| The adapter emits part deltas itself. A thought signature on text or generated media streams as an empty, signed `ThinkingPart` right after that part, and on a function call right before it; replay puts each back on its part. Code execution calls and results are replayed. Grounding sources arrive as `CitationPart`s at the end of the turn, anchored to the text a grounding support covers, plus one unanchored part per source no support names. A safety stop or blocked prompt adds a `RefusalPart` before the content-filter error. | Keep empty thinking parts in stored turns. Read citations from the turn's parts. |
| `WithResponseModalities` and `WithSpeechConfig` request image or audio output. Images arrive as `ImageOutPart` (with the thought signature Gemini puts on them) and audio as one `AudioOutPart` per run, with its bytes in `PartDelta.Data` chunks and the sample rate read from the media type. | None. |
| A request may carry a response schema and options together. | None. |

### Ollama adapter

| Change | What to do |
| --- | --- |
| The Ollama adapter streams part deltas natively, numbering parts in the order they start (thinking, text, then each tool call). Tool calls keep the ID the runtime returns, and replayed calls send it back; a tool message carries `tool_call_id` and `tool_name`. A stream that ends without `done` leaves its open parts open instead of closing them, so the aggregator reports them as truncated. | None for consumers of part deltas. |
| A request may carry a schema and options together: the options (tool choice, dials) apply, and the schema is sent as `format`. | None. |
| Images are sent only as inline JPEG or PNG bytes, in `images` on user and tool messages. An image reachable only by URL, workspace reference or vendor file, another image format, and audio, video, document and file parts fail before the request with an error matching `types.ErrModalityUnsupported` that names the part path. A source with no locator left, or one marked unavailable, fails with `types.ErrMediaUnavailable`. Before, such parts were dropped silently. Server tool parts and generated media in the history are rejected the same way. | Resolve URLs to bytes before the request, or route the request to a provider that takes the part. |
| Assistant thinking is replayed in the `thinking` field (text only; signatures and redacted blocks have no meaning to the runtime), refusals are replayed as content, and citations are not sent. | None. |

## Earlier releases

These changes shipped in the releases before this one, up to 0.32.0. Names are given as they are now; see [step 2](#2-rename-symbols-and-fix-constructors) for the renames.

### Providers

| Change | What to do |
| --- | --- |
| On claude-sonnet-5-5, claude-opus-5-5, claude-fable-5-1 and claude-mythos-5-1 the Anthropic adapter sends a response schema as `output_config.format` instead of failing with `ErrSchemaUnsupported`. `OutputAuto` therefore picks native output for them instead of the `final_answer` tool. | None. Set `WithOutputMode(OutputTool)` to keep the `final_answer` tool. |
| An Anthropic response that stops with `model_context_window_exceeded` ends with an `ErrorKindContextLength` error instead of passing as a final answer. | Handle it like any context-length error, for example with compaction. |
| The OpenAI Responses adapter sends the in-memory prompt cache retention as `in_memory`; the API now rejects `in-memory`. | None. `WithPromptCache` still accepts either spelling. |
| saige-mcp negotiates MCP protocol `2025-11-25` or older, never `2026-07-28`, because that revision forbids the elicitation approval relies on. Clients on the new SDKs try `server/discover` first and then fall back to `initialize`, so a connection costs one more request against the HTTP rate limit. | None. Allow for the extra request when sizing `--rate-burst`. |
| The Anthropic and OpenAI chat adapters set SDK retries to 0 by default. | Wrap them with `retry.New(adapter, retry.DefaultConfig())`, or pass `WithMaxRetries(n)` for a bare adapter. The OpenAI embedder keeps the SDK default; pass `WithMaxRetries(0)` when a retry decorator wraps it. The `saige` CLI now wraps hosted providers in `retry.Provider`. |
| `fallback.New` uses `fallback.DefaultFallbackOn`: it falls back on every error except cancellation, `ErrInvalidModelConfig`, `ErrorKindInvalidRequest` and budget errors. | Set `FallbackOn` (for example `types.IsTransient`) to keep transient-only fallback. |
| Retry, fallback, cache, privacy and tracing decorators reject a response schema the inner provider cannot enforce, with an error matching `types.ErrSchemaUnsupported` and `types.ErrInvalidModelConfig`. Request options are rejected the same way (`types.ErrOptionsUnsupported`). | Use a provider that implements `StructuredOutputProvider`, or drop the schema. A fallback chain skips such a member and tries the next. |
| Every built-in adapter implements `types.OptionsProvider` (it applies `Request.Options`), including the tool choice. Ollama emulates none and named choices by filtering tools and rejects required. | Forced tool choices from the agent now reach the adapters instead of failing with `ErrInvalidModelConfig`. |
| Adapters report an `ErrorDelta` for a stream that ends without a finish reason and for tool-call arguments cut off by the output token limit (`types.ErrResponseTruncated`). Complete but malformed tool-call arguments end the call with `ToolCallPart.ArgumentsError`; the agent answers it with an "invalid tool arguments" result so the model can correct it. | Treat these as failures of the turn, not as an empty answer. |
| A Gemini error whose `google.rpc.ErrorInfo` reason is `API_KEY_INVALID`, `API_KEY_EXPIRED` or `ACCESS_TOKEN_EXPIRED` is `ErrorKindAuth` (`types.IsAuth`), not `ErrorKindInvalidRequest`, although Gemini sends it as HTTP 400. | Check `types.IsAuth` for a bad Google key. |
| `ModelCapabilities.Without(CapStructuredOutput)` also sets `StructuredOutput` to `StructuredOutputNone`. An Anthropic adapter that thinks by default therefore resolves `OutputAuto` to the `final_answer` tool instead of a forced-tool schema it would reject. | None. |
| The Ollama adapter reports `CapToolChoice` for models that declare tool calling, so `agent.Config.ToolChoice` named and none reach its emulation. Required is still rejected before any request. | None. |
| The Ollama client has no total `http.Client` timeout; `WithStreamIdleTimeout` bounds silence between chunks instead. | Set a deadline on the context for a total bound. |
| Anthropic models that accept a trailing assistant turn declare `types.CapAssistantPrefill`, so `Agent.Continue` resumes the partial turn directly. Trailing whitespace is trimmed from that turn, which the API requires. | None. |
| The Anthropic adapter drops empty or whitespace-only system text, and omits `system` when none is left. The API rejected an empty text block with 400. | None. |
| The Anthropic adapter rejects a required or named tool choice, and forced-tool structured output, only with a manual thinking budget (`WithThinking`) or on a model whose row declares `reasoning.forced_tool_choice: false` (in a version 2 catalog, an offering whose `params.tool_choice.values` is `["none"]`). Adaptive thinking, including a model's default thinking, no longer blocks them, so claude-haiku-5-5 keeps forced tool choice and native structured output. | None. On claude-sonnet-5-5, claude-opus-5-5, claude-fable-5-1 and claude-mythos-5-1 a forced choice fails locally, as the API would. |
| `ModelCapabilities` gains `RejectsForcedToolChoice` and `ChatCompletionsTools`; `ValidateToolChoice` rejects a forced choice on a model that declares the first. `Intersect` keeps the stricter value of each. | Code that builds `ModelCapabilities` by hand can leave them zero. |
| The OpenAI chat adapter applies a model's Chat Completions tool rule: on gpt-6-luna and gpt-6-sol a request with tools and no reasoning effort sends `reasoning_effort: "none"`, and another effort with tools fails locally. On gpt-6.1-sol and gpt-6-astra a request with tools fails locally on the chat adapter. `provider.Build` serves those two models through `openai.NewResponses`. | Use `provider.Build`, a preset, or `openai.NewResponses` for gpt-6.1-sol and gpt-6-astra. To keep reasoning with tools on gpt-6-luna, use the Responses adapter. |
| Google models can run on Vertex AI: `provider.Config.Vertex`, the catalog entry's `vertex` block, `GOOGLE_GENAI_USE_VERTEXAI=true`, and the CLI's `--provider vertex`. The project and location default from `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION` (then `global`). The Google adapter and embedder now attach Application Default Credentials (or `WithCredentials` / `WithEmbedCredentials`) to Vertex requests themselves; before, `WithVertex` sent unauthenticated requests. `WithEmbedVertex` selects Vertex for the embedder. | A caller that passes its own authenticating `WithHTTPClient` keeps working: credentials are only looked up when neither an HTTP client nor credentials are given. |
| The Google adapter returns the thought signature Gemini 3 puts on a function call part. It streams as an empty, signed `ThinkingPart` just before the call, and goes back on the call's part. Without it, the turn after a tool call failed with 400 "Function call is missing a thought_signature". | Keep `ThinkingPart`s in stored turns; do not strip empty ones. |
| `WithResponseSchema` in auto output mode uses the `final_answer` tool when the provider reports tool calling but cannot constrain output now, for example a model that rejects forced tool choice. | None. Set `WithOutputMode(OutputNative)` to require native output and fail otherwise. |
| The Google and Ollama embedders classify HTTP failures like chat errors, so a rate limit or overload is transient. The Google embedder sends `RETRIEVAL_QUERY` or `RETRIEVAL_DOCUMENT` from the embed purpose on the context; `WithTaskType` fixes one. | Embedding vectors from a Google model can change once a purpose is set; re-index if query and document vectors must match an older index. |

### Catalog and presets

| Change | What to do |
| --- | --- |
| The `default` and `ollama` presets run `qwen3.5:4b` instead of `qwen3`, and set `local_fallback`: when that model is not pulled, `preset.Build` serves another pulled chat model and records a `local_model_substituted` warning. With no chat model pulled the entry fails with `preset.ErrNoLocalModel` (or is dropped when optional), and `preset.Available` reports it. | Pull `qwen3.5:4b`, or name a model with `--model` or a catalog layer to pin one. |
| The model table moved from Go code to the embedded `agent/provider/catalog/data/default.json`. Revisions it installs record the source `catalog/default.json`. Lookups are unchanged; a golden test freezes them. | Correct rows with a catalog layer or `catalog.Register`, as before. See [model catalog and presets](catalog.md). |
| Rows for `claude-haiku-5`, `claude-haiku-5-5`, `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-fable-5-1` and `claude-mythos-5-1` were added. The Claude 5 family and the 4.6, 4.7 and 4.8 rows declare a 1M context window and 128K output tokens. | Budgets and context-window routing see the larger window. |
| New rows, priced as of 2026-10-09: gpt-6-luna, gpt-6-sol, gpt-6.1-sol, gpt-6-astra, gpt-5.6-luna, gpt-5.6-terra, gpt-5.6-sol, gemini-3.1-flash-lite, gemini-3.5-flash-lite, gemini-3.7-flash and gemini-3.8-flash. claude-haiku-5-5, claude-sonnet-5-5, claude-opus-5-5, claude-fable-5-1 and claude-mythos-5-1 are priced, and gemini-3.1-pro carries its 200K-and-under rate. | Budgets that refused these models as unpriced now run. |
| The gpt-6 rows declare their effort ranges. gpt-6-luna and gpt-6-sol accept `temperature` and `top_p` only with reasoning effort `none` and default to `medium`; gpt-6.1-sol and gpt-6-astra have no effort `none` and declare no sampling knobs. A preset that sets temperature on them fails at load. | Add `"reasoning": {"effort": "none"}` beside the temperature on gpt-6-luna, or drop the temperature. |
| Legacy rows point `superseded_by` at the current models: small OpenAI models at gpt-6-luna, the rest at gpt-6.1-sol; Claude 3 and Haiku 4 at claude-haiku-5-5, Sonnet 4 and 5 at claude-sonnet-5-5, Opus 4 and 5 at claude-opus-5-5; Gemini 2.x and gemini-3-flash-preview at gemini-3.1-flash-lite, or gemini-3.8-flash for gemini-2.5-pro. gemini-2.0 is noted as shut down on 2026-06-01. Explicit rows for gpt-4.1-nano, gpt-5-mini and gpt-5-nano keep them from inheriting a larger family's successor. | `saige models` and `catalog.Successor` name the new targets. |
| The shipped presets use the cheapest current model per vendor: `default`, `anthropic`, `openai` and `google` run claude-haiku-5-5, gpt-6-luna and gemini-3.1-flash-lite. New `anthropic-quality`, `openai-quality` and `google-quality` presets run claude-sonnet-5-5, gpt-6.1-sol and gemini-3.8-flash, and a `vertex` preset runs gemini-3.1-flash-lite on Vertex AI. The eval harness default model and the `saige eval init` manifest use gpt-6-luna. | Pass `--preset <vendor>-quality`, `--model`, or a catalog layer to keep a stronger default. The default chain's primary now signs its reasoning, so validation warns that a tool loop it starts cannot fail over. |
| The router validates a request's options merged with the profile's own configured options (`types.OptionsReporter`), not the request options alone. A profile can be excluded that was eligible before, for example a `gpt-5.2` profile with a temperature and a request that sets a reasoning effort. | Intended: the request would have failed inside the adapter. The route reason is `options` when the group's first member was skipped. |
| `RouteDelta` carries `Preset`, `ConfigHash`, `CatalogRevision` and `Options`, and the agent attaches a `RoutePart` to each committed assistant turn served through a router. | None; the fields are optional on the wire and in stored trees. |
| The span's `gen_ai.request.*` attributes come from the serving attempt's effective options when a route reports them. | Dashboards that read these attributes now see the options actually sent. |
| `OutputAuto` uses the native schema path only when the provider's reported capabilities include structured output, even for a model inferred from a family prefix. | None. |
| The CLI has no table of default models. Without `--preset`, `--model` or `--provider` it runs the first entry of the catalog's `default_preset` that can serve (a key is set, or the local Ollama server answers), as a one-entry chain. It no longer fails over across vendors by default. With no key and no Ollama it reports which variables to set. | Pass `--preset default` (or your own preset) for cross-vendor failover. Pass `--model` or `--preset` to pin one configuration. |
| An optional chain entry that needs no credentials (Ollama) is dropped at build time when its server does not answer `preset.Options.Probe` (default `preset.ProbeOllama`, a 2s version request). | Pass a `Probe` that returns nil to keep the old behavior. |
| `routing` gains `fail_threshold`, `reprobe_after` and `failover_on_auth`. The first two map to `router.Affinity`, so a session returns to the primary. An authentication failure still ends the request unless `failover_on_auth` is set. | None. |
| `--base-url` applies to the selected provider's entries on every path, including `--preset` and the default. A multi-vendor chain without `--provider` is an error. | Add `--provider` to say which entries the URL is for. |
| An untrusted project catalog is checked against an allowlist. Besides `base_url` and `api_key_env`, it may not set `mcp_server`, `routing.failover_on_content_filter`, `routing.failover_on_auth`, `inherit_default`, or `vertex.project` and `vertex.location`. | Set `SAIGE_TRUST_PROJECT_CATALOG=1` or name the file with `--catalog`. |
| `types.ServerTool.Validate` requires a remote MCP server URL to be `https` with a host. | Use an https endpoint. |
| `HTTPSource` redacts user information and the query string from its name, which errors and installed revisions use. | Read the full URL from your own configuration, not from errors. |
| `RouteDelta.Provider` and `gen_ai.provider.name` name the adapter beneath decorators (`openai`, not `retry(openai)`). A routed span's `gen_ai.provider.name` and `gen_ai.request.model` come from the serving route. | Update dashboards or filters that matched `retry(...)`. |
| `StyledOutput.StreamDeltas` returns a terminal error without printing it; the caller reports it with `Output.Error`. `StreamVerbose` still prints it. | Call `Output.Error` with `VerboseResult.Err`. |

### Agent loop

| Change | What to do |
| --- | --- |
| With `agent.Config.Store` set and no `Tree`, the default tree is built with `tree.WithStore`, so compaction branches, feedback, the active branch, archive state, rewinds and checkpoints persist. A failed store write now fails the change and the run. | Build a caller-supplied tree with `tree.WithStore(store)` too. `LoadTreeFromStore` with an empty branch reloads onto the saved active branch. |
| The tree stores `TruncationPart`, `SteerPart`, server tool parts and `RoutePart`. Server tool calls are recorded on the assistant message. | None; older trees still load. |
| Compaction moves its boundary so a tool result stays with its call, and compacted branch IDs no longer nest (`compact-main-<id>`). | Code that parsed nested branch names must read the new form. |
| `WithTracing` sets `agent.Config.RunTracer`, so each run opens an `invoke_agent` span. The loop records cache token usage and the run outcome through `types.CacheUsageRecorder` and `types.AgentOutcomeRecorder`. | Remove a manual `NewAgentTracer(...).StartAgent` around `Invoke`, or the run gets two spans. |
| A marked tool is found through decorators that implement `Unwrap() types.Tool`, so it prompts for approval even when a decorator hides it. | None. |
| A repeated decision for an answered marker returns `agent.ErrMarkerResolved`; `saige serve` answers it with 409. | Treat 409 as already decided. |
| A sub-agent at its step limit gives a forced final answer (`MaxIterForceFinal`) instead of inheriting the parent's `OnMaxIter`, and gets a wrap-up note two iterations before its cap. A forced result reaches the parent model with a note saying so. | Add `agent.WithOnMaxIter(agent.MaxIterError)` to `SubAgentDef.Options` to fail the delegation at the limit. Set `WrapUpAt: -1` to send no note. |
| A negative `MaxIter` means no cap (`agent.NoIterLimit`); it used to mean 10. A sub-agent under an uncapped parent defaults to `DefaultSubAgentMaxIter`. | Pass 0 for the default of 10. |
| Each sub-agent invocation gets a private in-memory scratch over a read-only view of the parent's workspace, and a child with tools gets `scratch_write`, `scratch_read` and `scratch_search`. Child writes and spills now succeed into the scratch instead of failing with `workspace.ErrReadOnly`. | Set `SubAgentDef.Scratch.Off` for the previous behavior, or `Scratch.NoTools` to keep the scratch without the tools. |
| A delegated task or result over about 2000 estimated tokens goes by reference: the model sees a `saige-artifact://` URI and a preview, and reads more with `read_artifact`. An agent with sub-agents gets `read_artifact` and `search_artifact`. `SubAgentResult.Output` still holds the whole result. | Set `SubAgentDef.References.Off` to keep everything inline, or raise `InputTokens` and `ResultTokens`. See [delegation](delegation.md). |
| A handoff group, and a durable approval runner, accept a disabled compaction policy (`Strategy: types.CompactNone`, `agent.WithoutCompaction()`). Before, any `CompactCfg` failed the run. Active strategies are still rejected. | None. |
| Every compaction streams a `types.CompactionDelta` (wire kind `compaction`) and writes a `types.CompactionPart` record onto its new branch, which is stripped before provider calls. | A wire reader older than this release rejects the new kind with `ErrUnknownWireKind`; upgrade readers before producers. Exhaustive type switches over deltas or content gain a case. |
| `ConfigPart.CompactNow` compacts the next turn without `MaxInputTokens` too. `summarize` and an empty strategy then summarize the older half of the branch regardless of `Threshold`. | Do not set `CompactNow` on a policy you expect to wait for its threshold. |
| `summarize` without `MaxInputTokens` and `sliding_window` move their boundary back so the kept messages never start with a tool result, also when one call's results span several messages. Kept messages are copied to the new branch with their metadata (`ConfigPart`, `RoutePart`, approval records) instead of stripped. | None. |

### Storage

| Change | What to do |
| --- | --- |
| PostgreSQL 18 is required. `RunMigrations` reads `server_version_num` and returns `postgres.ErrUnsupportedServer` below 180000. | Upgrade the server to PostgreSQL 18, for example with the `paradedb/paradedb:0.26.1-pg18` image. See [deployment](deployment.md). |
| `RunMigrations` creates the `vector` and `pg_search` (ParadeDB) extensions and returns `postgres.ErrExtensionUnavailable` when either is not installed. It adds the BM25 index `idx_rag_variant_bm25` on `rag_variant.text`. | Install pg_search on the server, or use the ParadeDB image. pg_search is AGPL-3.0, licensed separately from saige. Building the index on a large `rag_variant` table takes time; run migrations before the rollout. |
| `rag.WithBM25` over a store that implements `types.KeywordSearcher`, such as `rag/pgstore`, searches through the store's BM25 index instead of an in-memory one. The `bm25retriever.Config` is ignored there, and BM25 scores come from pg_search. `pgstore.NewKeywordRetriever` exposes the same search as a retriever. | Drop the `rag.RebuildIndex` call after opening a pgstore pipeline; it is now a no-op for the BM25 arm. Retune a BM25 `MinScore` against pg_search scores. |
| `saige rag search` runs hybrid search (vector plus pg_search BM25), so keyword matches are found from a fresh process. | None. |
| `RunMigrations` adds `section_heading` and `document_title` to `rag_variant`, copies each variant's section heading and document title into them, and rebuilds `idx_rag_variant_bm25` over `(id, text, section_heading, document_title)`. This happens once. | Run migrations before the rollout; the copy and rebuild read the whole `rag_variant` table. Plain keyword searches still search the variant text only, so scores do not change. |
| `rag.WithBM25` finds the keyword search of a store wrapped by a decorator that implements `Unwrap() types.Store`. | Give store decorators an `Unwrap` method; a decorator that must see keyword searches implements `types.KeywordSearcher` itself. |
| `postgres.NewPool` with individual fields no longer forces `sslmode=disable`. pgx `prefer` encrypts without verifying the certificate and falls back to plaintext. | Set `sslmode` explicitly. |
| `RunMigrations` stops at the first failing statement and checks the embedding dimension when one is set explicitly. New migrations add `kg_episode.document_id`, `kg_relation_episode`, a relation fact search index, and `rag_document.scope` and `source_modified_at`. | Run migrations before starting the new release. |
| pgstore node reads are scoped to the conversation. `SaveNode` returns `ErrVersionConflict` for a stale version and `ErrConversationMismatch` for a node of another conversation. memstore also rejects stale versions. | Handle the errors instead of relying on silent skips. |
| The DBOS backend is removed. | Use the local engine or the duraturo adapter. |

### RAG sources

| Change | What to do |
| --- | --- |
| `source.Filesystem` skips `.git`, `.hg`, `.svn` and `node_modules`, dot files and dot directories, secret file names (`source.DefaultDenyPatterns`: `.env*`, `*.pem`, `*.key`, `id_rsa*`, `credentials*.json`, `*.tfstate*` and similar), and paths matched by `.gitignore` or `.saigeignore`. Before, it read every file under `Dir`. | Set `IncludeHidden`, `IncludeToolDirs`, `AllowSecretNames` or `NoIgnoreFiles` to keep a file the new defaults skip. Documents already ingested from such files stay in the store; delete them by UUID. |
| `SyncSource` compares the fingerprint of every fetched document that carries bytes, even when its `SourceModifiedAt` is not after `Since`. Before, an edit that kept an old modification time (`cp -p`, `rsync -a`) was counted as unchanged. | None. `Since` now only lets a document without bytes count as unchanged by time. |
| With `Prune`, `SyncSource` keeps documents whose URI a `types.FilteringSource` reports in `SyncResult.Skipped`. A `Filesystem` reports every file or directory a rule skipped, including files outside `Extensions` and subdirectories of a non-recursive walk, which were pruned before. | Delete documents a narrower filter now skips by UUID. |

### Evals

| Change | What to do |
| --- | --- |
| A subject or scorer error that `eval.IsInfra` reports (rate limit, unavailable, other transient kinds, authentication, network failures, timeouts, cancellation, expired, canceled or unclassified errored batch requests, `eval.ErrInfra`) is inconclusive. `Gate` returns the new `eval.OutcomeInconclusive` instead of `failed` when every violation is of that kind and more than `GatePolicy.MaxInconclusive` (default 0) of the cases are affected; within the tolerance it returns `passed`. A real violation still makes the outcome `failed`. | Handle `OutcomeInconclusive` where you switch on the outcome, for example by rerunning. `len(Check(...)) == 0` still requires every gate to be checked and hold. |
| An inconclusive score gets no `Score.Passed` verdict and is left out of `PassRate`, `GroupPassRate`, `MinPassRate`, McNemar and bootstrap pairing. A case one arm could not measure has the new `CaseInconclusive` status instead of `only_base` or `only_exp`. | Pass rates over a run with outages now cover the measured cases only; read `SuiteResult.Inconclusive` and `Completeness` for the gap. |
| `Violation` gains `Kind` (`metric`, `scorer`, `subject`, `inconclusive`), and `Violation.String` prefixes inconclusive ones with `inconclusive:` and no longer prints a blank metric for suite-level violations. Stored violations without a kind read as real failures. | Update code that matched violation text. |
| `harness.Runner.Run` returns an `*InconclusiveError` (`harness.ErrInconclusive`) instead of the joined script errors when every failed script failed on infrastructure, and nil when they stay within `Runner.MaxInconclusive`. An edit turn lost to infrastructure records `inconclusive: true` in `metrics.json`, and its `turn_succeeded` score is an inconclusive error instead of 0. `saige eval run` exits with status 3 for such a run (status 1 before); `--max-inconclusive` sets the tolerance. | Treat exit status 3 as "rerun", not as a failure. A custom flow that adds an `inconclusive` key to `TurnResult.Extra` must rename it. |
| `RunMigrations` adds `eval_score.inconclusive` (default false). `pgstore.Diff` with `Regressions` leaves out candidate scores that are inconclusive or missing because the candidate's subject failed on infrastructure. Older rows read as conclusive. | Run migrations before the rollout. |
| `online.Failing` no longer promotes a unit for an inconclusive score, such as a judge call refused by an overloaded provider. | None. |

### Toolchain

| Change | What to do |
| --- | --- |
| The module requires Go 1.26.9, and `golang.org/x/net` v0.60.0 and `golang.org/x/text` v0.42.0. Go 1.25 has no release with the fixes for the standard-library vulnerabilities govulncheck reports, x/net v0.60.0 requires Go 1.26, and the duraturo dependency requires Go 1.26.4. | Build with Go 1.26.9 or newer. |
