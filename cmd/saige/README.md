# saige CLI

Interactive chat, single-shot queries, and standalone RAG/KG operations from the terminal.

```bash
go install github.com/urmzd/saige/cmd/saige@latest
```

Or use the [install script](../../README.md#installation) for a pre-built binary.

## Chat and Ask

```bash
# Interactive multi-turn chat (Bubble Tea TUI)
saige chat
saige chat --provider anthropic --model claude-sonnet-4-6-20250514
saige chat --verbose  # plain-text mode for pipes/CI

# Single-shot question (pipe-friendly)
saige ask "What is retrieval-augmented generation?"
echo "Explain transformers" | saige ask --raw

# With RAG/KG tools attached to the agent
saige chat --rag-db "postgres://localhost/mydb" --kg-db "postgres://localhost/mydb"
saige ask --rag-db "$SAIGE_RAG_DB" "What does the paper say about attention?"
```

## Standalone RAG Operations

JSON output, suitable for scripting:

```bash
saige rag ingest --db "$SAIGE_RAG_DB" --file paper.pdf --mime application/pdf
saige rag search --db "$SAIGE_RAG_DB" --query "attention mechanism"
saige rag lookup --db "$SAIGE_RAG_DB" --uuid <variant-uuid>
saige rag delete --db "$SAIGE_RAG_DB" --uuid <doc-uuid>
```

## Standalone KG Operations

```bash
saige kg ingest --db "$SAIGE_KG_DB" --name "meeting" --text "Alice presented the roadmap."
saige kg search --db "$SAIGE_KG_DB" --query "Who presented?"
saige kg graph  --db "$SAIGE_KG_DB" --limit 50
saige kg node   --db "$SAIGE_KG_DB" --id <entity-uuid> --depth 2
```

## Provider Auto-Detection

The CLI checks for `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GOOGLE_API_KEY` in order, falling back to Ollama (no key needed). Override with `--provider` or `SAIGE_PROVIDER`.

> **Note:** Anthropic has no embedding API. With `--provider anthropic` plus RAG/KG
> features, set an additional key (e.g. `OPENAI_API_KEY`) so the CLI can pick a
> separate embedder.

## Related

- [`saige-mcp`](../saige-mcp/README.md): expose saige tools to Claude Code, Codex, and other MCP clients
- [`agent`](../../agent/README.md): the SDK powering chat and ask
- [Root README](../../README.md): project overview and installation
