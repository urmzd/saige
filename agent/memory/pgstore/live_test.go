package pgstore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/memory"
	agentpg "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// TestLiveCrossSessionRecall runs a real model and a real embedder: the
// agent saves a fact in session 1, recalls it with the recall tool in
// session 2, and answers from injected conversation turns in session 3.
// It needs SAIGE_TEST_POSTGRES_DSN, SAIGE_TEST_OLLAMA_HOST (with
// nomic-embed-text pulled), and ANTHROPIC_API_KEY. SAIGE_TEST_ANTHROPIC_MODEL
// overrides the model.
func TestLiveCrossSessionRecall(t *testing.T) {
	host, key := os.Getenv("SAIGE_TEST_OLLAMA_HOST"), os.Getenv("ANTHROPIC_API_KEY")
	if host == "" || key == "" {
		t.Skip("SAIGE_TEST_OLLAMA_HOST and ANTHROPIC_API_KEY are required for the live memory test")
	}
	pool := testPool(t)
	if _, err := pool.Exec(context.Background(), `TRUNCATE agent_node, agent_branch, agent_checkpoint`); err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("SAIGE_TEST_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	embedModel := os.Getenv("SAIGE_TEST_OLLAMA_EMBED_MODEL")
	if embedModel == "" {
		embedModel = "nomic-embed-text"
	}
	store, err := New(pool, Config{Embedder: ollama.NewEmbedder(ollama.NewClient(host, "", embedModel))})
	if err != nil {
		t.Fatal(err)
	}
	scope := memory.Scope{Tenant: "acme", Subject: "user-1"}
	policy := memory.Policy{
		AutoApprove: true,
		Scope:       func(context.Context, string) (memory.Scope, error) { return scope, nil },
	}
	provider := anthropic.NewAdapter(key, model)

	run := func(name, conversation string, tools []types.Tool, input ...types.Message) string {
		t.Helper()
		cfg := agentsdk.AgentConfig{
			Name:         "assistant",
			SystemPrompt: "You are a concise assistant with long-term memory. Save facts the user asks you to remember with the remember tool. Look facts up with your recall tools before saying you do not know.",
			Provider:     provider,
			Tools:        types.NewToolRegistry(tools...),
		}
		if conversation != "" {
			conv, err := agentpg.NewScopedStore(pool, scope.Tenant, conversation, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Store = conv
		}
		stream := agentsdk.NewAgent(cfg, agentsdk.WithMaxIter(6)).Invoke(ctx, input)
		text := agenttest.CollectText(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: %s", name, text)
		return text
	}

	// Session 1: remember a fact.
	run("session 1", "session-1", memory.Tools(store, policy),
		types.NewUserMessage("Please remember for future sessions: our deploy window is Thursdays at 14:00 UTC."))
	recs, err := store.Recall(ctx, scope, "", 0)
	if err != nil || len(recs) == 0 || !strings.Contains(strings.ToLower(recs[0].Content), "thursday") {
		t.Fatalf("session 1 stored %q, %v", contents(recs), err)
	}
	conv1, _ := agentpg.ScopedConversationID(scope.Tenant, "session-1")
	if n, err := store.IndexConversation(ctx, scope, conv1); err != nil || n == 0 {
		t.Fatalf("IndexConversation = %d, %v", n, err)
	}

	// Session 2: a fresh agent recalls it through the recall tool.
	answer := run("session 2", "", memory.Tools(store, policy),
		types.NewUserMessage("When is our deploy window?"))
	if !strings.Contains(strings.ToLower(answer), "thursday") {
		t.Fatalf("session 2 answer %q does not mention Thursday", answer)
	}

	// Session 3: no tools; past turns are injected at the start.
	inject := policy
	inject.Recall = memory.RecallByInjection
	msg, ok, err := inject.StartMessage(ctx, store.Conversations(), "assistant", "deploy window discussion")
	if err != nil || !ok {
		t.Fatalf("StartMessage = %v, %v", ok, err)
	}
	answer = run("session 3", "", nil, msg,
		types.NewUserMessage("Based on our earlier conversation, what did we say about deployments?"))
	if !strings.Contains(strings.ToLower(answer), "thursday") {
		t.Fatalf("session 3 answer %q does not mention Thursday", answer)
	}
}
