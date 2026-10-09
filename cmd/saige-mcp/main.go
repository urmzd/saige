// Command saige-mcp exposes saige tools over the Model Context Protocol (stdio).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	agenttypes "github.com/urmzd/saige/agent/types"
	kgtool "github.com/urmzd/saige/rag/knowledge/tool"
	kgtypes "github.com/urmzd/saige/rag/knowledge/types"
	"github.com/urmzd/saige/rag/source/searxng"
	"github.com/urmzd/saige/tools/research"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	toolPacks := flag.String("tools", "all", "Comma-separated tool packs to expose: research,kg,all")
	dbDSN := flag.String("db", os.Getenv("SAIGE_DB"), "PostgreSQL DSN (for KG tools) [$SAIGE_DB]")
	searxngURL := flag.String("searxng-url", os.Getenv("SEARXNG_URL"), "SearXNG base URL [$SEARXNG_URL]")
	root := flag.String("root", ".", "Root directory for file search/read tools")
	readOnly := flag.Bool("read-only", false, "Omit every mutating tool (store_knowledge, kg_ingest)")
	approval := flag.String("approval", string(approvalElicit), "How marked tools run (every marker kind needs approval): elicit (ask through the MCP client), host (rely on the client's own permission prompt), deny")
	flag.Parse()

	mode, err := parseApprovalMode(*approval)
	if err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	packs := parsePacks(*toolPacks)

	// Connect to PostgreSQL if any tool pack needs it.
	var pool *pgxpool.Pool
	needsDB := packs["kg"] || packs["research"]
	if needsDB && *dbDSN != "" {
		pool, err = pgxpool.New(ctx, *dbDSN)
		if err != nil {
			log.Fatalf("connect to database: %v", err)
		}
		defer pool.Close()
	}

	registry := agenttypes.NewToolRegistry()

	if packs["research"] {
		var client *searxng.Client
		if *searxngURL != "" {
			client = searxng.New(*searxngURL)
		}
		var graph kgtypes.Graph
		if pool != nil {
			graph = mustGraph(ctx, pool)
		}
		var opts []research.Option
		if *readOnly {
			opts = append(opts, research.ReadOnly())
		}
		for _, t := range research.NewTools(client, graph, *root, opts...) {
			registry.Register(t)
		}
	}

	if packs["kg"] && pool != nil {
		graph := mustGraph(ctx, pool)
		var opts []kgtool.Option
		if *readOnly {
			opts = append(opts, kgtool.ReadOnly())
		}
		for _, t := range kgtool.NewTools(graph, opts...) {
			registry.Register(t)
		}
	}

	defs := registry.Definitions()
	if len(defs) == 0 {
		fmt.Fprintln(os.Stderr, "saige-mcp: no tools registered (check --tools, --db, --searxng-url flags)")
		os.Exit(1)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "saige-mcp",
		Version: version,
	}, nil)

	b := bridge{approval: mode}
	for _, def := range defs {
		tool, _ := registry.Get(def.Name)
		b.register(server, tool)
	}

	log.Printf("saige-mcp: serving %d tools over stdio", len(defs))
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}
}

func parsePacks(s string) map[string]bool {
	packs := make(map[string]bool)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "all" {
			return map[string]bool{"research": true, "kg": true}
		}
		packs[p] = true
	}
	return packs
}
