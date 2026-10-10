# Model catalog and presets

The catalog is data. It declares each model, the endpoints that serve it, and what each **offering** (a model on one endpoint) accepts: parameters and their ranges, media and their limits, server tools, service tiers and prices. It also names **presets**: ordered provider chains in which every entry carries the options resolved for its own model. The SDK ships one catalog, embedded at build time from `agent/provider/catalog/data/default.json`. That file is the single source of truth for what `catalog.Lookup` returns. Hosts layer their own catalogs on top and load them from anywhere: a file, an HTTPS endpoint, an object store, or a database.

- [File format](#file-format)
  - [Models](#models), [Endpoints](#endpoints), [Offerings](#offerings), [Parameters and constraints](#parameters-and-constraints), [Modalities](#modalities), [Service tiers and pricing](#service-tiers-and-pricing)
  - [Migrating a version 1 catalog](#migrating-a-version-1-catalog)
- [Sources and layers](#sources-and-layers)
- [Presets](#presets)
- [Precedence](#precedence)
- [Validation](#validation)
- [Building a preset](#building-a-preset)
- [Recording the serving configuration](#recording-the-serving-configuration)
- [CLI](#cli)
- [Freshness check](#freshness-check)

## File format

A catalog is one JSON object. The JSON Schema is `agent/provider/catalog/catalog.schema.json` (`saige catalog schema` prints it), so editors can complete and check files.

The catalog separates three things that change for different reasons: the **model** (what the weights can do), the **endpoint** (where and how the model is reached), and the **offering** (a model served through one endpoint: what may actually be sent there, and what it costs). The same model can be offered on several endpoints with different parameters, media limits and prices, such as Gemini on the Gemini API and on Vertex AI.

| Key | Meaning |
| --- | --- |
| `version` | Required. Major schema version, currently `2`. A version 1 file still loads, with a `v1_catalog` warning (see [Migrating a version 1 catalog](#migrating-a-version-1-catalog)). Readers reject other majors. |
| `revision` | Free-form label, recorded in route events, traces and eval provenance. |
| `inherit_default` | `false` starts this layer from an empty catalog. Default `true`. |
| `model_templates` | Partial models that models reach only through `extends`. |
| `models` | Model families keyed by `"<vendor>/<prefix>"`. |
| `endpoints` | Where models are reached, keyed by name. |
| `offering_templates` | Partial offerings that offerings, endpoint defaults and endpoint overrides reach through `extends`. |
| `offerings` | Explicit model and endpoint pairs. |
| `presets` | Named configurations. |
| `default_preset` | Used by the CLI when neither `--preset` nor `--model` is given. |
| `dials` | Global [dials](dials.md), the lowest dial layer of every chain entry. |

### Models

A model key matches model IDs by longest prefix within its vendor. Only an exact match is a declaration (`Known` true); a dated or tagged ID inherits the family with `Known` false.

```json
"models": {
  "openai/gpt-6-luna": {
    "extends": "openai.reasoning", "tier": "economy",
    "limits": { "context_window": 1050000, "max_output_tokens": 128000 },
    "modalities": { "in": ["text", "image"] }
  }
}
```

Model fields: `extends` (a model template; chains up to four deep, cycles rejected), `tier` (`frontier`, `standard` or `economy`), `superseded_by`, `limits` (`context_window`, `max_output_tokens`), `modalities` (`in` and `out` lists of `text`, `image`, `audio`, `video`, `document`, `file`) and `notes`. An offering's modalities are narrowed to the ones its model declares.

### Endpoints

```json
"endpoints": {
  "google-vertex": {
    "surface": "vertex", "serves": ["google"],
    "location": { "region": "env:GOOGLE_CLOUD_LOCATION", "project": "env:GOOGLE_CLOUD_PROJECT" },
    "auth": { "type": "adc" },
    "files": { "uri_schemes": ["gs", "https"] },
    "modes": { "batch": true },
    "default_offering_template": "baseline.google",
    "inherit_offerings": "google-gemini"
  }
}
```

| Field | Meaning |
| --- | --- |
| `surface` | The API the endpoint speaks: `anthropic.messages`, `openai.chat`, `openai.responses`, `openai.compatible`, `gemini.api`, `vertex` or `ollama.native`. Required. |
| `serves` | The vendors whose models the endpoint serves. |
| `primary` | The endpoint `catalog.Lookup` resolves a vendor's models on. At most one per vendor. |
| `location` | `region` and `project`. A value `env:NAME` is read from the environment when an entry is built. |
| `auth` | `type` (`api_key`, `adc` or `none`) and `secret`, a reference such as `env:OPENAI_API_KEY`. A credential itself is rejected. |
| `transport` | `base_url` for a gateway or compatible server, and `timeout` for one attempt. |
| `capacity` | Declared `requests_per_minute`, `tokens_per_minute` and `max_concurrency`. |
| `data` | `zero_retention`, `store`, `residency`, and `pii_ok`, which lets the privacy decorator send raw personal data there (see [modality conversion](modality-conversion.md#privacy)). |
| `files` | The vendor file store (`api`, `max_bytes`, `ttl`) and the `uri_schemes` the endpoint fetches. |
| `modes` | `batch` (the endpoint has a batch API, which a batch tier needs) and `streaming`. |
| `model_ids` | Maps a catalog prefix to the ID this endpoint takes, when they differ. |
| `default_offering_template` | The offering a served model with no other offering here gets, and the baseline for a model the catalog does not list (with `Known` false). |
| `inherit_offerings` | Another endpoint whose offerings this one copies for every model it has no explicit offering for. |
| `overrides` | An offering patch applied to every offering on this endpoint. |

The embedded catalog declares `anthropic`, `openai-chat`, `openai-responses`, `google-gemini`, `google-vertex` and `ollama`. The primary endpoints are `anthropic`, `openai-chat`, `google-gemini` and `ollama`.

### Offerings

An offering is identified as `"<vendor>/<prefix>@<endpoint>"`, for example `openai/gpt-6-luna@openai-chat`. On an endpoint, a model's offering is resolved in this order, lowest first:

1. the offering templates it `extends`, root first;
2. the endpoint's `overrides`;
3. the explicit offering's own fields.

A model with no explicit offering on an endpoint that serves its vendor gets the offering of the `inherit_offerings` endpoint with this endpoint's overrides, or else the `default_offering_template`.

```json
"offerings": [
  {
    "model": "openai/gpt-6-luna", "endpoint": "openai-chat", "extends": "openai.reasoning",
    "params": {
      "temperature": { "allowed": true },
      "reasoning.effort": { "values": ["none", "low", "medium", "high", "xhigh", "max"], "default": "medium", "required": false },
      "prompt_cache.retention": { "type": "enum", "values": ["24h"], "allowed": true, "wire": "prompt_cache_retention" }
    },
    "constraints": {
      "openai.chat.tools": {
        "when": { "request.surface": { "in": ["openai.chat"] }, "request.tools": { "bool": true } },
        "require": { "reasoning.effort": ["none"] },
        "reason": "Chat Completions takes tools only with reasoning effort none"
      }
    },
    "modalities": { "in": { "image": { "media": ["image/jpeg", "image/png", "image/gif", "image/webp"] } } },
    "pricing": { "currency": "USD", "input_per_mtok": 0.1, "output_per_mtok": 0.5, "cached_input_per_mtok": 0.01,
      "cache_write_per_mtok": 0.125, "as_of": "2026-10-09", "source": "vendor list price" },
    "tiers": { "batch": { "transport": "batch", "discount": 0.5, "cached_input_per_mtok": 0.005 } }
  }
]
```

| Field | Meaning |
| --- | --- |
| `extends` | An offering template; chains up to four deep. |
| `features`, `add_features`, `remove_features` | Capabilities that are not request parameters, such as `tools`, `streaming`, `structured_output`, `web_search` or `citations`. A non-empty `features` replaces the inherited list; the other two edit it and accumulate across layers. |
| `params` | The request parameters, merged per parameter and field (see [Parameters and constraints](#parameters-and-constraints)). |
| `constraints` | Rules over several parameters, keyed by name so a child can replace one or remove it with `null`. |
| `modalities` | Input and output modalities with their limits, and how each reaches the model inside a tool result (see [Modalities](#modalities)). |
| `limits` | Caps the model's token limits on this endpoint. |
| `structured_output` | `""`, `native` or `tool_call`. |
| `server_tools`, `server_tool_fees` | Provider-executed tools the offering accepts, and their per-use fees. |
| `pricing` | The standard tier's rate card (see [Service tiers and pricing](#service-tiers-and-pricing)). |
| `tiers` | The other service tiers. |
| `modality_pricing` | Rates for modalities the vendor bills apart from text. |
| `defaults` | Option defaults, the lowest declared layer of a preset entry's options. |
| `dials` | How the offering compiles dials, and its own dial defaults (see [Dials](#dials)). |
| `fallback` | `equivalents` and `larger_context`: offering IDs to consider when this one cannot serve. They never create failover by themselves. |
| `notes` | Caveats shown by `saige models` and `saige catalog show`. |

`catalog.LookupOffering(endpoint, vendor, model)`, `Catalog.Offering` and `Catalog.ResolvedOfferings` return the resolved `types.Offering`. `catalog.Lookup` returns the `types.ModelCapabilities` of the offering on the vendor's primary endpoint, and `ModelCapabilities.Offering` points at the offering it was projected from. See [model capabilities](model-capabilities.md).

### Parameters and constraints

The parameter specification is data. Each entry of `params` is keyed by a parameter name: `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `seed`, `max_output_tokens`, `stop`, `parallel_tools`, `tool_choice`, `reasoning.enabled`, `reasoning.effort`, `reasoning.budget`, `service_tier` or `prompt_cache.retention`.

| Field | Meaning |
| --- | --- |
| `type` | `number`, `integer`, `boolean`, `enum` or `string_list`. |
| `min`, `max` | The accepted range. |
| `values` | The accepted values of an enum, such as the reasoning efforts. |
| `default` | The value the vendor applies when the parameter is not sent. |
| `allowed` | Whether the parameter may be sent. A parameter with a `type` and no `allowed` is accepted. |
| `required` | For a reasoning control: reasoning cannot be turned off. |
| `special` | Values outside the range and their meaning, such as `{"-1": "dynamic", "0": "off"}` for a reasoning budget. |
| `wire` | The vendor's field name, for documentation. |

A constraint restricts parameters together. `when` is a condition over parameters and the request (`request.tools`, `request.surface`), each tested with `in`, `not`, `bool` or `set`. When it holds, `forbid` lists parameters that may not be sent, `require` lists the values a parameter must take, and `exclusive` lists parameters of which at most one may be set. `reason` is the message a rejection carries. The catalog writes its own rules under fixed names: `reasoning.exclusive` (one reasoning control), `sampling.<control>` (sampling forbidden while that reasoning control is active) and `openai.chat.tools` (Chat Completions takes tools only with reasoning effort `none`, or not at all). `Offering.Space()` returns the parameter space, and its `Validate` checks a request against the parameters and constraints.

### Modalities

```json
"modalities": {
  "in": {
    "document": { "media": ["application/pdf"], "sources": ["inline", "uri", "file"], "max_bytes": 52428800, "max_pages": 1000 },
    "video": { "media": ["video/mp4"], "sources": ["inline", "uri", "file"], "fps": { "type": "number", "default": 1 } },
    "audio": null
  },
  "tool_result": { "image": "inline" }
}
```

Each input or output modality lists its `media` types and the `sources` it reads: `inline` bytes, a `uri` the endpoint fetches (with a scheme from the endpoint's `files.uri_schemes`), or a `file` in the endpoint's own store. Limits are `max_bytes`, `max_count`, `max_pixels`, `max_pages`, `max_duration` and `fps`, and a zero limit is undeclared, not unlimited. `tokens` estimates what one piece of media costs (`base`, `per_tile` with `tile`, `per_page`, `per_second`, `per_image`, `per_pixels`), which `types.EstimateTokensFor` uses. `null` removes a modality a template supplies. `tool_result` says how each modality inside a tool result is sent: `inline`, `follow_up_user` (in a user message after the tool results) or `none`.

The [conversion planner](modality-conversion.md) reads these modalities on each attempt: a part is native only when its media type, size, count and locator fit.

### Service tiers and pricing

`pricing` is the standard tier's rate card, per million tokens: `currency`, `input_per_mtok`, `output_per_mtok`, `cached_input_per_mtok`, `cache_write_per_mtok`, `per_request`, `free`, `as_of` (required when a rate is set) and `source`.

`tiers` holds the other service tiers, `priority`, `flex` and `batch`, each with a `discount` (the fraction taken off the standard token rates), a `cached_input_per_mtok` for vendors whose discounts do not stack, or a whole `pricing` card, and the vendor's `wire` name. A tier with `"transport": "batch"` is served through the endpoint's batch API and needs `modes.batch` on the endpoint. The batch tier prices [batch jobs](batch.md#cost-and-budget); interactive calls run on the standard tier. `Offering.TierPricing(tier)` returns a tier's rate card.

`modality_pricing` prices modalities the vendor bills apart from text, per million tokens of that modality: `{"audio": {"input_per_mtok": 0.5}}`. Settlement bills each priced modality at its rate from the usage the vendor reports. Usage of a modality the offering does not price is charged at the text rates as an uncertain lower bound and makes the call unpriced, which a budget with a cost limit refuses unless `AllowUnpriced` is set. The embedded catalog prices modalities for the Gemini models whose price list states them.

### The options object

The same shape appears in offering `defaults`, preset `options` and entry `options`:

| Key | Notes |
| --- | --- |
| `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `seed`, `max_output_tokens`, `stop`, `parallel_tools` | Sampling and limits. |
| `reasoning` | Exactly one of `{"enabled": bool}`, `{"effort": "high"}` or `{"budget": 2048}`. |
| `tool_choice` | `auto` or `none` only. A forced choice in options would force every turn; set it on the preset's `tool_choice`, which the agent applies to one turn. |
| `prompt_cache` | `{"mode": "off"}`, `{"mode": "markers", "ttl": "5m", "tools": true, "system": true, "conversation": true}` (Anthropic) or `{"mode": "automatic", "retention": "24h", "key": "..."}` (OpenAI). Google and Ollama accept only `off`. An offering that declares `prompt_cache.retention` values rejects any other retention. |
| `server_tools` | `[{"kind": "web_search", "max_uses": 3}]`. A remote MCP server's `url` must be `https` with a host, and its token is never part of a catalog. |

JSON `null` is not accepted inside an options object. Remove an inherited option with the entry's `unset`, so that every removal is written down.

### Dials

[Dials](dials.md) are model-neutral intents such as `{"creativity": "focused", "reasoning": {"depth": "high"}}`. Unlike options, a dial an entry's model cannot honor exactly is mapped to the nearest declared value or dropped when it is advisory, and rejected only when it is contractual (`tools`, `parallel: false`, `reproducible`, `modality`).

A dials object appears in two forms:

| Where | Shape | Meaning |
| --- | --- | --- |
| top-level `dials`, preset `dials`, entry `dials` | the dial values | What to ask every entry, a preset's entries, or one entry for. |
| offering (or offering template) `dials` | `creativity`, `reasoning`, `cache`, `defaults` | How the offering compiles dials to options, and the model's own dial defaults. |

An offering needs no `dials`: its mapping is derived from its parameters, its sampling constraints and its prompt cache features. Declare one only to override a part of it:

| Key | Meaning |
| --- | --- |
| `creativity.levels` | An options object per level (`deterministic`, `focused`, `balanced`, `creative`). |
| `creativity.requires_reasoning_off` | Drop creativity while reasoning is active. The Anthropic thinking offerings declare it. |
| `reasoning.off`, `reasoning.on`, `reasoning.adaptive` | The options object for each mode. `{}` means the mode is expressed by sending nothing. |
| `reasoning.depth` | An options object per depth (`minimal`, `low`, `medium`, `high`, `max`). A depth missing here maps to the nearest declared one. |
| `reasoning.depth_change` | `per_request` when a depth change applies even inside a tool loop with signed reasoning. |
| `reasoning.change_resets_cache` | A reasoning change invalidates the provider's cached prompt prefix. The Anthropic thinking offerings declare it. |
| `reasoning.with_tools` | Options per API surface (`chat`, `responses`) that replace the compiled reasoning when the request offers tools. |
| `cache.on` | The `prompt_cache` object the cache dial turns on. |
| `defaults` | The model's own dial values, below the preset's. |

A declared level, depth or surface overrides the derived one of the same key, so an overlay can patch an offering's `dials.creativity.levels.focused` and keep the rest. Templates and offerings merge `dials` field by field, and maps key by key.

### Migrating a version 1 catalog

Version 1 files kept one row per model with capability flags, a `reasoning` block, `media` and batch pricing in `pricing`. They still load: `catalog.UpgradeV1` converts each layer in memory and reports a `v1_catalog` warning. Convert the file once:

```
saige catalog migrate team.json           # print the version 2 form
saige catalog migrate --write team.json   # rewrite the file in place
```

Templates split into model and offering templates, rows into models and offerings on each vendor's primary endpoint, baselines into the endpoints' default offering templates, and `pricing.batch_discount` and `batch_cached_input_per_mtok` into the batch tier. In Go, the version 1 types are `catalog.CatalogV1` and `catalog.ModelSpecV1`. `saige catalog reconcile --write` refuses a version 1 file until it is migrated.

## Sources and layers

A `catalog.Source` produces one layer:

```go
type Source interface {
	Load(ctx context.Context) (*Catalog, error)
}
```

| Source | Use |
| --- | --- |
| `catalog.EmbeddedSource()` | The shipped default. |
| `catalog.FileSource(path)` | A local file; `file://` is accepted. |
| `catalog.HTTPSource(url, catalog.HTTPOptions{})` | HTTPS only unless `AllowInsecure`; sends `If-None-Match` with the last ETag, so an unchanged catalog costs a 304 and is parsed again from the cached body. Bounded by `Timeout` (10s) and `MaxBytes` (4 MiB). Errors and installed revisions name the URL with its user information and query string redacted (`catalog.RedactURL`). |
| `catalog.ReaderSource(open)` | Any reader: an object store, a database blob. |
| `catalog.StaticSource(c)` | A value the host already holds. |
| `catalog.Layered(sources...)` | Merges its sources in order. |

A failing source returns an error that names it. Wrap a source with `catalog.Named(name, src)` to choose that name.

Load and install in one call:

```go
src := catalog.Layered(catalog.EmbeddedSource(), catalog.FileSource("catalog.json"))
if _, err := catalog.Use(ctx, src); err != nil {
	return err
}
```

`Use` validates the merged result and installs its models and offerings, so `catalog.Lookup` and every adapter see them. `catalog.Refresh(ctx, src)` reinstalls only when the content changed; call it on a timer. `catalog.LoadSource` loads and validates without installing.

### Loading from S3

The SDK has no cloud storage dependency. An object store is a two-line `ReaderSource`:

```go
s3src := catalog.Named("s3://team-config/catalog.json", catalog.ReaderSource(
	func(ctx context.Context) (io.ReadCloser, error) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("team-config"), Key: aws.String("catalog.json")})
		if err != nil {
			return nil, err
		}
		return out.Body, nil
	}))
_, err := catalog.Use(ctx, catalog.Layered(catalog.EmbeddedSource(), s3src))
```

### Merge rules

- **Models, templates, endpoints and offerings** merge field by field. The overlay value with the same key (an offering's key is its model and endpoint) is a JSON merge patch (RFC 7396) on the base value: a present key replaces, an absent key keeps the base value, and arrays replace whole. `null` removes a whole model, template or endpoint; inside one, a `null` map entry (a parameter, a constraint, a modality, a tier) is kept, so it also removes what a template would supply. `add_features` and `remove_features` accumulate across layers. `"$replace": true` replaces the whole value and `"$delete": true` removes it; deleting a model removes its offerings.
- A version 1 layer is upgraded before it is merged.
- **Presets** merge entry by entry. An overlay preset replaces the base preset of the same name whole, and `null` deletes it. To change one field, write a new preset with `"extends": "<base>"`, so every preset is a configuration someone wrote.
- The recorded revision joins every layer's revision with `+`, the topmost `default_preset` wins, and `inherit_default: false` drops every layer below.

## Presets

```json
"balanced": {
  "description": "General assistant with cross-vendor failover",
  "options": { "max_output_tokens": 8192 },
  "tool_choice": "auto",
  "output_mode": "auto",
  "llm_timeout": "2m",
  "retry": { "max_attempts": 3, "base_delay": "500ms", "max_delay": "10s" },
  "routing": { "policy": "affinity" },
  "chain": [
    { "id": "primary", "provider": "anthropic", "model": "claude-sonnet-5-5",
      "options": { "reasoning": { "effort": "medium" },
                   "prompt_cache": { "mode": "markers", "ttl": "5m", "system": true, "tools": true } } },
    { "id": "openai", "offering": "openai/gpt-6-luna@openai-responses",
      "options": { "temperature": 0.3, "reasoning": { "effort": "none" } } },
    { "id": "local", "provider": "ollama", "model": "qwen3.5:4b", "optional": true, "local_fallback": true,
      "options": { "temperature": 0.3, "reasoning": { "enabled": false } }, "retry": { "disable": true } }
  ]
}
```

Preset keys: `description`, `extends`, `options`, `dials`, `tool_choice` (`auto`, `none`, `required` or `named:<tool>`), `output_mode` (`auto`, `native`, `tool` or `prompt`), `llm_timeout`, `compaction` (the agent's compaction strategy; see [context management](context-management.md#presets)), `retry`, `routing`, `require_declared` and `chain`.

Routing keys:

| Key | Meaning |
| --- | --- |
| `policy` | `sticky` (the default) or `affinity`. |
| `fail_threshold` | Consecutive failed requests before a session leaves its entry. Zero means 1. Affinity only. |
| `reprobe_after` | Requests served away from the primary before the session tries the primary again, so it returns once the primary recovers. A reprobe waits while a warm prompt-cache prefix makes the switch cost more. Zero never reprobes. Affinity only. |
| `failover_on_content_filter` | A content-filter refusal moves to an entry of another provider. Default `false`. |
| `failover_on_auth` | An authentication or permission failure moves to the next entry. Default `false`: a bad key fails the request instead of hiding behind another vendor. |
| `required` | Capabilities every request needs. |

Setting `fail_threshold` or `reprobe_after` without a `policy` selects `affinity`; with `policy: sticky` it is an error. Other permanent errors never fail over.

Chain entry keys: `id` (default `provider/model`), `offering` (`"<vendor>/<model>@<endpoint>"`, which sets the provider, model and endpoint together), `endpoint` (with `model`; empty uses the vendor's primary endpoint), `provider`, `model`, `options`, `dials`, `unset` (option names, and inherited dials as `dials.<name>`), `inherit` (`all` or `none`, which also skips the preset's dials), `retry`, `attempt_timeout`, `base_url`, `api_key_env` (the name of the variable; a key itself is rejected), `vertex`, `optional` (drop the whole entry when it cannot serve) and `local_fallback` (Ollama entries only: serve another pulled model when this one is not pulled).

`vertex` serves a Google entry through Vertex AI instead of the Gemini API: `{"project": "...", "location": "..."}`. Empty fields default from `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION`, and the location then defaults to `global`. Vertex authenticates with Application Default Credentials, so the entry needs a project, not an API key. Setting `GOOGLE_GENAI_USE_VERTEXAI=true` serves every Google entry through Vertex the same way. A `vertex` block on another provider's entry is rejected. An optional entry is dropped when its credentials are missing, and an optional entry that needs none (a local Ollama model) is dropped when its server does not answer a reachability check at build time (`preset.Options.Probe`, by default `preset.ProbeOllama`).

`local_fallback` lets an Ollama entry run on whatever the local server has. At build time `preset.Build` lists the pulled models (`preset.Options.ListLocal`, by default `preset.ListOllama`, which reads `/api/tags`). It keeps the entry's model when it is pulled, otherwise serves the first pulled model the catalog says calls tools, otherwise the first pulled model that is not an embedding model, and records a `local_model_substituted` warning. The entry keeps its ID, and resolves against the offering of the model that serves. With no chat model pulled, the entry fails with `preset.ErrNoLocalModel`, whose message says to run `ollama pull <model>`, or is dropped when it is optional. A server that cannot be listed leaves the entry as written. The shipped `default` and `ollama` presets set it.

## Precedence

Options resolve field by field, lowest first:

1. Provider default: nothing is sent and the adapter's own default applies (for example Anthropic's `max_tokens` of 4096). Reported with origin `provider`.
2. The offering's `defaults`.
3. The preset's `options`, unless the entry says `"inherit": "none"`.
4. The entry's `options`.
5. The entry's `unset` removes inherited names. Unsetting a name the same entry sets is an error.
6. A per-request override at call time, merged by the adapter. It is not part of the configuration hash.

There is one reasoning control: a higher layer that sets any `reasoning` key replaces the lower layer's reasoning whole.

Dials resolve the same way in their own layers: the top-level `dials`, the offering's `dials.defaults`, the preset's `dials` (skipped with `"inherit": "none"`), the entry's `dials`, then the entry's `unset`. The adapter compiles them on each request against its own model, under any dials the agent or the request adds ([precedence](dials.md#setting-dials)). An option that sets the same parameter as a dial wins over it. When the merged dials turn `cache` on and no `prompt_cache` option is set, the offering's prompt cache is selected, with origin of the dial's layer.

## Validation

Loading is strict. Unknown keys (with a suggestion), duplicate keys, wrong kinds, bad durations, unknown feature, parameter, modality, media and server tool names, unknown surfaces and service tiers, secret references that are not `env:NAME`, a second primary endpoint for a vendor, template cycles, secrets and trailing data are reported with a path such as `presets.balanced.chain[2].options.reasoning.budget`. Errors are a `*catalog.ValidationError` and match `catalog.ErrInvalidCatalog`.

Every chain entry is then checked against its own model:

- the offering's parameters, ranges and constraints (`ModelCapabilities.ValidateOptions`);
- what the adapter can send (`catalog.Expressible`, the same table `provider.Build` uses);
- the prompt cache mode and server tools against the model and adapter;
- the preset's `tool_choice` and `output_mode` (`native` needs native structured output);
- `require_declared`, which requires an exact model.

An option an entry cannot honor is rejected, never stripped. A preset whose `temperature` reaches a `gpt-6.1-sol` entry fails with:

```
presets.p.chain[1].options.temperature: not declared supported for openai/gpt-6.1-sol (inherited from preset options)
```

On `gpt-6-luna`, which accepts sampling only with reasoning effort `none` and defaults to `medium`, the same option fails with `requires reasoning to be disabled` unless the entry also sets `"reasoning": {"effort": "none"}`.

The fix is written in the file: `"unset": ["temperature"]`, an entry override, or `"inherit": "none"`. A preset with more than one entry whose shared options set `temperature`, `top_p`, `top_k` or `reasoning` also gets a `prefer_dial` warning: the creativity and reasoning dials adapt to each entry's model instead.

Dials are checked as well:

- every options object an offering declares for a dial must pass the offering's own validation and be expressible by its adapter, so a bad mapping fails at load time (`invalid_dial` at, for example, `offerings[3].dials.reasoning.depth.high`);
- each entry's dials are compiled for its own model as a request without tools would be. A contractual dial the model cannot honor is an error that names the layer it came from; an advisory dial that is mapped or dropped is a `dial_mapped` or `dial_dropped` warning, and a dial a raw option overrides is a `dial_overridden` warning.

The configuration hash covers an entry's dials and what they compile to, so changing an offering's mapping changes the hash. An entry on an endpoint other than its vendor's primary has a different hash from the same model on the primary. An entry without dials keeps the hash it had before dials existed.

Warnings do not fail validation: an inferred model, a fallback with a smaller context window than the primary, a superseded model, an unpriced entry under a budget, and a primary whose signed reasoning blocks failover during a tool loop.

## Building a preset

```go
cat, err := catalog.LoadSource(ctx, catalog.Layered(catalog.EmbeddedSource(), catalog.FileSource("catalog.json")))
if err != nil {
	return err
}
bundle, err := preset.Build(ctx, cat, "balanced", []types.PresetName{"deterministic-extract"}, preset.Options{})
if err != nil {
	return err
}
defer bundle.Close(ctx)

a, err := agent.New(agent.Config{SystemPrompt: "..."}, agent.WithPreset(bundle))
if err != nil {
	return err
}
```

`preset.Build` builds every entry with `provider.Build` and its own resolved options, wraps it in its own retry decorator and optional per-attempt deadline, and puts all entries behind one router. Each preset is a router group in chain order, and the primary is the default group, so failover follows the chain exactly. `preset.BuildFrom(ctx, src, ...)` takes a `Source`. Neither installs anything: hosts touch global state only through `catalog.Install` or `catalog.Use`.

`agent.WithPreset` sets the provider and the preset's tool choice, output mode, LLM timeout and compaction unless they were already set. Options applied after it win.

`ConfigPart.Target` (or an outcome policy's switch) can name another built preset (`types.PresetTarget`) or a single profile (`types.ProfileTarget`). The router selects that complete configuration; no options are copied onto another model. A signed-reasoning lock still keeps the previous profile. A target the router does not define fails with an error matching `router.ErrUnknownProfile` and `types.ErrUnknownTarget`. `fallback.Provider` and `split.Split` accept only model targets, which they copy to every member, so do not wrap a bundle in them; use `bundle.Session()` as a split arm instead.

`bundle.ConfigKey(target)` hashes the configuration hashes of a preset or profile target with the catalog revision, for `cache.Config.ConfigKey`.

## Recording the serving configuration

Every attempt's `types.RouteDelta` carries `Preset`, `ConfigHash`, `CatalogRevision`, the effective `Options` (the entry's options merged with the request override, every dial compiled) and, when the attempt had dials, a `Dials` report of each decision ([dials](dials.md#seeing-what-was-sent)). The agent attaches the last route of the committed call to the assistant turn as `types.RoutePart`, which is persisted with the tree and stripped before provider calls. Traces add `saige.route.preset`, `saige.route.config_hash` and `saige.catalog.revision`, a `saige.route.attempt` event per attempt, and set `gen_ai.request.*` from the serving attempt. Evals record served configurations with `AgentRun.AddProvenance`, and `eval.WithProvenance` makes a comparison warn when the same profile ran with a different hash.

## CLI

Layers merge in this order, lowest first:

1. the embedded default;
2. `$XDG_CONFIG_HOME/saige/catalog.json` (default `~/.config/saige/catalog.json`);
3. `.saige/catalog.json` in the project, found by walking up to the repository root;
4. `SAIGE_CATALOG`, one reference or a list separated by the OS path list separator;
5. each `--catalog` flag.

A reference is a path, a `file://` URL or an `https://` URL. Missing user and project files are skipped. A file named by more than one layer, compared by its cleaned absolute path, is loaded once, at its highest place in this order.

A project file can arrive with a cloned repository, so it is checked against an allowlist. It may set models, templates, offerings, presets, options and routing policy, and describe endpoints, but not:

- `base_url` or `api_key_env` on a chain entry, which could send your key to another host;
- `mcp_server` on a server tool, wherever options appear (preset, entry, offering defaults, offering templates), which makes the provider connect to that server;
- `routing.failover_on_content_filter` or `routing.failover_on_auth`, which send a refused or failed request to another vendor;
- `inherit_default: false`, which discards the trusted layers below;
- `vertex.project` or `vertex.location` on a chain entry, which bill and send prompts to a Google Cloud project you did not choose, with your own credentials. An empty `vertex` block, which uses your environment, is allowed;
- an endpoint's `transport`, `auth`, `location`, `data`, `model_ids` or `primary`, which redirect requests, credentials or personal data;
- a preset's `compaction`, which chooses what the agent forgets and which model it pays to summarize with;
- `dials`, at the top level, on an offering or offering template, on a preset or on an entry, which can turn on prompt caching or raise reasoning spend.

A field the allowlist does not name is refused too. Such a file fails validation unless `SAIGE_TRUST_PROJECT_CATALOG=1` is set or the same file is named with `--catalog`, which loads it once, as a trusted explicit layer. Explicit references, including remote URLs, are trusted.

```
saige --preset balanced chat
saige catalog show balanced           # per entry: endpoint, offering, modalities, tiers, options with origins, hash
saige catalog show openai/gpt-6-luna  # a one-entry chain from the model's defaults
saige catalog explain default --dials '{"creativity":"focused"}'  # per entry: dial decisions and raw options sent
saige catalog validate --strict       # exit 1 on errors, or on warnings with --strict
saige catalog validate --dry-build    # also build every adapter with a placeholder key
saige catalog layers                  # paths, trust state and revisions
saige catalog schema                  # the JSON Schema
saige catalog export                  # the merged catalog as canonical JSON
saige catalog reconcile               # compare provider model lists with the catalog
saige catalog migrate --write FILE    # convert a version 1 layer to version 2
```

The CLI picks `--preset` first, then `--model` (a one-entry chain from the offering's defaults), then `--provider` alone (the preset of that name), then `default_preset`. Without `--preset` or `--model`, the CLI runs one vendor: the first entry of `default_preset` that has credentials, or for the local Ollama entry, a server that answers. The shipped order is Anthropic, OpenAI, Google, then Ollama, and each vendor's entry is its cheapest current model. With no key set and Ollama not running, the CLI says which variables to set or to start Ollama; with Ollama running but no chat model pulled, it says to run `ollama pull qwen3.5:4b`. Cross-vendor failover is opt-in: `--preset default` runs the whole chain.

The shipped presets:

| Preset | Chain |
| --- | --- |
| `default` | claude-haiku-5-5, gpt-6-luna, gemini-3.1-flash-lite, then a local qwen3.5:4b (or another pulled model) |
| `anthropic`, `openai`, `google` | claude-haiku-5-5, gpt-6-luna, gemini-3.1-flash-lite |
| `anthropic-quality`, `openai-quality`, `google-quality` | claude-sonnet-5-5, gpt-6.1-sol, gemini-3.8-flash |
| `vertex` | gemini-3.1-flash-lite through Vertex AI |
| `ollama` | a local qwen3.5:4b, or another pulled model |

`--provider vertex` runs the `vertex` preset, or with `--model` a one-entry Google chain served through Vertex AI. When `GOOGLE_GENAI_USE_VERTEXAI=true` and neither the Anthropic nor the OpenAI key is set, the CLI detects `vertex` as the provider, and `--embed-provider` defaults to it as well.

`--base-url` applies to the entries of the selected provider: `--provider` when given, otherwise the one provider the chain uses. On a chain that spans several vendors without `--provider` it is an error, never ignored.

## Freshness check

Vendors add and retire models on their own schedule, and no list endpoint reports prices. `saige catalog reconcile` compares what each provider lists with the active catalog, and a monthly workflow turns what it finds into a pull request.

### Reconcile

```
saige catalog reconcile                                  # anthropic, openai, google
saige catalog reconcile --providers openai,ollama        # ollama is checked only when named
saige catalog reconcile --ignore-file .github/catalog-ignore.txt --write agent/provider/catalog/data/default.json
saige --format json catalog reconcile                    # machine-readable report
```

A provider is checked when its credentials are set: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and for Google either `GOOGLE_API_KEY` (or `GEMINI_API_KEY`) or `GOOGLE_GENAI_USE_VERTEXAI=true` with `GOOGLE_CLOUD_PROJECT`. A provider without credentials is reported as skipped. The report lists:

| Finding | Meaning | Drift |
| --- | --- | --- |
| New models | Listed models that only the endpoint baseline covers. | yes |
| Limit drift | The endpoint reports a context window or output cap that disagrees with the model (Anthropic and Google report limits; OpenAI does not). | yes |
| Disappeared upstream | Models no listed model matches and that set no `superseded_by`: candidates for `superseded_by` or removal. A model the key cannot access is missing too, so check before acting. Not reported for Ollama, which lists only pulled weights. | yes |
| Pending review | Models or offerings that still carry the note `needs review`. | yes |
| Stale pricing | Prices and server tool fees whose `as_of` is older than `--stale-days` (90), or is not a date. | no, a reminder |

Dated or tagged IDs that a family covers (`gpt-6-luna-2026-09-01`) are inferred, not new. `--ignore` and `--ignore-file` take globs over model IDs and model prefixes, bare or as `provider/glob`, for models the catalog deliberately does not describe, such as speech, image and moderation endpoints.

Exit status: `0` no drift, `1` drift, `2` error (a listing failed, no provider was configured, or a flag or file was invalid). The human report is Markdown, so it reads in a terminal and serves as a pull request body.

`--write <path>` adds a stub for each new model to a version 2 catalog file, or creates the file as an overlay layer when it does not exist. A version 1 file is refused until it is migrated. A stub is:

- a model with no facts, and an offering on the vendor's primary endpoint that extends the endpoint's default offering template, so it declares nothing the baseline does not already assume;
- no tier, limits, successor, defaults or price;
- the note `needs review` on both.

One stub covers the dated IDs that start with its prefix. Models already in the file are skipped, the stubs are inserted before the closing brace of the `models` object and the closing bracket of the `offerings` array, and no existing byte of the file changes. Reconcile never edits an existing model's or offering's prices or capabilities. Once a stub is in the catalog, the exact model ID resolves with `Known` true, so a stub must be completed before it is merged; until its note is removed, every later run reports it as pending review.

### The scheduled workflow

`.github/workflows/catalog-check.yml` runs on the first of each month and on demand (`workflow_dispatch`):

1. It checks each provider whose secret is set (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`) and posts a notice for each one it skips.
2. It runs reconcile with `.github/catalog-ignore.txt` and `--write agent/provider/catalog/data/default.json`, and adds the report to the job summary.
3. On drift with new stubs, it commits them to the `catalog/reconcile` branch and opens a pull request whose body is a review checklist followed by the report. When that pull request is already open, it only refreshes the body, so a reviewer's edits on the branch are never overwritten.
4. On drift with no stubs to propose (only disappeared models or limit drift), the job fails with the report in its summary, so the finding is not lost.

The pull request is opened with the release app's token when `SR_RELEASER_PRIVATE_KEY` is set, so CI runs on it; otherwise with `GITHUB_TOKEN`, and CI must be started by hand (close and reopen the pull request, or push to the branch).

### Reviewing a catalog pull request

For each stub:

1. Look the model up on the vendor's model page. On the model, set `tier`, `limits`, `modalities` and `extends`. On the offering, set `extends` (a template such as `openai.reasoning` or `anthropic.adaptive` usually fits better than the baseline), then adjust `features`, `params` and `modalities`. Declare only what the vendor documents.
2. Price the offering from the vendor's price list with `as_of` and `source`, add its `tiers` and `modality_pricing`, or leave it unpriced; a budget refuses an unpriced model, which is the safe failure.
3. Remove the `needs review` notes. Keep a note for anything a caller should know.

For each disappeared model, set `superseded_by` to the current family, or add the prefix to `.github/catalog-ignore.txt` when the key simply cannot see it. For limit drift, confirm the new limit on the vendor's page before changing the model. Refresh stale prices the same way, updating `as_of`.

Then bump `revision`, regenerate the lookup golden file (`go test ./agent/provider/catalog -run TestDefaultLookupGolden -update`) and review its diff, run `saige catalog validate --dry-build`, and merge. CI fails while any model or offering in the embedded catalog still carries `needs review`.
