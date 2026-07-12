# saige-mcp

MCP server binary that exposes saige tool packs over the [Model Context Protocol](https://modelcontextprotocol.io/) (stdio transport). Any MCP-compatible client can use saige tools: Claude Code, Codex, Gemini CLI, and others.

```bash
go install github.com/urmzd/saige/cmd/saige-mcp@latest
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

- [`saige` CLI](../saige/README.md): interactive chat and standalone RAG/KG operations
- [Root README](../../README.md): project overview and installation
