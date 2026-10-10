package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	saigemcp "github.com/urmzd/saige/agent/mcp"
	agenttypes "github.com/urmzd/saige/agent/types"
	kgtool "github.com/urmzd/saige/rag/knowledge/tool"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/source/searxng"
	"github.com/urmzd/saige/tools/research"
)

// Tool packs.
const (
	packResearch = "research"
	packKG       = "kg"
	packEval     = "eval"
	packMCP      = "mcp"
)

var allPacks = []string{packResearch, packKG, packEval, packMCP}

// parsePacks reads --tools. "all" selects every pack and "none" selects
// none, which serves only the agent tool. An unknown name is an error, so a
// misspelled pack is not silently left out.
func parsePacks(s string) (map[string]bool, error) {
	packs := make(map[string]bool)
	for _, p := range strings.Split(s, ",") {
		switch p = strings.TrimSpace(strings.ToLower(p)); p {
		case "":
		case "all":
			for _, name := range allPacks {
				packs[name] = true
			}
		case "none":
		case packResearch, packKG, packEval, packMCP:
			packs[p] = true
		default:
			return nil, fmt.Errorf("unknown tool pack %q (available: %s, all, none)", p, strings.Join(allPacks, ", "))
		}
	}
	return packs, nil
}

// packConfig is what the packs are built from.
type packConfig struct {
	packs      map[string]bool
	dbDSN      string
	searxngURL string
	root       string
	readOnly   bool
	mcpConfig  string
}

// buildRegistry registers the tools of every selected pack. A pack that
// needs a dependency the flags do not provide (a database, an MCP
// configuration) is skipped, as "all" has always done.
func buildRegistry(ctx context.Context, cfg packConfig) (*agenttypes.ToolRegistry, func(), error) {
	registry := agenttypes.NewToolRegistry()
	cleanup := func() {}

	var pool *pgxpool.Pool
	if (cfg.packs[packKG] || cfg.packs[packResearch]) && cfg.dbDSN != "" {
		var err error
		if pool, err = pgxpool.New(ctx, cfg.dbDSN); err != nil {
			return nil, cleanup, fmt.Errorf("connect to database: %w", err)
		}
		cleanup = pool.Close
	}
	var graph kgtypes.Graph
	if pool != nil {
		graph = mustGraph(ctx, pool)
	}

	if cfg.packs[packResearch] {
		var client *searxng.Client
		if cfg.searxngURL != "" {
			client = searxng.New(cfg.searxngURL)
		}
		var opts []research.Option
		if cfg.readOnly {
			opts = append(opts, research.ReadOnly())
		}
		for _, t := range research.NewTools(client, graph, cfg.root, opts...) {
			registry.Register(t)
		}
	}

	if cfg.packs[packKG] && graph != nil {
		var opts []kgtool.Option
		if cfg.readOnly {
			opts = append(opts, kgtool.ReadOnly())
		}
		for _, t := range kgtool.NewTools(graph, opts...) {
			registry.Register(t)
		}
	}

	if cfg.packs[packEval] {
		for _, t := range evalTools() {
			registry.Register(t)
		}
	}

	if cfg.packs[packMCP] && cfg.mcpConfig != "" {
		specs, err := saigemcp.LoadConfig(cfg.mcpConfig)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		for _, t := range mcpTools(specs) {
			registry.Register(t)
		}
	}
	return registry, cleanup, nil
}
