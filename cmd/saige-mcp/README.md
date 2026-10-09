# saige-mcp

MCP server binary that exposes saige tool packs over the [Model Context Protocol](https://modelcontextprotocol.io/) (stdio transport). Any MCP-compatible client can use saige tools: Claude Code, Codex, Gemini CLI, and others.

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

# Expose everything
saige-mcp --tools all --db "$SAIGE_DB" --searxng-url http://localhost:8080
```

## Flags

| Flag | Env | Description |
|------|-----|-------------|
| `--tools` | | Comma-separated tool packs: `research`, `kg`, `all` (default: `all`) |
| `--db` | `SAIGE_DB` | PostgreSQL DSN for KG tools |
| `--searxng-url` | `SEARXNG_URL` | SearXNG base URL for web search |
| `--root` | | Root directory for file search/read (default: `.`) |
| `--read-only` | | Omit every mutating tool (`store_knowledge`, `kg_ingest`) |
| `--approval` | | How marked tools run: `elicit` (default), `host`, or `deny` |

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

| Pack | Tools |
|------|-------|
| `research` | `web_search`, `file_search`, `read_file`, `search_knowledge`, `store_knowledge`, `get_knowledge_graph` |
| `kg` | `kg_search`, `kg_ingest` |

See [`tools/research`](../../tools/research/README.md) for tool details.

## Client Setup

### Claude Code

```bash
claude mcp add saige -- saige-mcp --tools research --searxng-url http://localhost:8080
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
