# saige CLI

Interactive chat, single-shot queries, standalone RAG/KG operations, and live evals from the terminal.

```bash
go install github.com/urmzd/saige/cmd/saige@latest
```

Or use the [install script](../../README.md#installation) for a pre-built binary. Update in place with `saige update`, or check without installing via `saige update --check`. Updates are verified against the release's `SHA256SUMS`.

## Chat and Ask

```bash
# Interactive multi-turn chat (Bubble Tea TUI)
saige chat
saige chat --provider anthropic --model claude-haiku-5-5
saige chat --verbose  # plain-text mode for pipes/CI

# Single-shot question (pipe-friendly)
saige ask "What is retrieval-augmented generation?"
echo "Explain transformers" | saige ask --template minimal

# With RAG/KG tools attached to the agent
saige chat --rag-db "postgres://localhost/mydb" --kg-db "postgres://localhost/mydb"
saige ask --rag-db "$SAIGE_RAG_DB" "What does the paper say about attention?"
```

### Chat keys

The input stays focused while a reply streams, so you can type the next message at any time.

| Key | While a reply streams | Otherwise |
|---|---|---|
| `Enter` | Queue the message; it runs when the current reply would finish | Send |
| `Ctrl-J` or `Alt-Enter` | Steer: add the message at the next safe point without stopping the run (most terminals send `Ctrl-J` for `Ctrl-Enter`) | Send |
| `Esc` | Stop the run and stay in the session | |
| `Ctrl-C` | Stop the run; press again to quit | Press twice to quit |
| `PgUp` `PgDn` `Up` `Down` | Scroll the transcript | Scroll the transcript |

Queued messages show in a strip above the input until the run picks them up. A stopped run is tagged `stopped`; `/continue` resumes the last turn, and messages still queued at the stop go back to the input. An error ends the turn, not the session. `/quit` exits.

`--template detailed` adds reasoning, tool arguments and results, handoffs, routes, and citations to the transcript.

### JSON output

With `--format json`, `ask` and `chat` write one JSON object per line: a versioned envelope per stream event (`{"v":1,"kind":"text.delta","data":{...}}`), the same format other stream consumers decode. `chat --format json` reads prompts line by line from stdin and writes its prompts to stderr, so stdout stays machine-readable.

```bash
saige ask --format json "Summarize RFC 9110" | jq -j 'select(.kind=="text.delta") | .data.content'
```

### Tool approval

Tools that change data (`rag_update`, `rag_delete`, `kg_ingest`) require approval before they run.

| Command | How approval works |
|---|---|
| `saige chat` | The TUI shows the tool's arguments and asks `Approve? (y/n)`. Only `y`/`yes` approve and `n`/`no` deny; anything else, including an empty answer, asks again. |
| `saige chat --verbose` | Same prompt, inline in the plain-text stream. End of input denies. |
| `saige ask` | Cannot prompt. `--approve=deny` (default) rejects the call and tells the model why; `--approve=allow` runs it. |

## Serve

`saige serve` runs the agent behind a small HTTP API. Each session owns a conversation tree; each turn streams Server-Sent Events whose `id` is the envelope `seq`, whose `event` is its `kind`, and whose `data` is the same versioned envelope `--format json` prints. Sub-agent events are flattened: the envelope `path` lists the tool call IDs that lead to the run that emitted them.

```bash
saige serve --tools fs,fetch --workspace .            # listens on 127.0.0.1:8787

sid=$(curl -s -XPOST -H 'Content-Type: application/json' -d '{}' localhost:8787/v1/sessions | jq -r .session_id)
tid=$(curl -s -XPOST -H 'Content-Type: application/json' -d '{"message":"List the Go files"}' \
  localhost:8787/v1/sessions/$sid/turns | jq -r .turn_id)
curl -N localhost:8787/v1/sessions/$sid/turns/$tid/events
```

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/sessions` | Create a session: `{session_id}`. `429` at the session limit. |
| DELETE | `/v1/sessions/{sid}` | Cancel the running turn and drop the session |
| POST | `/v1/sessions/{sid}/turns` | `{message}` starts a turn: `202 {turn_id}`. `409` while another turn in the session runs. |
| GET | `/v1/sessions/{sid}/turns/{tid}/events` | SSE stream. `Last-Event-ID` (or `?after=`) resumes after that seq. Ends after the turn's last event. `410 {oldest_seq}` when the next event is no longer kept (see below). |
| POST | `/v1/sessions/{sid}/turns/{tid}/interrupts/{tool_call_id}` | `{approved, message, modified_args}` answers a `marker` event. A sub-agent's call id contains `/` (`<delegation id>/<child id>`); send it as-is or percent-encoded. `404` when nothing is pending for that call. |
| POST | `/v1/sessions/{sid}/turns/{tid}/cancel` | Cancel the turn |
| GET | `/v1/sessions/{sid}/turns/{tid}` | `{done, last_seq, error}` |
| GET | `/v1/sessions/{sid}/tree` | The conversation tree as JSON |

Each turn keeps its most recent 10000 events for replay. When a client asks for events older than that, the request fails with `410 Gone` and `oldest_seq`, and a stream that falls that far behind ends with a `gap` event carrying `oldest_seq`. Refetch the turn and the tree, then resume with `Last-Event-ID: <oldest_seq - 1>`.

A session with no running turn is dropped after `--idle-ttl` (default 1h). A running turn keeps its session alive.

Safety rules:

- **Localhost by default.** Binding to a non-loopback address requires `--token` or `SAIGE_SERVE_TOKEN`, sent as `Authorization: Bearer <token>`. Without a token, requests whose `Host` is not a loopback name are refused.
- **JSON-only POSTs.** Every POST must send `Content-Type: application/json`, which a cross-site form cannot do without a CORS preflight the server never grants.
- **No silent approvals.** `approved` must be present. A pending approval that gets no decision within `--approval-timeout` (default 10m) is denied, including when the client disconnected.

### Tool packs

`--tools` adds opt-in packs to `serve`:

| Pack | Tools | Notes |
|---|---|---|
| `fs` | `read`, `glob`, `grep` | Needs `--workspace` |
| `fs-write` | `fs` plus `write`, `edit` | Both require approval |
| `fetch` | `fetch` | Private and metadata addresses refused |
| `bash` | `bash` | Requires approval. `--bash-network deny` (default) needs `sandbox-exec` or `unshare`; `--bash-network allow` runs without network isolation |

## Standalone RAG Operations

Results are printed as indented JSON. Pass `--format json` for the plain JSON renderer (no header or styling), suitable for scripting.

Ingest and search use the same embedding provider (`--embed-provider`, defaulting to the LLM provider) and retrieve by vector similarity, so a search finds documents ingested by an earlier run. Use the same embedding settings for both.

```bash
saige rag ingest --db "$SAIGE_RAG_DB" --file paper.pdf --mime application/pdf
saige rag search --db "$SAIGE_RAG_DB" --query "attention mechanism"
saige rag lookup --db "$SAIGE_RAG_DB" --uuid <variant-uuid>
saige rag delete --db "$SAIGE_RAG_DB" --uuid <doc-uuid>
```

## Standalone KG Operations

`kg ingest` extracts entities and relations with the LLM selected by `--provider`/`--model` and embeds them with the embedding provider. Read commands (`search`, `graph`, `node`) need only the embedding provider.

```bash
saige kg ingest --db "$SAIGE_KG_DB" --name "meeting" --text "Alice presented the roadmap."
saige kg search --db "$SAIGE_KG_DB" --query "Who presented?"
saige kg graph  --db "$SAIGE_KG_DB" --limit 50
saige kg node   --db "$SAIGE_KG_DB" --id <entity-uuid> --depth 2
```

## Live Evals

Run a multi-turn eval corpus against any OpenAI-compatible API or a saige provider (see [eval/harness](../../eval/harness/README.md)):

```bash
saige eval init evals                          # scaffold a sample corpus and saige.eval.json
saige eval validate evals                      # check the manifest and corpus offline
saige eval run --manifest evals/saige.eval.json --dry-run   # preflight and print the plan
saige eval run --manifest evals/saige.eval.json             # --experiments-dir is optional with a manifest
saige eval run --experiments-dir evals --model gpt-6-luna --flows base --force
saige eval run --experiments-dir evals --store eval-results --suite docs   # also record the run for saige eval runs/show
saige eval run --manifest evals/saige.eval.json --store eval-results --resume <run-id>
saige eval run --manifest evals/saige.eval.json --concurrency 4 --assert 'aggregate:latency_ms<=2000'
saige eval runs --store eval-results [--suite S] [--limit N]
saige eval show <run-id> --store eval-results
saige eval scorers
```

`runs`, `show` and `scorers` accept `--format json`.

The API key is chosen by the host the request goes to, never by the first variable that happens to be set: `--api-key`, then the variable the manifest names with `api_key_env` (only the host's own variable or a `SAIGE_EVAL_*` variable), then `SAIGE_EVAL_API_KEY`, then the host's own key (`OPENAI_API_KEY` for api.openai.com, `GEMINI_API_KEY` or `GOOGLE_API_KEY` for Gemini, `GROQ_API_KEY`, `OPENROUTER_API_KEY`, `MISTRAL_API_KEY`, `GITHUB_TOKEN` for GitHub Models). The base URL comes from `--api-base`, the manifest, `SAIGE_EVAL_API_BASE`, or `OPENAI_BASE_URL`. A base URL from the manifest receives only `--api-key` or the host's own variable; confirm it with `--api-base` to send it `SAIGE_EVAL_API_KEY` or a `SAIGE_EVAL_*` variable.

Each script gets an `outputs/` directory and a `metrics.json`. `--continue-on-error` (default true) keeps running after a failed experiment, writes `<experiment>/error.json`, and exits non-zero with all failures joined. The command exits with status 1 when a script fails or an assertion is violated, and with status 2 when the manifest or corpus fails validation (`saige eval validate` uses the same code). Errors are printed once, as `error: <msg>`.

## Presets and the Catalog

Model capabilities and named presets come from the model catalog: the embedded default, then `~/.config/saige/catalog.json`, then `.saige/catalog.json` in the project, then `SAIGE_CATALOG`, then each `--catalog` (a path, `file://` or `https://` URL).

| Flag | Env | Effect |
| --- | --- | --- |
| `--preset` | `SAIGE_PRESET` | Run a catalog preset: an ordered chain whose entries each carry their own options. |
| `--model` | | A one-entry chain from the catalog's model defaults; the provider comes from `--provider` or is inferred from the model. |
| `--provider` | `SAIGE_PROVIDER` | Without `--model`, run the preset of that name. `vertex` runs Google models through Vertex AI. |
| `--catalog` | `SAIGE_CATALOG` | Add a catalog layer (repeatable). |

```bash
saige catalog show                    # the preset that would run, with option origins and hashes
saige catalog explain default --dials '{"reasoning":{"depth":"high"}}'  # each entry's dial decisions and raw options
saige catalog validate --strict       # exit 1 on errors, or warnings with --strict
saige catalog layers                  # which files were merged, and whether each is trusted
saige catalog export                  # the merged catalog as canonical JSON
saige catalog schema                  # the JSON Schema of the file format
```

A project catalog is checked against an allowlist: it may not set `base_url`, `api_key_env`, a server tool's `mcp_server`, `routing.failover_on_content_filter`, `routing.failover_on_auth`, `inherit_default` or `dials` unless `SAIGE_TRUST_PROJECT_CATALOG=1` is set or the file is also named with `--catalog` (then it is loaded once, as that flag's layer). See [model catalog and presets](../../docs/catalog.md).

## Provider Auto-Detection

Without `--preset`, `--model` or `--provider`, the CLI runs one vendor: the first entry of the catalog's `default_preset` that can serve. The shipped order is Anthropic, OpenAI, Google (whichever key is set first), then a local Ollama model when the server answers. With none available, it tells you to set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` (or a Google key), or to start Ollama. Cross-vendor failover is opt-in: pass `--preset default`. Each vendor's preset runs its cheapest current model (claude-haiku-5-5, gpt-6-luna, gemini-3.1-flash-lite); `--preset anthropic-quality`, `openai-quality` or `google-quality` runs the mid tier. For Vertex AI, set `GOOGLE_GENAI_USE_VERTEXAI=true` and `GOOGLE_CLOUD_PROJECT` (and optionally `GOOGLE_CLOUD_LOCATION`, default `global`), or pass `--provider vertex`; it authenticates with Application Default Credentials. `--base-url` applies to the selected provider's entries; on a multi-vendor preset add `--provider`.

> **Note:** Anthropic has no embedding API. With `--provider anthropic` plus RAG/KG
> features, also pass `--embed-provider` (openai, google, or ollama) and that
> provider's key.

## Related

- [`saige-mcp`](../saige-mcp/README.md): expose saige tools to Claude Code, Codex, and other MCP clients
- [`agent`](../../agent/README.md): the SDK powering chat and ask
- [Root README](../../README.md): project overview and installation
