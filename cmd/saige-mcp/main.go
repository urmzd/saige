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

	"github.com/urmzd/saige/cmd/internal/approvals"
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

	agentRef := flag.String("agent", "", "Expose a saige agent as one MCP tool: an agent definition in --agents-dir (NAME or NAME@RANGE), or a catalog preset or provider/model")
	agentsDir := flag.String("agents-dir", "", "Directory of agent definitions --agent may name")
	agentCatalog := flag.String("agent-catalog", "", "Catalog file for --agent (default: the embedded catalog)")
	agentName := flag.String("agent-tool", defaultAgentTool, "Name of the agent tool")
	agentDesc := flag.String("agent-description", "", "Description of the agent tool")
	agentSystem := flag.String("agent-system", "", "System prompt of the agent")
	agentSchema := flag.String("agent-schema", "", "JSON schema file; the agent then answers with a structured result")
	agentMaxIter := flag.Int("agent-max-iter", 10, "Most model turns one agent call may take")
	agentTimeout := flag.Duration("agent-timeout", 5*time.Minute, "Time limit of one agent call, not counting time held for an approval")
	approvalsDir := flag.String("approvals-dir", approvals.DefaultDir(os.Getenv), "Where held approvals wait for saige approvals, for clients without elicitation [$"+approvals.EnvDir+"]")
	approvalTimeout := flag.Duration("approval-timeout", 15*time.Minute, "How long a held approval waits for a decision before its run ends")
	approvalWait := flag.Duration("approval-wait", 30*time.Second, "How long one resume call waits for a decision before it reports the approval still pending")

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
	if mode == approvalElicit && *agentRef != "" {
		b.held = newHeldApprovals(ctx, *approvalsDir, *approvalTimeout, *approvalWait)
		defer b.held.close()
	}
	server := newServer(version)
	defs := registry.Definitions()
	for _, def := range defs {
		tool, _ := registry.Get(def.Name)
		b.register(server, tool)
	}
	served := len(defs)

	if *agentRef != "" {
		af := agentFlags{
			ref: *agentRef, catalog: *agentCatalog, name: *agentName, description: *agentDesc,
			system: *agentSystem, schemaFile: *agentSchema, maxIter: *agentMaxIter, timeout: *agentTimeout,
		}
		var at agentTool
		found := false
		if *agentsDir != "" {
			set := map[string]bool{}
			flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
			at, found, err = newDefinitionTool(ctx, definitionFlags{agentFlags: af, dir: *agentsDir, root: *root, set: set}, registry)
			if err != nil {
				log.Fatalf("saige-mcp: %v", err)
			}
		}
		if !found {
			if at, err = newAgentTool(ctx, af, registry); err != nil {
				log.Fatalf("saige-mcp: %v", err)
			}
		}
		if at.gated && b.held != nil {
			if _, clash := registry.Get(at.name + resumeSuffix); clash {
				log.Fatalf("saige-mcp: the resume tool %s collides with a pack tool; set --agent-tool", at.name+resumeSuffix)
			}
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
