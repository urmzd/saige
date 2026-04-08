package research

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/urmzd/saige/agent/types"
	kgtypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag/source/searxng"
)

type searchResultJSON struct {
	Index   int    `json:"index"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// WebSearchTool implements types.Tool for web searching via SearXNG.
type WebSearchTool struct {
	searcher   *searxng.Client
	graph      kgtypes.Graph
	groupID    string
	autoIngest bool
}

func NewWebSearchTool(s *searxng.Client, graph kgtypes.Graph) *WebSearchTool {
	return &WebSearchTool{searcher: s, graph: graph}
}

func (t *WebSearchTool) WithGroupID(id string) *WebSearchTool {
	return &WebSearchTool{searcher: t.searcher, graph: t.graph, groupID: id, autoIngest: t.autoIngest}
}

// WithAutoIngest returns a copy with auto-ingestion into the knowledge graph enabled.
func (t *WebSearchTool) WithAutoIngest() *WebSearchTool {
	return &WebSearchTool{searcher: t.searcher, graph: t.graph, groupID: t.groupID, autoIngest: true}
}

func (t *WebSearchTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "web_search",
		Description: "Search the web for current information on a topic using SearXNG (a privacy-respecting metasearch engine). Results come from third-party search engines and may be inaccurate or outdated. Returns a JSON array of results with index, title, url, and snippet. Use the index numbers as citation references [1], [2], etc.",
		Parameters: types.ParameterSchema{
			Type:     "object",
			Required: []string{"query"},
			Properties: map[string]types.PropertyDef{
				"query": {Type: "string", Description: "The search query"},
			},
		},
	}
}

func (t *WebSearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("web_search: query is required")
	}

	results, err := t.searcher.Search(ctx, query)
	if err != nil {
		return "", err
	}

	limit := min(len(results), 5)

	items := make([]searchResultJSON, 0, limit)
	for i, r := range results[:limit] {
		snippet := r.Snippet
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		items = append(items, searchResultJSON{
			Index:   i + 1,
			Title:   r.Title,
			URL:     r.URL,
			Snippet: snippet,
		})
	}

	if len(items) == 0 {
		return "[]", nil
	}

	if t.autoIngest && t.graph != nil {
		for _, item := range items {
			body := fmt.Sprintf("Title: %s\nURL: %s\nSnippet: %s", item.Title, item.URL, item.Snippet)
			input := &kgtypes.EpisodeInput{
				Name:    fmt.Sprintf("%s - Result %d", query, item.Index),
				Body:    body,
				Source:  item.URL,
				GroupID: t.groupID,
			}
			if _, err := t.graph.IngestEpisode(ctx, input); err != nil {
				log.Printf("[web_search] auto-ingest error (result %d): %v", item.Index, err)
			}
		}
	}

	out, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("web_search: marshal results: %w", err)
	}
	return string(out), nil
}
