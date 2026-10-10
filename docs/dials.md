# Dials: model-neutral generation settings

A raw request option names a vendor parameter: `temperature`, `reasoning_effort`, a thinking budget. A model that cannot take it rejects the request (D-12), so a preset whose chain spans vendors has to spell out each entry's options. A **dial** names an intent instead, such as "focused" or "think hard". It travels with the request and is compiled for each attempt against the model that serves it, so a failover re-targets it instead of failing.

- [The dials](#the-dials)
- [What happens when a model cannot honor a dial](#what-happens-when-a-model-cannot-honor-a-dial)
- [Setting dials](#setting-dials)
- [Changes during a run](#changes-during-a-run)
- [Seeing what was sent](#seeing-what-was-sent)
- [Declaring dials in the catalog](#declaring-dials-in-the-catalog)
- [Scenarios](#scenarios)
- [Known limits](#known-limits)

Raw options keep their meaning and their strictness. Dials are additive: nothing is deprecated, and a raw option that sets the same parameter as a dial wins.

## The dials

| Dial | JSON key | Values | Class | Compiles to |
| --- | --- | --- | --- | --- |
| Creativity | `creativity` | `deterministic`, `focused`, `balanced`, `creative` | advisory | `temperature`, `top_p`, `top_k` |
| Reasoning | `reasoning` | `{"mode": "off" \| "adaptive" \| "on", "depth": "minimal" \| "low" \| "medium" \| "high" \| "max"}` | advisory | a toggle, an effort, a budget or a thinking level |
| Max output | `max_output` | tokens | clamp | `max_output_tokens` |
| Tools | `tools` | `{"mode": "auto" \| "none" \| "required" \| "named", "name": "..."}` | contractual | `tool_choice` |
| Parallel | `parallel` | `true`, `false` | `false` is contractual, `true` advisory | `parallel_tools` |
| Reproducible | `reproducible` | a seed | contractual | `seed` |
| Cache | `cache` | `true`, `false` | advisory | the prompt cache mode the catalog row declares |
| Modality | `modality` | `{"default": action, "per": {"audio": ["transcribe", "omit"]}}` | contractual | a conversion plan, not a vendor parameter; see [Modality conversion](modality-conversion.md) |

A depth without a mode means `on`. Reasoning merges whole: a layer that sets it replaces mode and depth together. Stop sequences and penalties have no portable meaning and stay raw-only.

**Reasoning outranks creativity.** When the effective reasoning rules out sampling controls (the row's `sampling_requires_no_reasoning`, or an API that rejects sampling while the model thinks), creativity is dropped and the conflict recorded. Set `reasoning.mode` to `off` to let creativity apply.

A seed is not reproducibility on any vendor. To replay a call exactly, use the response cache (`agent/provider/cache`).

In Go:

```go
focused := types.CreativityFocused
d := types.Dials{
	Creativity: &focused,
	Reasoning:  &types.ReasoningDial{Depth: types.DepthHigh},
}
```

## What happens when a model cannot honor a dial

Each dial has a class, and the class sets the default handling:

| Class | Default | Recorded as |
| --- | --- | --- |
| advisory | the nearest declared value, or nothing when there is none | `mapped` or `dropped` |
| clamp | lowered to the model's limit, never raised | `mapped` |
| contractual | the attempt fails with `ErrInvalidModelConfig` | `rejected` |

A `types.DialPolicy` changes the handling:

- `Per: map[types.DialName]types.Handling{...}` sets one dial to `reject`, `nearest` or `drop`. Naming a contractual dial here is the only way to loosen it, for example `{"reproducible": "drop"}`. There is no global lenient switch.
- `Strict: true` (`types.StrictDials`) rejects every dial a model cannot honor exactly, and outranks `Per`. Evals use it to hold settings constant across models.

In a router, a contractual dial a member cannot honor removes that member from the request, and the next eligible member serves. An advisory dial never removes a member. When no member is eligible, the error names each member's reason.

## Setting dials

Dials resolve field by field, lowest first:

1. Global: `provider.Config.Dials`, or the catalog's top-level `dials`.
2. The model row's `dials.defaults`.
3. The preset's `dials`, unless the entry says `"inherit": "none"`.
4. The entry's `dials`. The entry's `unset` removes an inherited dial with `"dials.<name>"`.
5. The agent: `agent.WithDials`.
6. A sub-agent inherits its parent's dials unless its `Options` set `WithDials`. A handoff member with its own `HandoffDef.Dials` uses them instead of the entry agent's.
7. The conversation: `ConfigPart{Dials: ...}`, sticky and saved in the tree. Blocks merge field by field in order.
8. One request: `RequestOptions.Dials`.

Layers 1 to 4 are configured on the adapter (`WithDials` on each adapter package). Layers 5 to 8 travel with the request as `RequestOptions.DialLayers` and `RequestOptions.Dials`. A raw option at any layer that sets the same parameter wins over every dial, is recorded as `raw_override`, and is validated strictly.

`agent.WithDialPolicy`, `provider.Config.DialPolicy` and each adapter's `WithDialPolicy` set the policy. `types.ContextWithDialPolicy` overrides it for one call tree, which is how an eval holds dials constant.

Dials compile late, on each attempt, against that attempt's model and request: whether it offers tools or a response schema, and which API serves it. The OpenAI adapter reports its API through `types.DialSurfaceReporter`.

A tool-free call that carries a native response schema does not carry the agent's dials; see [known limits](#known-limits).

## Changes during a run

- `ConfigPart{Dials}` takes effect at the next safe point (D-25).
- While a tool loop with signed reasoning is open, a reasoning mode change waits for the next user turn: providers reject a thinking change in the middle of such a loop. A depth change waits too, unless the row declares `"depth_change": "per_request"`. The report records the change as `deferred`, with the value that was kept.
- When the row declares `"change_resets_cache": true`, a reasoning change against the previous call reports `cache_reset_expected`. The Anthropic thinking rows declare it.
- `types.Switch` carries `Dials`, so an `OutcomePolicy` can raise reasoning depth on the same model before it switches models. A switch that changes neither the model nor the dials is ignored (D-26).
- A running session keeps its catalog snapshot (D-13). A changed catalog revision applies to new sessions.

## Seeing what was sent

`types.ResolveDials` returns a `types.DialReport`:

| Field | Meaning |
| --- | --- |
| `Requested` | the merged dials |
| `Effective` | the raw options the adapter sends |
| `EffectiveHash` | a short hash of `Effective`, equal for equal parameters |
| `Decisions` | one per dial: `applied`, `mapped`, `dropped`, `rejected`, `raw_override` or `deferred`, with what was sent, why, the scope that set it, and `cache_reset_expected` |
| `Policy` | `default`, `strict`, or the overrides |

- Each router attempt's `types.RouteDelta` carries the compiled `Options` and the `Dials` report. The agent saves the committed attempt's route on the turn as `types.RoutePart`, report included. A single adapter reports no route, so for a call with dials the agent compiles them the same way the adapter does and emits the route itself, with the report, whatever the decisions were.
- `saige catalog explain <preset|provider/model> --dials '{"creativity":"focused"}'` prints, for every chain entry, each dial decision with its reason and the raw options the entry would send. `--tools` compiles for a request with tools, `--surface chat|responses` picks the OpenAI API, `--strict` and `--policy name=handling` set the policy, and `--format json` prints the same as JSON.
- Traces add `saige.dials.requested`, `saige.dials.mapped`, `saige.dials.dropped` and `saige.dials.policy` to the call span. `gen_ai.request.*` keeps reporting the effective raw options.
- `agent/eval.AgentRun.Routes` records each call's effective options and decisions. `eval.WithDialPolicy` runs subjects under a policy, and a comparison warns when a held dial was sent as different raw parameters on the two arms.

## Declaring dials in the catalog

A row needs no `dials` object: its mapping is derived from the rest of the row.

- Creativity is derived only when the row declares `temperature`: `deterministic` is temperature 0, `focused` 0.3 (with `top_p` 0.9 when declared), `balanced` 0.7 and `creative` 1.0. A row with `sampling_requires_no_reasoning` drops creativity while reasoning is active.
- Reasoning depth maps by name onto an effort list, then to the nearest declared effort. A budget range maps by fraction of the range; without a declared maximum the budgets are 1024, 2048, 8192, 16384 and 32768 tokens, and a budget is kept below the output cap. A toggle maps to on and off, and a depth collapses to on. Off maps to effort `none`, budget 0 or the toggle where the row allows it, and otherwise to the lowest depth.
- On OpenAI's Chat Completions, a `no_reasoning` row with tools offered maps reasoning to effort `none`. The Responses API keeps the effort, and `provider.Build` serves such a row on the Responses API when a configured reasoning dial is not off.
- Cache selects markers on Anthropic and automatic caching on OpenAI.

Declare `dials` to override any part:

```json
"dials": {
  "creativity": {
    "levels": { "focused": { "temperature": 0.2 } },
    "requires_reasoning_off": true
  },
  "reasoning": {
    "off": { "reasoning": { "effort": "none" } },
    "depth": { "max": { "reasoning": { "effort": "xhigh" } } },
    "depth_change": "per_request",
    "change_resets_cache": true,
    "with_tools": { "chat": { "reasoning": { "effort": "none" } } }
  },
  "cache": { "on": { "mode": "markers", "ttl": "5m", "system": true, "tools": true } },
  "defaults": { "reasoning": { "depth": "medium" } }
}
```

Mapped values are options objects. A declared level, depth or surface overrides the derived one of the same key, so an overlay can patch one level and keep the rest. Templates and rows merge `dials` field by field. See [the catalog guide](catalog.md#dials) for presets, validation and hashing.

## Scenarios

| | Scenario | Expressed as | What happens | Recorded |
| --- | --- | --- | --- | --- |
| a | Temperature 0.2 everywhere, failover from gpt-6-luna to claude-haiku-5-5 | `creativity: focused` | luna defaults to effort medium and takes sampling only with reasoning off, so creativity is dropped. Haiku's adaptive thinking takes no sampling controls, so it is dropped again. Failover proceeds; no member is skipped. | `creativity` `dropped` on both attempts |
| a | The same, raw | `temperature: 0.2` | Both members reject it, no member is eligible, and the error names both reasons. A preset that shares a raw temperature across a chain gets a `prefer_dial` warning. | `ErrInvalidModelConfig` |
| b | "Deterministic" on a model with no seed | `creativity: deterministic` | Advisory: temperature 0 where the model takes it, dropped where it does not. | `applied` or `dropped` |
| b | The same, with a seed | `reproducible: 7` | Contractual: Claude takes no seed, so a Claude member is skipped and a member that takes one serves. | `rejected` on Claude, `applied` on the server |
| c | Depth high across vendors | `reasoning: {depth: high}` | claude-haiku-5-5: effort high. gpt-6.1-sol: effort high. gemini-3.8-flash: thinking level HIGH. Ollama qwen3: think on. | `applied`, `applied`, `applied`, `mapped` ("depth collapsed to toggle") |
| c | Depth max, or reasoning off | `depth: max`, `mode: off` | Gemini 3 maps max to its highest level, high. gpt-6.1-sol requires reasoning, so off maps to effort low; Gemini 3 maps off to its lowest level. | `mapped` |
| d | Tools on gpt-6-luna | `depth: high` with tools | Chat Completions maps reasoning to effort none and records the remedy. The Responses API keeps effort high, and creativity is then dropped by the conflict rule. `provider.Build` picks the Responses API for this row when the reasoning dial is on. A raw effort with tools on Chat Completions stays rejected. | `mapped` on chat, `applied` on responses |
| e | A policy raises depth mid-conversation | `ConfigPart{Dials: depth high, Reason: "policy"}` | Resolved at the next safe point. A mode change waits while a signed tool loop is open. The next route shows a new effective hash and `cache_reset_expected` on rows that declare it. | `deferred`, then `applied` |
| f | Structured output on a model without native support | an output schema | D-21 applies unchanged: tool or prompt mode is chosen, and an explicit `native` is rejected. The SDK never falls back to extracting JSON from text. | not a dial |
| g | An eval that holds settings constant | `eval.WithDialPolicy(types.StrictDials)` | Any map or drop fails the attempt. Each call records the effective options and decisions, and a comparison warns when a held dial was sent differently. | `rejected` |
| h | A user catalog overrides a mapping | an overlay patching `models[].dials.creativity.levels.focused` | Merged as a JSON merge patch. Every mapped value must pass the row's validation and be expressible by its adapter, or the catalog fails to load. The configuration hash covers the compiled mapping. A repository layer may not set `dials`. | load-time `invalid_dial` |

## Known limits

- **Native response schemas.** Dials travel to the provider as request options (`types.OptionsProvider.ChatStreamWithOptions`), and that call has no response schema parameter. A tool-free call that carries a native schema (`OutputNative`, or auto mode on a provider with native structured output and no tools) therefore goes through `ChatStreamWithSchema`, and the agent does not send its dials on it; it logs a warning instead. Carrying both would need a new provider interface that every adapter implements and every decorator (retry, cache, tracing, attempt deadline, fallback, split, router) forwards. Putting the schema inside `RequestOptions` instead would let an options provider that does not know the field drop the schema silently, which D-12 rules out. Until then, set dials that must cover such calls on the provider: `provider.Config.Dials`, a preset's or entry's `dials`, or an adapter's `WithDials`. Those are compiled by the adapter on every path, the schema path included.
- **Deferral needs a starting value.** A signed tool loop holds the reasoning dial that was in force when it opened. If no reasoning dial was set then, the model ran on its default, which no dial value names, so a reasoning change inside that loop is not deferred.
- **Cache reset after a reload.** `cache_reset_expected` compares against the options last sent in the same routing session, which is not saved with the tree. The first call after a reload does not report it.

