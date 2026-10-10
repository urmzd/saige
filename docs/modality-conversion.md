# Modality conversion

A model takes some media natively and not others: claude-haiku-5-5 reads images and PDFs but not audio, gpt-6-luna on Chat Completions reads images but not documents, and a local model may read text only. When a request holds a part the serving model cannot take, saige never drops it or sends a placeholder. Each attempt is planned against the offering that serves it, and every part is sent natively, converted by an action you permitted, or the request is rejected.

**Reject is the default.** A part is converted only when the modality dial names the action, the same way a contractual dial is loosened only by naming it (D-45).

- [Actions](#actions)
- [Configuring](#configuring)
- [Where it runs](#where-it-runs)
- [Memoization, durability and budget](#memoization-durability-and-budget)
- [Seeing what was converted](#seeing-what-was-converted)
- [Reasoning across vendors](#reasoning-across-vendors)
- [Known limits](#known-limits)

## Actions

For each media part, including media inside a tool result, the planner decides one of:

| Decision | When |
| --- | --- |
| `native` | The offering's input modalities list the media type, the part is within the limits (bytes, count), and it has a locator the endpoint reads: inline bytes, a URI with a scheme the endpoint fetches (`gs` on Vertex AI, `https`), or a file in the endpoint's own store. |
| `lowered` | An image in a tool result that the adapter moves to a follow-up user message, as Chat Completions requires. |
| one of the actions below | The part is not native, and the modality dial permits the action and a registered converter accepts the part. |
| `rejected` | Nothing permitted can serve the part. The attempt fails with an error matching `types.ErrModalityUnsupported` (and `types.ErrInvalidModelConfig`), naming the part's path, kind, media type and reason. Media whose bytes cannot be reached also matches `types.ErrMediaUnavailable`. |

The actions, in the order you list them for a modality:

| Action | What it does | Built-in converter |
| --- | --- | --- |
| `reject` | Refuses the request. The default. | none needed |
| `convert` | Keeps the modality and changes the format, such as a GIF to PNG for a model that reads only JPEG and PNG. | `convert.Transcode()` |
| `transcribe` | Replaces audio with its transcript, made by a model that hears it. | `convert.Transcribe(provider)` |
| `describe` | Replaces an image (or a video) with a description, made by a vision model. | `convert.Describe(provider)` |
| `extract` | Replaces a document with its text. | `convert.Documents()` for PDF, HTML, plain text, Markdown and CSV; `convert.Extract(extractor, mediaTypes...)` for your own |
| `omit` | Drops the part and sends a notice in its place, such as `[audio clip.wav omitted: audio/wav is not an input ... takes]`. | none needed |

Actions are tried in order. When a converter fails, the next permitted action is tried, so `["extract", "omit"]` sends a notice for a scanned PDF with no text instead of failing the request. `reject` and `omit` cannot fail, so either must come last.

`Transcribe` and `Describe` call another model, chosen by you: any provider whose offering takes the media natively, such as gemini-3.1-flash-lite. They run with temperature 0 where the model accepts it and a fixed prompt, and the converter's version names the model and a digest of the prompt and output cap.

A converter is any `types.Converter`: a name and version, the action, which parts it accepts, what modalities it produces, an estimate, and the conversion itself.

## Configuring

The modality dial is `types.Dials.Modality`, a `types.ModalityDial`: a default action and a list of actions per modality (`text`, `image`, `audio`, `video`, `document`, `file`). It follows the dial layers (see [Dials](dials.md#setting-dials)), lowest first, and a later layer replaces the list for each modality it names:

| Scope | Where |
| --- | --- |
| Policy | `provider.Config.Conversion.Dial`, the dial of the policy a provider was built with |
| Global | the catalog's top-level `dials.modality`, or `provider.Config.Dials.Modality` |
| Model, preset, entry | `dials.modality` in a row's dial defaults, a preset, or a chain entry |
| Agent | `agent.WithConversion(policy)` (its `Dial`), then `agent.WithDials(types.Dials{Modality: ...})` |
| Turn | `ConfigPart{Dials: &types.Dials{Modality: ...}}` |
| Request | `RequestOptions.Dials.Modality` |

Converters, the cache, a cost cap and the cache scope come from the policy: `provider.Config.Conversion` for a provider, and `agent.WithConversion` above it. The agent's converters are tried first.

```go
gemini, _ := provider.Build(ctx, provider.Config{Provider: provider.Google, Model: "gemini-3.1-flash-lite", Vertex: &provider.Vertex{}})

a := agent.NewAgent(agent.AgentConfig{Provider: claude},
	agent.WithConversion(types.ConversionPolicy{
		Dial: types.ModalityDial{Per: map[types.Modality][]types.ModalityAction{
			types.ModalityDocument: {types.ActExtract, types.ActOmit},
			types.ModalityAudio:    {types.ActTranscribe},
			types.ModalityImage:    {types.ActDescribe},
		}},
		Converters: []types.Converter{convert.Documents(), convert.Transcribe(gemini), convert.Describe(gemini)},
		MaxCost:    types.USD(0.01), // per request, estimated
		Scope:      tenantID,         // memoize per tenant
	}))
```

In the catalog:

```json
{
  "dials": {"modality": {"per": {"document": ["extract", "omit"]}}},
  "presets": {
    "support": {
      "dials": {"modality": {"per": {"audio": ["omit"]}}},
      "chain": [{"offering": "anthropic/claude-haiku-5-5@anthropic"}]
    }
  }
}
```

The catalog permits actions; converters that call a model are configured in code, because they need a provider.

`agent.WithExtractors(map[types.MediaType]types.Extractor{...})` is shorthand: it registers an extract converter per media type and permits `extract` for each of their modalities.

## Where it runs

1. **Before compaction**, the agent resolves sources: a part without bytes whose URI scheme has a `Resolver` is fetched, and bytes get a digest. A part is never replaced. A resolver error marks the source unavailable, and planning rejects it (or omits it, when permitted). A URI with no resolver, such as `gs://` or `https://`, is left for the endpoint to fetch when it can.
2. **Per attempt**, the conversion decorator (`convert.Provider`) plans the request against the offering of the adapter it wraps, runs the planned conversions on a copy of the messages, and sends the copy. `provider.Build` wraps every adapter with it, so router members and fallback members built from a preset have it, and the agent wraps a bare provider it is given. The modality dial is spent here and removed from the request options, so an adapter that takes no options still works with it.
3. **In a router**, each member is planned while candidates are filtered. A member whose plan rejects leaves the request with that reason; a member whose plan converts stays. Failover to a member with other modalities plans again for that member: a vision member sends the image, and a text-only member behind it sends the description.
4. **In a fallback chain**, a member that rejects the request's parts never sent it, so the chain moves on.

The adapter reports its offering where it depends on the endpoint: a Google adapter on Vertex AI reports the Vertex offering, which reads `gs://` URIs, and the OpenAI Responses adapter reports the Responses offering.

**Convert the view, never the record.** The conversation keeps the original parts. Only the copy sent to the provider is converted, so a later turn on a model that takes the media natively sends it as it is.

## Memoization, durability and budget

History is sent again every turn, so a conversion is memoized by the policy's scope, the converter's name and version, and the part's digest (or its URI). The same clip is transcribed once, however many turns or members send it, and the description in the prompt does not change from turn to turn, which keeps provider prompt caches warm. The default cache is in memory, one per agent (shared with its sub-agents) or per decorator; set `ConversionPolicy.Cache` to share one, and `Scope` per tenant when you do.

Each conversion runs as a durable step named `convert:<digest>:<converter>@<version>` under the run's step runner (`types.StepKindConvert`). A replay returns the recorded parts and restores the recorded charge without calling the converter's model again.

Converters that call a model (`convert.Priced`) are charged to the run's budget. The agent adds the plan's estimate to the turn's reservation (`types.Budget.ReserveWith`) when the budget sets a per-call bound (`PerCallCost`, `PerCallTokens`), and each conversion takes its share of that reservation (`types.Budget.Carve`) and settles on its own, under the converter model's rate card. An unpriced converter model is refused under a cost limit unless `AllowUnpriced` is set. `ConversionPolicy.MaxCost` rejects a request whose estimated conversions cost more.

## Seeing what was converted

| Where | What |
| --- | --- |
| `types.RouteDelta.Conversions` | The plan of a router attempt, when it converts anything. |
| `types.ConversionDelta` | The executed report of an attempt, emitted before its output, with the router profile. It is kept on failover, because its cost is real. |
| `types.RoutePart.Conversions` | The serving attempt's executed report, saved with the turn. |
| Traces (`agent/otel`) | A `saige.conversion` span event per report, and `saige.conversion.offering`, `saige.conversion.hash` and `saige.conversion.decisions` on the span for the serving attempt. |

A report lists a `types.ConversionDecision` per part: its path, kind, media type, digest, the decision, the converter (`via`), references to the produced parts, whether it was a cache hit, its cost, the reason the part was not native, and the dial scope that permitted the action. `Hash` identifies the view: the same decisions hash the same whether or not they were served from the cache.

## Reasoning across vendors

A reasoning part carries a signature only its own vendor can verify, and the Anthropic Messages API rejects one it did not sign. The agent records which provider produced each reasoning part (`ThinkingPart.Origin`), and the planner leaves reasoning signed by another provider, or unsigned, out of the view sent to Anthropic. With `ConversionPolicy.Thinking` set to `types.ThinkingAsText`, readable reasoning is sent as a text part instead. Signed reasoning recorded before its origin was saved is replayed as before.

## Known limits

- The response cache key does not include the report hash yet, and a converter's text output is not tokenized by the privacy decorator yet.
- Batch requests are not converted; parts a batch endpoint cannot take are rejected.
- Native media is not counted in the reservation estimate; only converters are.
- No converter ships for video frames, uploads to a vendor file store, or fetching an `https` URI to bytes. Write a `types.Converter` for them.
