# tools/research

Six agent tools for web search, local file exploration, and knowledge graph CRUD. Register them with any saige agent or expose them over MCP via [`saige-mcp`](../../cmd/saige-mcp/README.md).

```go
import (
    "github.com/urmzd/saige/tools/research"
    "github.com/urmzd/saige/rag/source/searxng"
)

researchTools := research.NewTools(searxng.New("http://localhost:8080"), graph, ".")
```

## Tools

| Tool | Description |
|------|-------------|
| `web_search` | Search the web via SearXNG (privacy-respecting metasearch engine). Results come from third-party search engines and may be inaccurate or outdated. |
| `file_search` | Regex search across local file contents with glob filtering |
| `read_file` | Read file contents with line numbers, offset, and limit |
| `search_knowledge` | Query the knowledge graph for stored facts |
| `store_knowledge` | Extract entities and relationships from text into the knowledge graph |
| `get_knowledge_graph` | Visualize the knowledge graph as a text summary |

All constructor parameters are optional except the root directory. Pass `nil` for `searxng.Client` (omits `web_search`) or `nil` for `Graph` (omits the knowledge tools).

## Related

- [`knowledge`](../../knowledge/README.md): the graph backing the knowledge tools
- [`rag/source/searxng`](../../rag/source/searxng/): the SearXNG HTTP client
- [Root README](../../README.md): project overview and installation
