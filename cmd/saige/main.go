// Command saige is the CLI for the saige SDK.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

const version = "0.4.0"

// Provider name constants.
const (
	providerOllama    = "ollama"
	providerOpenAI    = "openai"
	providerGoogle    = "google"
	providerAnthropic = "anthropic"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch os.Args[1] {
	case "chat":
		runChat(ctx, os.Args[2:])
	case "ask":
		runAsk(ctx, os.Args[2:])
	case "rag":
		runRAG(ctx, os.Args[2:])
	case "kg":
		runKG(ctx, os.Args[2:])
	case "version":
		fmt.Printf("saige v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: saige <command> [flags]

Commands:
  chat     Interactive multi-turn chat session
  ask      Single-shot question (pipe-friendly)
  rag      RAG document operations (search, lookup, ingest, delete)
  kg       Knowledge graph operations (search, ingest, graph, node)
  version  Print version info
  help     Show this help

Provider flags (chat/ask):
  --provider   LLM provider: anthropic, openai, google, ollama (auto-detected)
  --model      Model name (provider-specific default)
  --system     System prompt

Connection flags (chat/ask):
  --rag-db     Postgres DSN to enable RAG tools [$SAIGE_RAG_DB]
  --kg-db      Postgres DSN to enable KG tools [$SAIGE_KG_DB]`)
}
