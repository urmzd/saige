# saige-mcp

MCP server binary that exposes saige tool packs, and optionally a saige agent, over the [Model Context Protocol](https://modelcontextprotocol.io/). It serves stdio by default, or streamable HTTP with bearer authentication. Any MCP-compatible client can use saige tools: Claude Code, Codex, Gemini CLI, and others.

```bash
go install github.com/urmzd/saige/cmd/saige-mcp@latest
```

Or install a pre-built, checksum-verified binary (Linux and macOS, amd64 and arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/urmzd/saige/main/install.sh | BIN=saige-mcp bash
```

## Usage

```bash
# Expose research tools (web search + file ops + KG)
saige-mcp --tools research --searxng-url http://localhost:8080 --db "$SAIGE_DB"

# Expose only KG tools
saige-mcp --tools kg --db "$SAIGE_DB"

# Expose the eval scorers, and health checks of the servers in an MCP config file
saige-mcp --tools eval,mcp --mcp-config ~/.config/saige/mcp.json

# Expose everything
saige-mcp --tools all --db "$SAIGE_DB" --searxng-url http://localhost:8080

# Expose a saige agent as one tool, and nothing else
saige-mcp --tools none --agent anthropic/claude-haiku-5-5 --agent-system "You are a careful researcher."
```

### Streamable HTTP

```bash
export SAIGE_MCP_TOKEN="$(openssl rand -hex 32)"

# Loopback only, plain HTTP
saige-mcp --transport http --tools research,eval --addr 127.0.0.1:8765

# Any interface, with TLS and tokens from a file
saige-mcp --transport http --addr :8443 --tls-cert cert.pem --tls-key key.pem --token-file /etc/saige-mcp/tokens

# Behind a TLS-terminating proxy
saige-mcp --transport http --addr :8765 --allow-insecure-bind
```

The endpoint is `http://<addr>/mcp`; `--path` changes the path. Every request needs `Authorization: Bearer <token>`, and anything else gets 401.

- **Tokens**: `$SAIGE_MCP_TOKEN` holds one or more tokens, comma-separated; `--token-env` names another variable. `--token-file` adds one token per line, skipping blank lines and `#` comments. Each token must be at least 16 characters, and the server does not start without one.
- **Rate limit**: each token has its own bucket of `--rate-burst` requests (default 20), refilled at `--rate-limit` requests per second (default 5). A request over the limit gets 429 with `Retry-After`.
- **Bind**: plain HTTP is refused on any address other than loopback. Serve TLS with `--tls-cert` and `--tls-key`, or pass `--allow-insecure-bind` when a TLS-terminating proxy is in front.
- **Sessions**: a session is bound to the token that opened it and closes after 30 minutes idle. A request body over 4 MiB is rejected, and the SDK's DNS rebinding and cross-origin protections stay on.

Approval works over HTTP as over stdio: the elicitation request reaches the client within the session.

## Flags

| Flag | Env | Description |
|------|-----|-------------|
| `--tools` | | Comma-separated tool packs: `research`, `kg`, `eval`, `mcp`, `all`, `none` (default: `all`) |
| `--db` | `SAIGE_DB` | PostgreSQL DSN for KG tools |
| `--searxng-url` | `SEARXNG_URL` | SearXNG base URL for web search |
| `--root` | | Root directory for file search/read (default: `.`) |
| `--read-only` | | Omit every mutating tool (`store_knowledge`, `kg_ingest`) |
| `--approval` | | How marked tools run: `elicit` (default), `host`, or `deny` |
| `--mcp-config` | | MCP configuration file (`mcpServers`) for the `mcp` pack |
| `--agent` | | Catalog preset or `provider/model` to expose as the agent tool |
| `--agent-catalog` | | Catalog file for `--agent` (default: the embedded catalog) |
| `--agent-tool` | | Name of the agent tool (default: `saige_agent`) |
| `--agent-description` | | Description of the agent tool |
| `--agent-system` | | System prompt of the agent |
| `--agent-schema` | | JSON object schema file; the agent then returns a structured result |
| `--agent-max-iter` | | Most model turns per agent call (default: 10) |
| `--agent-timeout` | | Time limit of one agent call (default: `5m`) |
| `--transport` | | `stdio` (default) or `http` |
| `--addr` | | HTTP listen address (default: `127.0.0.1:8765`) |
| `--path` | | HTTP endpoint path (default: `/mcp`) |
| `--token-env` | | Variable holding the bearer tokens (default: `SAIGE_MCP_TOKEN`) |
| `--token-file` | | File of bearer tokens, one per line |
| `--tls-cert`, `--tls-key` | | Serve HTTPS with this certificate and key |
| `--allow-insecure-bind` | | Allow plain HTTP on a non-loopback address |
| `--rate-limit`, `--rate-burst` | | Per-token request rate and burst (default: 5 per second, 20) |

An unknown pack name is an error. A selected pack whose dependency is missing is skipped: `kg` without `--db`, `mcp` without `--mcp-config`.

## Approval and annotations

`store_knowledge` and `kg_ingest` write to the knowledge graph, so saige marks them for human approval.
Inside a saige agent the loop pauses for that approval. Over MCP the server enforces it, according to `--approval`.
The server treats a marker of any kind, such as `audit` or `rate_limit`, as needing approval, because it has no agent loop to resolve the marker otherwise.

| Mode | Behavior |
|------|----------|
| `elicit` | Asks the user through MCP elicitation and runs the tool only on an explicit yes. A client without elicitation gets a refusal that names the other modes. |
| `host` | Runs the tool and relies on the client's own per-tool permission prompt. |
| `deny` | Refuses every marked tool. |

Every tool is published with MCP annotations. Read-only tools such as `read_file` carry `readOnlyHint`, and marked tools carry `destructiveHint`, so a client can decide which calls to confirm.
Use `--read-only` to remove the mutating tools entirely.

## Tool Packs

| Pack | Tools | Needs |
|------|-------|-------|
| `research` | `web_search`, `file_search`, `read_file`, `search_knowledge`, `store_knowledge`, `get_knowledge_graph` | `--searxng-url` for `web_search`, `--db` for the knowledge tools |
| `kg` | `kg_search`, `kg_ingest` | `--db` |
| `eval` | `eval_run`, `eval_compare` | nothing |
| `mcp` | `mcp_probe`, `mcp_catalog` | `--mcp-config` |

See [`tools/research`](../../tools/research/README.md) for the research tools.

`eval_run` scores outputs you pass in with saige's deterministic scorers (`exact_match`, `token_f1`, `contains`, `regex_count`, `json_schema` and the others in `eval.DefaultRegistry`) and returns per-case scores and the aggregate. `eval_compare` scores a baseline and a candidate set with the same scorers and returns the paired comparison: per-metric deltas with intervals and the cases that improved or regressed. Neither calls a model or writes anything.

```json
{
  "cases": [{"id": "fr", "output": "Paris", "ground_truth": "Paris"}],
  "scorers": [{"kind": "exact_match"}, {"kind": "contains", "params": {"substrings": ["Paris"]}}]
}
```

`mcp_probe` connects to configured servers, lists their tools and reports `ok`, `unreachable`, `auth_rejected`, `blocked`, `timeout` or `protocol` for each. `mcp_catalog` returns one server's tools exactly as advertised, with a fingerprint per tool. Both accept only server names from `--mcp-config`, never a URL or command from the caller.

## Agent tool

`--agent` publishes a saige agent as one MCP tool (`saige_agent` unless `--agent-tool` renames it). Its input is `{"task": "..."}`; each call runs a fresh agent on the task with the selected packs as its tools and returns the agent's final answer.

```bash
saige-mcp --transport http --tools research --root ~/notes \
  --agent anthropic/claude-haiku-5-5 \
  --agent-system "Answer from the notes. Cite file paths." \
  --agent-schema answer.schema.json
```

With `--agent-schema`, the agent answers with a JSON object matching the schema, and the tool returns it as structured content (and publishes the schema as its output schema):

```json
{"type": "object", "required": ["answer", "sources"], "properties": {
  "answer": {"type": "string"},
  "sources": {"type": "array", "items": {"type": "string"}}
}}
```

The agent's provider credentials come from the environment, as for the `saige` CLI (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and so on).

A marked tool the agent calls is decided as a direct call would be: `elicit` asks the client, and `deny` refuses. `host` cannot apply, because the client's permission prompt covered the agent tool and not the calls the agent makes, so those calls are refused with a message naming `--approval=elicit`. The model sees each refusal and can answer without the tool. The agent tool carries `destructiveHint` when any of its tools needs approval.

## Client Setup

### Claude Code

```bash
claude mcp add saige -- saige-mcp --tools research --searxng-url http://localhost:8080
```

Over HTTP:

```bash
claude mcp add --transport http saige http://127.0.0.1:8765/mcp --header "Authorization: Bearer $SAIGE_MCP_TOKEN"
```

Or add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "saige": {
      "command": "saige-mcp",
      "args": ["--tools", "research", "--searxng-url", "http://localhost:8080"]
    }
  }
}
```

### Codex

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.saige]
command = "saige-mcp"
args = ["--tools", "research", "--searxng-url", "http://localhost:8080"]
```

### Gemini CLI

Add to `~/.gemini/settings.json`:

```json
{
  "mcpServers": {
    "saige": {
      "command": "saige-mcp",
      "args": ["--tools", "research", "--searxng-url", "http://localhost:8080"]
    }
  }
}
```

## Related

- [MCP client](../../docs/mcp-client.md): connecting a saige agent to other MCP servers
- [`saige` CLI](../saige/README.md): interactive chat and standalone RAG/KG operations
- [Root README](../../README.md): project overview and installation
