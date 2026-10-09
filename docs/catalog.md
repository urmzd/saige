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

A model row matches model IDs by longest prefix. Only an exact match is a declaration (`Known` true); a dated or tagged ID inherits the family row with `Known` false.

```json
{
  "provider": "openai", "prefix": "gpt-4.1", "extends": "openai.chat.structured", "tier": "standard",
  "limits": { "context_window": 1000000, "max_output_tokens": 32768 },
  "pricing": { "currency": "USD", "input_per_mtok": 2, "output_per_mtok": 8, "cached_input_per_mtok": 0.5,
    "as_of": "2026-07-24", "source": "vendor list price" },
  "defaults": { "max_output_tokens": 4096 }
}
```

Row fields: `extends` (a template; chains up to four deep, cycles rejected), `tier`, `superseded_by`, `capabilities` (replaces the inherited list), `add_capabilities`, `remove_capabilities`, `limits`, `reasoning` (`efforts`, `default_effort`, `required`, `default_enabled`, `min_budget`, `max_budget`, `dynamic_budget`, `zero_budget`, `sampling_requires_no_reasoning`), `structured_output` (`""`, `native` or `tool_call`), `media`, `server_tools`, `server_tool_fees`, `pricing` (`as_of` is required when a rate is set), `defaults` and `notes`.

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
    { "id": "primary", "provider": "anthropic", "model": "claude-sonnet-4-6",
      "options": { "reasoning": { "effort": "medium" },
                   "prompt_cache": { "mode": "markers", "ttl": "5m", "system": true, "tools": true } } },
    { "id": "openai", "provider": "openai", "model": "gpt-4.1",
      "options": { "temperature": 0.3, "seed": 7 } },
    { "id": "local", "provider": "ollama", "model": "qwen3", "optional": true,
      "options": { "temperature": 0.3, "reasoning": { "enabled": false } }, "retry": { "disable": true } }
  ]
}
```

Preset keys: `description`, `extends`, `options`, `tool_choice` (`auto`, `none`, `required` or `named:<tool>`), `output_mode` (`auto`, `native`, `tool` or `prompt`), `llm_timeout`, `retry`, `routing`, `require_declared` and `chain`.

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

Chain entry keys: `id` (default `provider/model`), `provider`, `model`, `options`, `unset`, `inherit` (`all` or `none`), `retry`, `attempt_timeout`, `base_url`, `api_key_env` (the name of the variable; a key itself is rejected) and `optional` (drop the whole entry when it cannot serve). An optional entry is dropped when its credentials are missing, and an optional entry that needs none (a local Ollama model) is dropped when its server does not answer a reachability check at build time (`preset.Options.Probe`, by default `preset.ProbeOllama`).

## Precedence

Options resolve field by field, lowest first:

1. Provider default: nothing is sent and the adapter's own default applies (for example Anthropic's `max_tokens` of 4096). Reported with origin `provider`.
2. The model row's `defaults`.
3. The preset's `options`, unless the entry says `"inherit": "none"`.
4. The entry's `options`.
5. The entry's `unset` removes inherited names. Unsetting a name the same entry sets is an error.
6. A per-request override at call time, merged by the adapter. It is not part of the configuration hash.

There is one reasoning control: a higher layer that sets any `reasoning` key replaces the lower layer's reasoning whole.

## Validation

Loading is strict. Unknown keys (with a suggestion), duplicate keys, wrong kinds, bad durations, unknown capability, media and server tool names, template cycles, secrets and trailing data are reported with a path such as `presets.balanced.chain[2].options.reasoning.budget`. Errors are a `*catalog.ValidationError` and match `catalog.ErrInvalidCatalog`.

Every chain entry is then checked against its own model:

- the model's declared support, ranges and reasoning rules (`ModelCapabilities.ValidateOptions`);
- what the adapter can send (`catalog.Expressible`, the same table `provider.Build` uses);
- the prompt cache mode and server tools against the model and adapter;
- the preset's `tool_choice` and `output_mode` (`native` needs native structured output);
- `require_declared`, which requires an exact row.

An option an entry cannot honor is rejected, never stripped. A preset whose `temperature` reaches an `o3` entry fails with:

```
presets.p.chain[1].options.temperature: not declared supported for openai/o3 (inherited from preset options)
```

The fix is written in the file: `"unset": ["temperature"]`, an entry override, or `"inherit": "none"`.

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

`agent.WithPreset` sets the provider and the preset's tool choice, output mode and LLM timeout unless they were already set. Options applied after it win.

`ConfigContent.Model` (or an outcome policy's switch) can name another built preset or a single profile ID. The router selects that complete configuration; no options are copied onto another model. A signed-reasoning lock still keeps the previous profile. A name that is neither fails with `router.ErrUnknownProfile`. `fallback.Provider` and `split.Split` copy one model string across members, so do not wrap a bundle in them; use `bundle.Session()` as a split arm instead.

`bundle.ConfigKey(name)` hashes a group's configuration hashes with the catalog revision, for `cache.Config.ConfigKey`.

## Recording the serving configuration

Every attempt's `types.RouteDelta` carries `Preset`, `ConfigHash`, `CatalogRevision` and the effective `Options` (the entry's options merged with the request override). The agent attaches the last route of the committed call to the assistant turn as `types.RouteContent`, which is persisted with the tree and stripped before provider calls. Traces add `saige.route.preset`, `saige.route.config_hash` and `saige.catalog.revision`, a `saige.route.attempt` event per attempt, and set `gen_ai.request.*` from the serving attempt. Evals record served configurations with `AgentRun.AddProvenance`, and `eval.WithProvenance` makes a comparison warn when the same profile ran with a different hash.

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
- `inherit_default: false`, which discards the trusted layers below.

A field the allowlist does not name is refused too. Such a file fails validation unless `SAIGE_TRUST_PROJECT_CATALOG=1` is set or the same file is named with `--catalog`, which loads it once, as a trusted explicit layer. Explicit references, including remote URLs, are trusted.

```
saige --preset balanced chat
saige catalog show balanced           # effective options per entry, with origins and hashes
saige catalog show openai/gpt-4.1     # a one-entry chain from the model's defaults
saige catalog validate --strict       # exit 1 on errors, or on warnings with --strict
saige catalog validate --dry-build    # also build every adapter with a placeholder key
saige catalog layers                  # paths, trust state and revisions
saige catalog schema                  # the JSON Schema
saige catalog export                  # the merged catalog as canonical JSON
```

The CLI picks `--preset` first, then `--model` (a one-entry chain from the model row's defaults), then `--provider` alone (the preset of that name), then `default_preset`. Without `--preset` or `--model`, the CLI runs one vendor: the first entry of `default_preset` that has credentials, or for the local Ollama entry, a server that answers. The shipped order is Anthropic, OpenAI, Google, then Ollama. With no key set and Ollama not running, the CLI says which variables to set or to start Ollama. Cross-vendor failover is opt-in: `--preset default` runs the whole chain.

`--base-url` applies to the entries of the selected provider: `--provider` when given, otherwise the one provider the chain uses. On a chain that spans several vendors without `--provider` it is an error, never ignored.
