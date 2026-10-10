# Model catalog and presets

The catalog is data. It declares what each model accepts (capabilities, limits, reasoning controls, server tools, prices) and names **presets**: ordered provider chains in which every entry carries the options resolved for its own model. The SDK ships one catalog, embedded at build time from `agent/provider/catalog/data/default.json`. That file is the single source of truth for what `catalog.Lookup` returns. Hosts layer their own catalogs on top and load them from anywhere: a file, an HTTPS endpoint, an object store, or a database.

- [File format](#file-format)
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

| Key | Meaning |
| --- | --- |
| `version` | Required. Major schema version, currently `1`. Readers reject other majors. |
| `revision` | Free-form label, recorded in route events, traces and eval provenance. |
| `inherit_default` | `false` starts this layer from an empty catalog. Default `true`. |
| `templates` | Partial rows that model rows reach only through `extends`. |
| `models` | Rows keyed by `provider` and `prefix`. |
| `baselines` | The conservative row per provider for unknown models. |
| `presets` | Named configurations. |
| `default_preset` | Used by the CLI when neither `--preset` nor `--model` is given. |
| `dials` | Global [dials](dials.md), the lowest dial layer of every chain entry. |

A model row matches model IDs by longest prefix. Only an exact match is a declaration (`Known` true); a dated or tagged ID inherits the family row with `Known` false.

```json
{
  "provider": "openai", "prefix": "gpt-6-luna", "extends": "openai.reasoning", "tier": "economy",
  "chat_completions_tools": "no_reasoning", "add_capabilities": ["temperature", "top_p"],
  "limits": { "context_window": 1050000, "max_output_tokens": 128000 },
  "reasoning": { "efforts": ["none", "low", "medium", "high", "xhigh", "max"], "default_effort": "medium",
    "required": false, "sampling_requires_no_reasoning": ["temperature", "top_p"] },
  "pricing": { "currency": "USD", "input_per_mtok": 0.1, "output_per_mtok": 0.5, "cached_input_per_mtok": 0.01,
    "cache_write_per_mtok": 0.125, "as_of": "2026-10-09", "source": "vendor list price" }
}
```

Row fields: `extends` (a template; chains up to four deep, cycles rejected), `tier`, `superseded_by`, `chat_completions_tools`, `capabilities` (replaces the inherited list), `add_capabilities`, `remove_capabilities`, `limits`, `reasoning` (`efforts`, `default_effort`, `required`, `default_enabled`, `min_budget`, `max_budget`, `dynamic_budget`, `zero_budget`, `sampling_requires_no_reasoning`, `forced_tool_choice`), `structured_output` (`""`, `native` or `tool_call`), `media`, `server_tools`, `server_tool_fees`, `pricing` (`as_of` is required when a rate is set), `defaults`, `dials` and `notes`.

Two fields describe request shapes a vendor rejects for one model:

| Field | Meaning |
| --- | --- |
| `reasoning.forced_tool_choice: false` | The API rejects a `required` or named tool choice for the model (`ModelCapabilities.RejectsForcedToolChoice`). Validation rejects such a choice locally, and the Anthropic adapter sends schema output as `output_config.format` instead of a forced tool. Declared on claude-sonnet-5-5, claude-opus-5-5, claude-fable-5-1 and claude-mythos-5-1. |
| `chat_completions_tools` | How OpenAI's Chat Completions API takes tools for the model: `any` (the default), `no_reasoning` (only with reasoning effort `none`; the chat adapter sends `none` when tools are offered and no effort is set, and rejects another effort) or `responses_only` (tools need the Responses API; `provider.Build` serves the model through `openai.NewResponsesAdapter`). |

### The options object

The same shape appears in model `defaults`, preset `options` and entry `options`:

| Key | Notes |
| --- | --- |
| `temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `seed`, `max_output_tokens`, `stop`, `parallel_tools` | Sampling and limits. |
| `reasoning` | Exactly one of `{"enabled": bool}`, `{"effort": "high"}` or `{"budget": 2048}`. |
| `tool_choice` | `auto` or `none` only. A forced choice in options would force every turn; set it on the preset's `tool_choice`, which the agent applies to one turn. |
| `prompt_cache` | `{"mode": "off"}`, `{"mode": "markers", "ttl": "5m", "tools": true, "system": true, "conversation": true}` (Anthropic) or `{"mode": "automatic", "retention": "24h", "key": "..."}` (OpenAI). Google and Ollama accept only `off`. |
| `server_tools` | `[{"kind": "web_search", "max_uses": 3}]`. A remote MCP server's `url` must be `https` with a host, and its token is never part of a catalog. |

JSON `null` is not accepted inside an options object. Remove an inherited option with the entry's `unset`, so that every removal is written down.

### Dials

[Dials](dials.md) are model-neutral intents such as `{"creativity": "focused", "reasoning": {"depth": "high"}}`. Unlike options, a dial an entry's model cannot honor exactly is mapped to the nearest declared value or dropped when it is advisory, and rejected only when it is contractual (`tools`, `parallel: false`, `reproducible`).

A dials object appears in three places:

| Where | Shape | Meaning |
| --- | --- | --- |
| top-level `dials`, preset `dials`, entry `dials` | the dial values | What to ask every entry, a preset's entries, or one entry for. |
| model row (or template) `dials` | `creativity`, `reasoning`, `cache`, `defaults` | How the row compiles dials to options, and the model's own dial defaults. |

A row needs no `dials`: its mapping is derived from `reasoning`, the sampling capabilities and the prompt cache capabilities. Declare one only to override a part of it:

| Key | Meaning |
| --- | --- |
| `creativity.levels` | An options object per level (`deterministic`, `focused`, `balanced`, `creative`). |
| `creativity.requires_reasoning_off` | Drop creativity while reasoning is active. The Anthropic thinking rows declare it. |
| `reasoning.off`, `reasoning.on`, `reasoning.adaptive` | The options object for each mode. `{}` means the mode is expressed by sending nothing. |
| `reasoning.depth` | An options object per depth (`minimal`, `low`, `medium`, `high`, `max`). A depth missing here maps to the nearest declared one. |
| `reasoning.depth_change` | `per_request` when a depth change applies even inside a tool loop with signed reasoning. |
| `reasoning.change_resets_cache` | A reasoning change invalidates the provider's cached prompt prefix. The Anthropic thinking rows declare it. |
| `reasoning.with_tools` | Options per API surface (`chat`, `responses`) that replace the compiled reasoning when the request offers tools. |
| `cache.on` | The `prompt_cache` object the cache dial turns on. |
| `defaults` | The model's own dial values, below the preset's. |

A declared level, depth or surface overrides the derived one of the same key, so an overlay can patch `models[].dials.creativity.levels.focused` and keep the rest. Templates and rows merge `dials` field by field, and maps key by key.

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

`Use` validates the merged result and installs its rows, so `catalog.Lookup` and every adapter see them. `catalog.Refresh(ctx, src)` reinstalls only when the content changed; call it on a timer. `catalog.LoadSource` loads and validates without installing.

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

- **Rows, templates and baselines** merge field by field. The overlay row with the same key is a JSON merge patch (RFC 7396) on the base row: a present key replaces, `null` deletes, an absent key keeps the base value, and arrays replace whole. `add_capabilities` and `remove_capabilities` accumulate across layers instead. `"$replace": true` replaces the whole row and `"$delete": true` removes it.
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
    { "id": "openai", "provider": "openai", "model": "gpt-6-luna",
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

Chain entry keys: `id` (default `provider/model`), `provider`, `model`, `options`, `dials`, `unset` (option names, and inherited dials as `dials.<name>`), `inherit` (`all` or `none`, which also skips the preset's dials), `retry`, `attempt_timeout`, `base_url`, `api_key_env` (the name of the variable; a key itself is rejected), `vertex`, `optional` (drop the whole entry when it cannot serve) and `local_fallback` (Ollama entries only: serve another pulled model when this one is not pulled).

`vertex` serves a Google entry through Vertex AI instead of the Gemini API: `{"project": "...", "location": "..."}`. Empty fields default from `GOOGLE_CLOUD_PROJECT` and `GOOGLE_CLOUD_LOCATION`, and the location then defaults to `global`. Vertex authenticates with Application Default Credentials, so the entry needs a project, not an API key. Setting `GOOGLE_GENAI_USE_VERTEXAI=true` serves every Google entry through Vertex the same way. A `vertex` block on another provider's entry is rejected. An optional entry is dropped when its credentials are missing, and an optional entry that needs none (a local Ollama model) is dropped when its server does not answer a reachability check at build time (`preset.Options.Probe`, by default `preset.ProbeOllama`).

`local_fallback` lets an Ollama entry run on whatever the local server has. At build time `preset.Build` lists the pulled models (`preset.Options.ListLocal`, by default `preset.ListOllama`, which reads `/api/tags`). It keeps the entry's model when it is pulled, otherwise serves the first pulled model the catalog says calls tools, otherwise the first pulled model that is not an embedding model, and records a `local_model_substituted` warning. The entry keeps its ID, and resolves against the row of the model that serves. With no chat model pulled, the entry fails with `preset.ErrNoLocalModel`, whose message says to run `ollama pull <model>`, or is dropped when it is optional. A server that cannot be listed leaves the entry as written. The shipped `default` and `ollama` presets set it.

## Precedence

Options resolve field by field, lowest first:

1. Provider default: nothing is sent and the adapter's own default applies (for example Anthropic's `max_tokens` of 4096). Reported with origin `provider`.
2. The model row's `defaults`.
3. The preset's `options`, unless the entry says `"inherit": "none"`.
4. The entry's `options`.
5. The entry's `unset` removes inherited names. Unsetting a name the same entry sets is an error.
6. A per-request override at call time, merged by the adapter. It is not part of the configuration hash.

There is one reasoning control: a higher layer that sets any `reasoning` key replaces the lower layer's reasoning whole.

Dials resolve the same way in their own layers: the top-level `dials`, the row's `dials.defaults`, the preset's `dials` (skipped with `"inherit": "none"`), the entry's `dials`, then the entry's `unset`. The adapter compiles them on each request against its own model, under any dials the agent or the request adds ([precedence](dials.md#setting-dials)). An option that sets the same parameter as a dial wins over it. When the merged dials turn `cache` on and no `prompt_cache` option is set, the row's prompt cache is selected, with origin of the dial's layer.

## Validation

Loading is strict. Unknown keys (with a suggestion), duplicate keys, wrong kinds, bad durations, unknown capability, media and server tool names, template cycles, secrets and trailing data are reported with a path such as `presets.balanced.chain[2].options.reasoning.budget`. Errors are a `*catalog.ValidationError` and match `catalog.ErrInvalidCatalog`.

Every chain entry is then checked against its own model:

- the model's declared support, ranges and reasoning rules (`ModelCapabilities.ValidateOptions`);
- what the adapter can send (`catalog.Expressible`, the same table `provider.Build` uses);
- the prompt cache mode and server tools against the model and adapter;
- the preset's `tool_choice` and `output_mode` (`native` needs native structured output);
- `require_declared`, which requires an exact row.

An option an entry cannot honor is rejected, never stripped. A preset whose `temperature` reaches a `gpt-6.1-sol` entry fails with:

```
presets.p.chain[1].options.temperature: not declared supported for openai/gpt-6.1-sol (inherited from preset options)
```

On `gpt-6-luna`, which accepts sampling only with reasoning effort `none` and defaults to `medium`, the same option fails with `requires reasoning to be disabled` unless the entry also sets `"reasoning": {"effort": "none"}`.

The fix is written in the file: `"unset": ["temperature"]`, an entry override, or `"inherit": "none"`. A preset with more than one entry whose shared options set `temperature`, `top_p`, `top_k` or `reasoning` also gets a `prefer_dial` warning: the creativity and reasoning dials adapt to each entry's model instead.

Dials are checked as well:

- every options object a row declares for a dial must pass the row's own validation and be expressible by its adapter, so a bad mapping fails at load time (`invalid_dial` at, for example, `models[3].dials.reasoning.depth.high`);
- each entry's dials are compiled for its own model as a request without tools would be. A contractual dial the model cannot honor is an error that names the layer it came from; an advisory dial that is mapped or dropped is a `dial_mapped` or `dial_dropped` warning, and a dial a raw option overrides is a `dial_overridden` warning.

The configuration hash covers an entry's dials and what they compile to, so changing a row's mapping changes the hash. An entry without dials keeps the hash it had before dials existed.

Warnings do not fail validation: an inferred model, a fallback with a smaller context window than the primary, a superseded model, an unpriced entry under a budget, and a primary whose signed reasoning blocks failover during a tool loop.

## Building a preset

```go
cat, err := catalog.LoadSource(ctx, catalog.Layered(catalog.EmbeddedSource(), catalog.FileSource("catalog.json")))
if err != nil {
	return err
}
bundle, err := preset.Build(ctx, cat, "balanced", []string{"deterministic-extract"}, preset.Options{})
if err != nil {
	return err
}
defer bundle.Close()

a := agent.NewAgent(agent.AgentConfig{SystemPrompt: "..."}, agent.WithPreset(bundle))
```

`preset.Build` builds every entry with `provider.Build` and its own resolved options, wraps it in its own retry decorator and optional per-attempt deadline, and puts all entries behind one router. Each preset is a router group in chain order, and the primary is the default group, so failover follows the chain exactly. `preset.BuildFrom(ctx, src, ...)` takes a `Source`. Neither installs anything: hosts touch global state only through `catalog.Install` or `catalog.Use`.

`agent.WithPreset` sets the provider and the preset's tool choice, output mode, LLM timeout and compaction unless they were already set. Options applied after it win.

`ConfigContent.Model` (or an outcome policy's switch) can name another built preset or a single profile ID. The router selects that complete configuration; no options are copied onto another model. A signed-reasoning lock still keeps the previous profile. A name that is neither fails with `router.ErrUnknownProfile`. `fallback.Provider` and `split.Split` copy one model string across members, so do not wrap a bundle in them; use `bundle.Session()` as a split arm instead.

`bundle.ConfigKey(name)` hashes a group's configuration hashes with the catalog revision, for `cache.Config.ConfigKey`.

## Recording the serving configuration

Every attempt's `types.RouteDelta` carries `Preset`, `ConfigHash`, `CatalogRevision`, the effective `Options` (the entry's options merged with the request override, every dial compiled) and, when the attempt had dials, a `Dials` report of each decision ([dials](dials.md#seeing-what-was-sent)). The agent attaches the last route of the committed call to the assistant turn as `types.RouteContent`, which is persisted with the tree and stripped before provider calls. Traces add `saige.route.preset`, `saige.route.config_hash` and `saige.catalog.revision`, a `saige.route.attempt` event per attempt, and set `gen_ai.request.*` from the serving attempt. Evals record served configurations with `AgentRun.AddProvenance`, and `eval.WithProvenance` makes a comparison warn when the same profile ran with a different hash.

## CLI

Layers merge in this order, lowest first:

1. the embedded default;
2. `$XDG_CONFIG_HOME/saige/catalog.json` (default `~/.config/saige/catalog.json`);
3. `.saige/catalog.json` in the project, found by walking up to the repository root;
4. `SAIGE_CATALOG`, one reference or a list separated by the OS path list separator;
5. each `--catalog` flag.

A reference is a path, a `file://` URL or an `https://` URL. Missing user and project files are skipped. A file named by more than one layer, compared by its cleaned absolute path, is loaded once, at its highest place in this order.

A project file can arrive with a cloned repository, so it is checked against an allowlist. It may set model rows, templates, baselines, presets, options and routing policy, but not:

- `base_url` or `api_key_env` on a chain entry, which could send your key to another host;
- `mcp_server` on a server tool, wherever options appear (preset, entry, model defaults, templates, baselines), which makes the provider connect to that server;
- `routing.failover_on_content_filter` or `routing.failover_on_auth`, which send a refused or failed request to another vendor;
- `inherit_default: false`, which discards the trusted layers below;
- `vertex.project` or `vertex.location` on a chain entry, which bill and send prompts to a Google Cloud project you did not choose, with your own credentials. An empty `vertex` block, which uses your environment, is allowed;
- `dials`, at the top level, on a row or template, on a preset or on an entry, which can turn on prompt caching or raise reasoning spend.

A field the allowlist does not name is refused too. Such a file fails validation unless `SAIGE_TRUST_PROJECT_CATALOG=1` is set or the same file is named with `--catalog`, which loads it once, as a trusted explicit layer. Explicit references, including remote URLs, are trusted.

```
saige --preset balanced chat
saige catalog show balanced           # effective options per entry, with origins and hashes
saige catalog show openai/gpt-6-luna  # a one-entry chain from the model's defaults
saige catalog explain default --dials '{"creativity":"focused"}'  # per entry: dial decisions and raw options sent
saige catalog validate --strict       # exit 1 on errors, or on warnings with --strict
saige catalog validate --dry-build    # also build every adapter with a placeholder key
saige catalog layers                  # paths, trust state and revisions
saige catalog schema                  # the JSON Schema
saige catalog export                  # the merged catalog as canonical JSON
saige catalog reconcile               # compare provider model lists with the catalog
```

The CLI picks `--preset` first, then `--model` (a one-entry chain from the model row's defaults), then `--provider` alone (the preset of that name), then `default_preset`. Without `--preset` or `--model`, the CLI runs one vendor: the first entry of `default_preset` that has credentials, or for the local Ollama entry, a server that answers. The shipped order is Anthropic, OpenAI, Google, then Ollama, and each vendor's entry is its cheapest current model. With no key set and Ollama not running, the CLI says which variables to set or to start Ollama; with Ollama running but no chat model pulled, it says to run `ollama pull qwen3.5:4b`. Cross-vendor failover is opt-in: `--preset default` runs the whole chain.

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
| New models | Listed models that only the provider baseline covers. | yes |
| Limit drift | The endpoint reports a context window or output cap that disagrees with the row (Anthropic and Google report limits; OpenAI does not). | yes |
| Disappeared upstream | Rows no listed model matches and that set no `superseded_by`: candidates for `superseded_by` or removal. A model the key cannot access is missing too, so check before acting. Not reported for Ollama, which lists only pulled weights. | yes |
| Pending review | Rows that still carry the note `needs review`. | yes |
| Stale pricing | Prices and server tool fees whose `as_of` is older than `--stale-days` (90), or is not a date. | no, a reminder |

Dated or tagged IDs that a family row covers (`gpt-6-luna-2026-09-01`) are inferred, not new. `--ignore` and `--ignore-file` take globs over model IDs and row prefixes, bare or as `provider/glob`, for models the catalog deliberately does not describe, such as speech, image and moderation endpoints.

Exit status: `0` no drift, `1` drift, `2` error (a listing failed, no provider was configured, or a flag or file was invalid). The human report is Markdown, so it reads in a terminal and serves as a pull request body.

`--write <path>` appends a stub row for each new model to a catalog file, or creates the file as an overlay layer when it does not exist. A stub has:

- the provider baseline's capability fields (its `extends`, capability lists, limits and media), so it declares nothing the baseline does not already assume;
- no tier, successor, defaults or price;
- the note `needs review`.

One stub covers the dated IDs that start with its prefix. Rows already in the file are skipped, the stubs are inserted before the closing bracket of the `models` array, and no existing byte of the file changes. Reconcile never edits an existing row's prices or capabilities. Once a stub is in the catalog, the exact model ID resolves with `Known` true, so a stub must be completed before it is merged; until its note is removed, every later run reports it as pending review.

### The scheduled workflow

`.github/workflows/catalog-check.yml` runs on the first of each month and on demand (`workflow_dispatch`):

1. It checks each provider whose secret is set (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY`) and posts a notice for each one it skips.
2. It runs reconcile with `.github/catalog-ignore.txt` and `--write agent/provider/catalog/data/default.json`, and adds the report to the job summary.
3. On drift with new stub rows, it commits them to the `catalog/reconcile` branch and opens a pull request whose body is a review checklist followed by the report. When that pull request is already open, it only refreshes the body, so a reviewer's edits on the branch are never overwritten.
4. On drift with no stub rows to propose (only disappeared rows or limit drift), the job fails with the report in its summary, so the finding is not lost.

The pull request is opened with the release app's token when `SR_RELEASER_PRIVATE_KEY` is set, so CI runs on it; otherwise with `GITHUB_TOKEN`, and CI must be started by hand (close and reopen the pull request, or push to the branch).

### Reviewing a catalog pull request

For each stub row:

1. Look the model up on the vendor's model page. Set `tier`, `limits` and `extends` (a template such as `openai.reasoning` or `anthropic.adaptive` usually fits better than the baseline), then adjust capabilities with `add_capabilities` and `remove_capabilities`. Declare only what the vendor documents.
2. Price it from the vendor's price list with `as_of` and `source`, or leave it unpriced; a budget refuses an unpriced model, which is the safe failure.
3. Remove the `needs review` note. Keep a note for anything a caller should know.

For each disappeared row, set `superseded_by` to the current family, or add the prefix to `.github/catalog-ignore.txt` when the key simply cannot see it. For limit drift, confirm the new limit on the vendor's page before changing the row. Refresh stale prices the same way, updating `as_of`.

Then bump `revision`, regenerate the lookup golden file (`go test ./agent/provider/catalog -run TestDefaultLookupGolden -update`) and review its diff, run `saige catalog validate --dry-build`, and merge. CI fails while any row in the embedded catalog still carries `needs review`.
