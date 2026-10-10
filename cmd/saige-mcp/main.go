// Command saige-mcp exposes saige tools over the Model Context Protocol, on
// stdio or streamable HTTP.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// Transports.
const (
	transportStdio = "stdio"
	transportHTTP  = "http"
)

func main() {
	toolPacks := flag.String("tools", "all", "Comma-separated tool packs to expose: research,kg,eval,mcp,all,none")
	dbDSN := flag.String("db", os.Getenv("SAIGE_DB"), "PostgreSQL DSN (for KG tools) [$SAIGE_DB]")
	searxngURL := flag.String("searxng-url", os.Getenv("SEARXNG_URL"), "SearXNG base URL [$SEARXNG_URL]")
	root := flag.String("root", ".", "Root directory for file search/read tools")
	readOnly := flag.Bool("read-only", false, "Omit every mutating tool (store_knowledge, kg_ingest)")
	approval := flag.String("approval", string(approvalElicit), "How marked tools run (every marker kind needs approval): elicit (ask through the MCP client), host (rely on the client's own permission prompt), deny")
	mcpConfig := flag.String("mcp-config", "", "MCP configuration file (mcpServers) whose servers the mcp pack probes and lists")

	agentRef := flag.String("agent", "", "Expose a saige agent as one MCP tool, running this catalog preset or provider/model")
	agentCatalog := flag.String("agent-catalog", "", "Catalog file for --agent (default: the embedded catalog)")
	agentName := flag.String("agent-tool", defaultAgentTool, "Name of the agent tool")
	agentDesc := flag.String("agent-description", "", "Description of the agent tool")
	agentSystem := flag.String("agent-system", "", "System prompt of the agent")
	agentSchema := flag.String("agent-schema", "", "JSON schema file; the agent then answers with a structured result")
	agentMaxIter := flag.Int("agent-max-iter", 10, "Most model turns one agent call may take")
	agentTimeout := flag.Duration("agent-timeout", 5*time.Minute, "Time limit of one agent call")

	transport := flag.String("transport", transportStdio, "Transport: stdio or http (streamable HTTP)")
	addr := flag.String("addr", defaultAddr, "Listen address for --transport http")
	path := flag.String("path", defaultPath, "Endpoint path for --transport http")
	tokenEnv := flag.String("token-env", defaultTokenEnv, "Environment variable holding the bearer tokens, comma-separated, for --transport http")
	tokenFile := flag.String("token-file", "", "File holding the bearer tokens, one per line, for --transport http")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file for --transport http")
	tlsKey := flag.String("tls-key", "", "TLS key file for --transport http")
	allowInsecure := flag.Bool("allow-insecure-bind", false, "Allow plain HTTP on a non-loopback address, for a TLS-terminating proxy in front")
	rateLimit := flag.Float64("rate-limit", 5, "Requests per second allowed per token for --transport http")
	rateBurst := flag.Int("rate-burst", 20, "Burst of requests allowed per token for --transport http")
	flag.Parse()

	mode, err := parseApprovalMode(*approval)
	if err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}
	packs, err := parsePacks(*toolPacks)
	if err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Check the HTTP settings before connecting to anything.
	var httpCfg httpConfig
	switch *transport {
	case transportStdio:
	case transportHTTP:
		httpCfg, err = newHTTPConfig(httpFlags{
			addr: *addr, path: *path, tokenEnv: *tokenEnv, tokenFile: *tokenFile,
			tlsCert: *tlsCert, tlsKey: *tlsKey, allowInsecure: *allowInsecure,
			rate: *rateLimit, burst: *rateBurst,
		}, os.Getenv)
		if err != nil {
			log.Fatalf("saige-mcp: %v", err)
		}
	default:
		log.Fatalf("saige-mcp: unknown --transport %q: use stdio or http", *transport)
	}

	registry, cleanup, err := buildRegistry(ctx, packConfig{
		packs: packs, dbDSN: *dbDSN, searxngURL: *searxngURL, root: *root,
		readOnly: *readOnly, mcpConfig: *mcpConfig,
	})
	if err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}
	defer cleanup()

	b := bridge{approval: mode}
	server := newServer(version)
	defs := registry.Definitions()
	for _, def := range defs {
		tool, _ := registry.Get(def.Name)
		b.register(server, tool)
	}
	served := len(defs)

	if *agentRef != "" {
		at, err := newAgentTool(ctx, agentFlags{
			ref: *agentRef, catalog: *agentCatalog, name: *agentName, description: *agentDesc,
			system: *agentSystem, schemaFile: *agentSchema, maxIter: *agentMaxIter, timeout: *agentTimeout,
		}, registry)
		if err != nil {
			log.Fatalf("saige-mcp: %v", err)
		}
		b.registerAgent(server, at)
		served++
	}

	if served == 0 {
		fmt.Fprintln(os.Stderr, "saige-mcp: no tools registered (check --tools, --db, --searxng-url, --mcp-config and --agent)")
		os.Exit(1)
	}

	if *transport == transportHTTP {
		log.Printf("saige-mcp: serving %d tools over streamable HTTP at %s", served, httpCfg.endpoint())
		err = serveHTTP(ctx, server, httpCfg)
	} else {
		log.Printf("saige-mcp: serving %d tools over stdio", served)
		err = server.Run(ctx, &mcp.StdioTransport{})
	}
	if err != nil {
		log.Fatalf("saige-mcp: %v", err)
	}
}
