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
saige chat --provider ollama   # fully local, on a model pulled into Ollama
saige chat --verbose  # plain-text mode for pipes/CI
saige chat --tools harness --workspace ./repo  # add write_file, edit_file, execute_code, fetch_url

# Single-shot question (pipe-friendly)
saige ask "What is retrieval-augmented generation?"
echo "Explain transformers" | saige ask --template minimal

# Run an agent definition (see docs/agent-definitions.md)
saige ask --agents-dir examples/agents --agent assistant "What does go.mod declare?"
saige chat --agent repo-steward@^1 --workspace .

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

### Built-in tools

`--tools` picks the [harness toolset](../../docs/harness-tools.md), confined to `--workspace` (default `.`):

| Value | Tools |
|---|---|
| `none` | None. The default for `ask`. |
| `readonly` | `read_file`, `list_dir`, `glob`, `grep`, scratch tools. The default for `chat`. |
| `harness` | Adds `write_file`, `edit_file`, `execute_code`, and `fetch_url` |
| a group list | Any of `read`, `write`, `exec`, `web`, such as `read,exec` |

`execute_code` runs in a `--sandbox subprocess` (default) or `docker` sandbox with `--exec-network deny` (default) or `allow`.

### Tool approval

Tools that change data (`write_file`, `edit_file`, `execute_code`, `rag_update`, `rag_delete`, `kg_ingest`) require approval before they run.

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
| POST | `/v1/sessions/{sid}/turns/{tid}/interrupts/{tool_call_id}` | `{approved, message, modified_args, grant}` answers a `marker` event. An optional `grant` (`scope`: `once`, `tool`, `args` or `session`; `match`; `expires_at`) approves later calls it covers for the rest of the session. An invalid, expired, or refused grant is a `400`. A sub-agent's call id contains `/` (`<delegation id>/<child id>`); send it as-is or percent-encoded. `404` when nothing is pending for that call. |
| POST | `/v1/sessions/{sid}/turns/{tid}/cancel` | Cancel the turn |
| GET | `/v1/sessions/{sid}/turns/{tid}` | `{done, last_seq, error}` |
| GET | `/v1/sessions/{sid}/tree` | The conversation tree as JSON |

Each turn keeps its most recent 10000 events for replay. When a client asks for events older than that, the request fails with `410 Gone` and `oldest_seq`, and a stream that falls that far behind ends with a `gap` event carrying `oldest_seq`. Refetch the turn and the tree, then resume with `Last-Event-ID: <oldest_seq - 1>`.

A session with no running turn is dropped after `--idle-ttl` (default 1h). A running turn keeps its session alive.

Safety rules:

- **Localhost by default.** Binding to a non-loopback address requires `--token` or `SAIGE_SERVE_TOKEN`, sent as `Authorization: Bearer <token>`. Without a token, requests whose `Host` is not a loopback name are refused.
- **JSON-only POSTs.** Every POST must send `Content-Type: application/json`, which a cross-site form cannot do without a CORS preflight the server never grants.
- **Grants are explicit.** Only a client's approval creates a grant, it never covers a destructive tool, and `--deny-after N` stops asking about a tool after N denials in a session. See [approval policy and grants](../../docs/approval-policy.md).
- **No silent approvals.** `approved` must be present. A pending approval that gets no decision within `--approval-timeout` (default 10m) is denied, including when the client disconnected.

### Tool packs

`--tools` adds opt-in packs to `serve`:

| Pack | Tools | Notes |
|---|---|---|
| `fs` | `read`, `list`, `glob`, `grep` | Needs `--workspace` |
| `fs-write` | `fs` plus `write`, `edit` | Both require approval |
| `fetch` | `fetch` | Private and metadata addresses refused |
| `bash` | `bash` | Requires approval. `--bash-network deny` (default) needs `sandbox-exec` or `unshare`; `--bash-network allow` runs without network isolation |

## Standalone RAG Operations

Results are printed as indented JSON. Pass `--format json` for the plain JSON renderer (no header or styling), suitable for scripting.

Ingest and search use the same embedding provider (`--embed-provider`, defaulting to the LLM provider) and retrieve by vector similarity, so a search finds documents ingested by an earlier run. Use the same embedding settings for both.

```bash
saige rag ingest --db "$SAIGE_RAG_DB" --file paper.pdf --mime application/pdf
saige rag sync --db "$SAIGE_RAG_DB" --dir docs --include '**/*.md' --exclude 'drafts/**' --prune
saige rag search --db "$SAIGE_RAG_DB" --query "attention mechanism"
saige rag lookup --db "$SAIGE_RAG_DB" --uuid <variant-uuid>
saige rag delete --db "$SAIGE_RAG_DB" --uuid <doc-uuid>
```

`rag sync` walks `--dir` and ingests new files, replaces changed ones in place, and with `--prune` deletes documents of files that no longer exist. It skips `.git`, `.hg`, `.svn`, `node_modules`, dot paths, secret file names (`.env*`, `*.pem`, `*.key`, `id_rsa*` and similar), and paths in `.gitignore` or `.saigeignore`. `--include-hidden`, `--include-tool-dirs`, `--allow-secret-names` and `--no-ignore-files` turn those off; `--include` and `--exclude` add doublestar globs relative to `--dir`. The output lists skipped paths with a count per reason, and skipped paths are never pruned.

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
saige eval compare <run-id> --store eval-results            # regression gate against the previous succeeded run
saige eval compare --suite pr-42 --baseline-suite main --store eval-results --format markdown --output compare.md
saige eval runs --store postgres://user:pass@host/db --tenant acme   # a PostgreSQL results store
saige eval online --store postgres://user:pass@host/db --since 24h --rate 0.1 --scorer tool_success_rate
saige eval online --store postgres://user:pass@host/db --watch --scorer tool_success_rate
saige eval scorers
```

`runs`, `show` and `scorers` accept `--format json`. `compare` accepts `--format human|markdown|junit|json`.

The API key is chosen by the host the request goes to, never by the first variable that happens to be set: `--api-key`, then the variable the manifest names with `api_key_env` (only the host's own variable or a `SAIGE_EVAL_*` variable), then `SAIGE_EVAL_API_KEY`, then the host's own key (`OPENAI_API_KEY` for api.openai.com, `GEMINI_API_KEY` or `GOOGLE_API_KEY` for Gemini, `GROQ_API_KEY`, `OPENROUTER_API_KEY`, `MISTRAL_API_KEY`, `GITHUB_TOKEN` for GitHub Models). The base URL comes from `--api-base`, the manifest, `SAIGE_EVAL_API_BASE`, or `OPENAI_BASE_URL`. A base URL from the manifest receives only `--api-key` or the host's own variable; confirm it with `--api-base` to send it `SAIGE_EVAL_API_KEY` or a `SAIGE_EVAL_*` variable.

Each script gets an `outputs/` directory and a `metrics.json`. `--continue-on-error` (default true) keeps running after a failed experiment, writes `<experiment>/error.json`, and exits non-zero with all failures joined. A script or edit turn whose request fails on infrastructure (a rate limit, an outage, a timeout, bad credentials, an unreachable endpoint, or cancellation) is inconclusive rather than failed, since the model never answered.

| Exit status | `saige eval run` | `saige eval compare` |
| --- | --- | --- |
| 0 | every script ran and every assertion held, with at most `--max-inconclusive` of the scripts inconclusive | no metric regressed |
| 1 | a script failed for another reason, or a measured result violated an assertion | a metric regressed, or the candidate failed its `--assert` gate |
| 2 | the manifest, corpus or flags are invalid (`saige eval validate` uses the same code) | invalid flags, or the baseline is the candidate |
| 3 | nothing failed for real, but more than `--max-inconclusive` (default 0) of the scripts were inconclusive or the gate was; rerun it, for example with `--resume` | inconclusive: a metric had fewer than `--min-cases` paired cases, only one run measured it, or the candidate's gate was inconclusive |

Errors are printed once, as `error: <msg>`. `saige eval compare` pairs the cases both runs measured, checks each metric's pass rate (gated metrics) or mean, and fails when a drop beyond `--max-regression` (or a per-metric `--threshold`) is significant at `--significance`. Its flags, report formats and a GitHub Actions recipe are in [Regression Gate in CI](../../eval/README.md#regression-gate-in-ci).

## Presets and the Catalog

Model capabilities and named presets come from the model catalog: the embedded default, then `~/.config/saige/catalog.json`, then `.saige/catalog.json` in the project, then `SAIGE_CATALOG`, then each `--catalog` (a path, `file://` or `https://` URL).

| Flag | Env | Effect |
| --- | --- | --- |
| `--preset` | `SAIGE_PRESET` | Run a catalog preset: an ordered chain whose entries each carry their own options. |
| `--model` | | A one-entry chain from the catalog's model defaults; the provider comes from `--provider` or is inferred from the model. |
| `--provider` | `SAIGE_PROVIDER` | The adapter to use: `anthropic`, `openai`, `google`, `vertex` (Google models through Vertex AI) or `ollama` (open-weight models on a local Ollama runtime). Without `--model`, run the preset of that name. |
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

## Agent Definitions

An agent definition is a Markdown file with YAML frontmatter: `*.agent.md`, or any `.md` file in a directory named `agents`. Directories are searched lowest precedence first: `~/.config/saige/agents`, the project's `.saige/agents`, then each `--agents-dir`.

```bash
saige agent list                        # every definition, highest version first
saige agent show repo-steward@^1        # source, digests, sub-agents, skill hashes, the file
saige agent validate [PATH...]          # offline checks; exit 2 when anything is invalid
saige agent schema                      # JSON Schema of the frontmatter
saige ask --agent assistant "question"  # also chat, and serve (one pinned agent per session)
```

| Flag | Effect |
| --- | --- |
| `--agent` | On `ask`, `chat` and `serve`: run a definition, `NAME` or `NAME@RANGE`. `--preset`, `--model` and `--provider` override its model; `--tools` and `--system` cannot be combined with it. |
| `--agents-dir` | Add a directory of definitions (repeatable). |
| `--mcp-config` | MCP configuration file whose servers a definition's `tools.mcp` may name. |
| `--agents-reload` | On `serve --agent`: reload the definitions this often, so new sessions see edits. |

A project's `.saige/agents` is untrusted unless `SAIGE_TRUST_PROJECT_AGENTS=1` or it is named with `--agents-dir`: it may not connect to MCP servers, use memory, loosen approvals, or use the `exec` and `web` harness groups. See [agent definitions](../../docs/agent-definitions.md).

## Provider Auto-Detection

`--provider` names a saige adapter, not always a model vendor: `ollama` is a local runtime that serves open-weight models such as qwen3.5:4b. See [vendors, runtimes and adapters](../../docs/concepts.md#vendors-runtimes-and-adapters).

Without `--preset`, `--model` or `--provider`, the CLI runs one vendor: the first entry of the catalog's `default_preset` that can serve. The shipped order is Anthropic, OpenAI, Google (whichever key is set first), then a local Ollama server: it runs `qwen3.5:4b` when that is pulled, else another pulled chat model. With none available, it tells you to set `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` (or a Google key), to start Ollama, or, when Ollama runs with no chat model, to run `ollama pull qwen3.5:4b`. `--provider ollama` uses the same rule; `--model` names the model exactly. Cross-vendor failover is opt-in: pass `--preset default`. Each vendor's preset runs its cheapest current model (claude-haiku-5-5, gpt-6-luna, gemini-3.1-flash-lite); `--preset anthropic-quality`, `openai-quality` or `google-quality` runs the mid tier. For Vertex AI, set `GOOGLE_GENAI_USE_VERTEXAI=true` and `GOOGLE_CLOUD_PROJECT` (and optionally `GOOGLE_CLOUD_LOCATION`, default `global`), or pass `--provider vertex`; it authenticates with Application Default Credentials. `--base-url` applies to the selected adapter's entries; on a multi-vendor preset add `--provider`.

> **Note:** Anthropic has no embedding API. With `--provider anthropic` plus RAG/KG
> features, also pass `--embed-provider` (openai, google, or ollama) and that
> provider's key.

## Related

- [`saige-mcp`](../saige-mcp/README.md): expose saige tools to Claude Code, Codex, and other MCP clients
- [`agent`](../../agent/README.md): the SDK powering chat and ask
- [Root README](../../README.md): project overview and installation
