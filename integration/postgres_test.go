package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentsdk "github.com/urmzd/saige/agent"
	agentpgstore "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/knowledge"
	knowledgetypes "github.com/urmzd/saige/knowledge/types"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/extractor"
	ragpgstore "github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// TestAgentPersistencePostgres runs a live agent turn with a Postgres-backed
// store, then rehydrates the conversation tree from the database and verifies
// the persisted history matches what was streamed.
func TestAgentPersistencePostgres(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 10*time.Minute)

	store := agentpgstore.NewStore(pool, uuid.NewString(), nil)
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "persistent",
		SystemPrompt: "You are a concise assistant.",
		Provider:     ollama.NewAdapter(client),
	}, agentsdk.WithStore(store))

	const question = "Reply with exactly one word: pong"
	stream := agent.Invoke(ctx, []types.Message{types.NewUserMessage(question)})
	text, _, err := drainStream(stream)
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("expected non-empty response")
	}

	// Rehydrate from Postgres the way a restarted process would.
	root := agent.Tree().Root()
	if root == nil {
		t.Fatal("agent tree has no root")
	}
	loaded, err := agentsdk.LoadTreeFromStore(ctx, store, root.ID, "")
	if err != nil {
		t.Fatalf("LoadTreeFromStore: %v", err)
	}
	msgs, err := loaded.FlattenBranch(loaded.Active())
	if err != nil {
		t.Fatalf("flatten rehydrated tree: %v", err)
	}

	var sawUser, sawAssistant bool
	for _, m := range msgs {
		switch v := m.(type) {
		case types.UserMessage:
			for _, c := range v.Content {
				if tc, ok := c.(types.TextContent); ok && strings.Contains(tc.Text, question) {
					sawUser = true
				}
			}
		case types.AssistantMessage:
			sawAssistant = true
		}
	}
	if !sawUser {
		t.Error("rehydrated tree is missing the user message")
	}
	if !sawAssistant {
		t.Error("rehydrated tree is missing the assistant response")
	}
	t.Logf("rehydrated %d messages from Postgres", len(msgs))
}

// variantTextEmbedder adapts the batch text Embedder to RAG's VariantEmbedder.
type variantTextEmbedder struct {
	embedder *ollama.OllamaEmbedder
}

func (e variantTextEmbedder) Embed(ctx context.Context, variants []ragtypes.ContentVariant) ([][]float32, error) {
	texts := make([]string, len(variants))
	for i, v := range variants {
		texts[i] = v.Text
	}
	return e.embedder.Embed(ctx, texts)
}

// TestRAGPipelinePostgres runs the full RAG path against pgvector with live
// Ollama embeddings: ingest, hybrid search (vector + BM25), reconstruct, delete.
func TestRAGPipelinePostgres(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 10*time.Minute)

	embedder := ollama.NewEmbedder(client)
	requireEmbedDim(t, ctx, embedder)
	truncate(t, pool, "rag_document", "rag_original", "rag_section", "rag_variant")

	pipe, err := rag.NewPipeline(
		rag.WithStore(ragpgstore.NewStore(pool, nil)),
		rag.WithContentExtractor(extractor.NewAuto()),
		rag.WithEmbedders(embedderregistry.NewTextOnly(variantTextEmbedder{embedder})),
		rag.WithRecursiveChunker(256, 25),
		rag.WithBM25(nil),
	)
	if err != nil {
		t.Fatalf("create pipeline: %v", err)
	}

	docs := map[string]string{
		"mem://pasta": "To cook pasta al dente, boil it in salted water for one minute less than the package suggests. Fresh pasta cooks in two to three minutes.",
		"mem://go":    "Go is a statically typed programming language with goroutines for concurrency and channels for communication between them.",
		"mem://whale": "Blue whales are the largest animals ever known to have lived, reaching lengths of up to thirty meters and feeding mostly on krill.",
	}
	var pastaDoc string
	for uri, body := range docs {
		res, err := pipe.Ingest(ctx, &ragtypes.RawDocument{
			SourceURI: uri,
			MIMEType:  "text/plain",
			Data:      []byte(body),
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", uri, err)
		}
		if res.Variants == 0 {
			t.Fatalf("ingest %s produced no variants", uri)
		}
		if uri == "mem://pasta" {
			pastaDoc = res.DocumentUUID
		}
	}

	sr, err := pipe.Search(ctx, "how long should I boil pasta", ragtypes.WithLimit(3))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Hits) == 0 {
		t.Fatal("search returned no hits")
	}
	top := sr.Hits[0]
	if !strings.Contains(strings.ToLower(top.Variant.Text), "pasta") {
		t.Errorf("expected top hit about pasta, got [%.4f] %q from %s",
			top.Score, top.Variant.Text, top.Provenance.SourceURI)
	}

	doc, err := pipe.Reconstruct(ctx, pastaDoc)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if len(doc.Sections) == 0 {
		t.Error("reconstructed document has no sections")
	}

	if err := pipe.Delete(ctx, pastaDoc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := pipe.Reconstruct(ctx, pastaDoc); err == nil {
		t.Error("expected reconstruct to fail after delete")
	}
	t.Logf("top hit [%.4f] %q", top.Score, top.Variant.Text)
}

// TestKnowledgeGraphPostgres exercises LLM-driven entity extraction into the
// pgvector-backed graph, then semantic fact search over it.
func TestKnowledgeGraphPostgres(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 15*time.Minute)

	requireEmbedDim(t, ctx, ollama.NewEmbedder(client))
	truncate(t, pool, "kg_entity", "kg_relation", "kg_episode", "kg_mention")

	graph, err := knowledge.NewGraph(ctx,
		knowledge.WithPostgres(pool),
		knowledge.WithExtractor(knowledge.NewOllamaExtractor(client)),
		knowledge.WithEmbedder(knowledge.NewOllamaEmbedder(client)),
	)
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}
	defer graph.Close(ctx)

	result, err := graph.IngestEpisode(ctx, &knowledgetypes.EpisodeInput{
		Name:    "team-intro",
		Body:    "Alice is a software engineer at Acme Corp. She uses Go for backend services.",
		Source:  "integration test",
		GroupID: "it",
	})
	if err != nil {
		t.Fatalf("ingest episode: %v", err)
	}
	if len(result.EntityNodes) == 0 {
		t.Fatalf("extractor found no entities; model %s may be too weak for extraction", client.Model)
	}
	t.Logf("extracted %d entities, %d edges", len(result.EntityNodes), len(result.EpisodicEdges))

	facts, err := graph.SearchFacts(ctx, "Where does Alice work?",
		knowledgetypes.WithLimit(5),
		knowledgetypes.WithGroupID("it"),
	)
	if err != nil {
		t.Fatalf("search facts: %v", err)
	}
	for _, f := range knowledge.FactsToStrings(facts.Facts) {
		t.Logf("fact: %s", f)
	}

	data, err := graph.GetGraph(ctx, 50)
	if err != nil {
		t.Fatalf("get graph: %v", err)
	}
	if len(data.Nodes) == 0 {
		t.Error("expected at least one node in the graph")
	}
}
