// Package integration contains end-to-end integration tests that exercise
// the SDK against live services: an Ollama server for LLM inference and
// embeddings, PostgreSQL/AlloyDB (with pgvector) for persistence and RAG,
// and DBOS for durable workflow execution.
//
// Tests are opt-in via environment variables and skip cleanly when a backing
// service is not configured, so plain `go test ./...` stays hermetic:
//
//	SAIGE_TEST_OLLAMA_HOST        Ollama base URL (e.g. http://localhost:11434)
//	SAIGE_TEST_OLLAMA_MODEL       chat model (default qwen3.5:4b)
//	SAIGE_TEST_OLLAMA_EMBED_MODEL embedding model (default nomic-embed-text)
//	SAIGE_TEST_POSTGRES_DSN       PostgreSQL or AlloyDB DSN with pgvector
//
// See integration/README.md for setup instructions, or run:
//
//	just integration-up
//	just test-integration
package integration
