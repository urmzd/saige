// Package main demonstrates running an agent as a DBOS-backed durable workflow.
// Each LLM call and tool execution becomes a memoized durable step: if the
// process crashes mid-run, Launch() recovers the workflow and resumes it from
// its last completed step instead of repeating (and re-billing) the work.
//
// Requires a reachable PostgreSQL instance. The DBOS engine shares the same
// pgxpool used by the agent's pgstore, and uses the workflow ID as an
// idempotency key so retries from a UI are exactly-once.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentsdk "github.com/urmzd/saige/agent"
	durabledbos "github.com/urmzd/saige/agent/durable/dbos"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://localhost:5432/saige?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Build the agent normally — the core package never imports dbos.
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "researcher",
		SystemPrompt: "You research questions and summarize concisely.",
		Provider:     ollama.NewAdapter(ollama.NewClient("http://localhost:11434", "llama3.2", "")),
	})

	// The durable engine shares the same Postgres pool.
	engine, err := durabledbos.NewEngine(ctx, "saige-demo", pool, dsn)
	if err != nil {
		log.Fatalf("dbos engine: %v", err)
	}
	defer engine.Shutdown(30 * time.Second)

	// Register the agent-run workflow BEFORE Launch; Launch recovers any
	// in-flight workflows from a previous crash.
	wf := engine.RegisterAgent(agent, "saige.agent.run")
	if err := engine.Launch(); err != nil {
		log.Fatalf("launch: %v", err)
	}

	// Run durably. The workflow ID is the idempotency key — a second call with
	// the same ID returns a handle to the existing run instead of re-executing.
	handle, err := engine.Run(wf, durabledbos.RunInput{
		Messages: []types.Message{types.NewUserMessage("Summarize the benefits of durable workflows.")},
		Branch:   agent.Tree().Active(),
	}, "demo-turn-1")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	out, err := handle.GetResult()
	if err != nil {
		log.Fatalf("result: %v", err)
	}
	if out.Final != nil {
		for _, c := range out.Final.Content {
			if t, ok := c.(types.TextContent); ok {
				log.Printf("final: %s", t.Text)
			}
		}
	}
}
