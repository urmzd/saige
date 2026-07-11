package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentsdk "github.com/urmzd/saige/agent"
	durabledbos "github.com/urmzd/saige/agent/durable/dbos"
	agentpgstore "github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// TestDBOSDurableRun executes an agent as a DBOS workflow end to end: engine
// bootstrap against Postgres, workflow registration, durable run, result.
func TestDBOSDurableRun(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 15*time.Minute)

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "durable",
		SystemPrompt: "You are a concise assistant.",
		Provider:     ollama.NewAdapter(client),
	})

	engine, err := durabledbos.NewEngine(ctx, "saige-it-durable", pool, pgDSN(t))
	if err != nil {
		t.Fatalf("dbos engine: %v", err)
	}
	defer engine.Shutdown(30 * time.Second)

	wf := engine.RegisterAgent(agent, "saige.it.durable")
	if err := engine.Launch(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	handle, err := engine.Run(wf, durabledbos.RunInput{
		Messages: []types.Message{types.NewUserMessage("Reply with exactly one word: durable")},
		Branch:   agent.Tree().Active(),
	}, "it-durable-"+uuid.NewString())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	out, err := handle.GetResult()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	text := assistantText(out.Final)
	if strings.TrimSpace(text) == "" {
		t.Fatal("expected non-empty durable result")
	}
	t.Logf("durable result: %q", strings.TrimSpace(text))
}

// TestDBOSIdempotentReplay verifies the workflow ID acts as an idempotency
// key: re-running the same ID returns the memoized result without invoking
// the LLM or tools again.
func TestDBOSIdempotentReplay(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 15*time.Minute)

	tool, calls := addTool()
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "idempotent",
		SystemPrompt: "You are a calculator. You must use the add tool for any addition; never compute it yourself.",
		Provider:     ollama.NewAdapter(client),
		Tools:        types.NewToolRegistry(tool),
	})

	engine, err := durabledbos.NewEngine(ctx, "saige-it-idem", pool, pgDSN(t))
	if err != nil {
		t.Fatalf("dbos engine: %v", err)
	}
	defer engine.Shutdown(30 * time.Second)

	wf := engine.RegisterAgent(agent, "saige.it.idem")
	if err := engine.Launch(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	input := durabledbos.RunInput{
		Messages: []types.Message{types.NewUserMessage("What is 2 + 3? Use the add tool.")},
		Branch:   agent.Tree().Active(),
	}
	workflowID := "it-idem-" + uuid.NewString()

	first, err := engine.Run(wf, input, workflowID)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	out1, err := first.GetResult()
	if err != nil {
		t.Fatalf("first result: %v", err)
	}
	if *calls == 0 {
		t.Fatal("add tool was never executed on the first run")
	}
	callsAfterFirst := *calls

	// Same workflow ID: DBOS must return the completed run, not re-execute.
	second, err := engine.Run(wf, input, workflowID)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	out2, err := second.GetResult()
	if err != nil {
		t.Fatalf("second result: %v", err)
	}

	if *calls != callsAfterFirst {
		t.Errorf("replay re-executed tools: %d calls before, %d after", callsAfterFirst, *calls)
	}
	if got, want := assistantText(out2.Final), assistantText(out1.Final); got != want {
		t.Errorf("replay returned a different result:\nfirst:  %q\nsecond: %q", want, got)
	}
	t.Logf("idempotent replay verified (tool calls stayed at %d)", callsAfterFirst)
}

// TestDBOSFullStack wires every layer together: Ollama inference, a DBOS
// durable workflow, and Postgres persistence of the conversation tree — then
// proves the turn survived by rehydrating it from the database.
func TestDBOSFullStack(t *testing.T) {
	pool := requirePostgres(t)
	client := requireOllama(t)
	ctx := testContext(t, 15*time.Minute)

	store := agentpgstore.NewStore(pool, uuid.NewString(), nil)
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "fullstack",
		SystemPrompt: "You are a concise assistant.",
		Provider:     ollama.NewAdapter(client),
	}, agentsdk.WithStore(store))

	engine, err := durabledbos.NewEngine(ctx, "saige-it-fullstack", pool, pgDSN(t))
	if err != nil {
		t.Fatalf("dbos engine: %v", err)
	}
	defer engine.Shutdown(30 * time.Second)

	wf := engine.RegisterAgent(agent, "saige.it.fullstack")
	if err := engine.Launch(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	handle, err := engine.Run(wf, durabledbos.RunInput{
		Messages: []types.Message{types.NewUserMessage("Reply with exactly one word: persisted")},
		Branch:   agent.Tree().Active(),
	}, "it-fullstack-"+uuid.NewString())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out, err := handle.GetResult()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if strings.TrimSpace(assistantText(out.Final)) == "" {
		t.Fatal("expected non-empty durable result")
	}

	// The same turn must be recoverable from Postgres alone.
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
	var sawAssistant bool
	for _, m := range msgs {
		if _, ok := m.(types.AssistantMessage); ok {
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Error("rehydrated tree is missing the assistant response from the durable run")
	}
	t.Logf("full stack verified: %d messages persisted through DBOS + Postgres", len(msgs))
}
