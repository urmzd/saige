// Package main runs an agent durably: each LLM call and tool execution is a
// memoized step, so a crashed or restarted process resumes the run from its
// last completed step instead of repeating (and re-billing) the work. The run
// ID is the idempotency key: running the same ID again returns the recorded
// result.
//
// By default the run uses the local engine, which keeps each run in a private
// directory on this machine:
//
//	go run ./examples/agent/durable/
//
// With -backend=duraturo the same agent runs as a duraturo workflow on
// Postgres, the production backend: the ledger and the queue are tables in
// your database, and any number of worker processes can execute runs.
//
//	DATABASE_URL=postgres://localhost:5432/saige go run ./examples/agent/durable/ -backend=duraturo
//
// duraturo never creates tables. When the schema is missing, the example
// prints the suggested DDL for you to apply.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/duraturo/adapters/postgres/pgledger"
	"github.com/urmzd/duraturo/adapters/postgres/pgqueue"

	agentsdk "github.com/urmzd/saige/agent"
	durableduraturo "github.com/urmzd/saige/agent/durable/duraturo"
	"github.com/urmzd/saige/agent/durable/local"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// newAgent builds a fresh agent, tree and budget. Both engines call it again
// for every replay, so concurrent and recovered runs never share state.
func newAgent() *agentsdk.Agent {
	return agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "researcher",
		SystemPrompt: "You research questions and summarize concisely.",
		Provider:     ollama.NewAdapter(ollama.NewClient("http://localhost:11434", "llama3.2", "")),
	})
}

func main() {
	backend := flag.String("backend", "local", "durable backend: local or duraturo")
	flag.Parse()

	ctx := context.Background()
	input := []types.Message{types.NewUserMessage("Summarize the benefits of durable workflows.")}

	var (
		final *types.AssistantMessage
		err   error
	)
	switch *backend {
	case "local":
		final, err = runLocal(ctx, input)
	case "duraturo":
		final, err = runDuraturo(ctx, input)
	default:
		log.Fatalf("unknown backend %q: use local or duraturo", *backend)
	}
	if errors.Is(err, types.ErrSuspended) {
		log.Fatal("run is waiting for an approval; decide it and run again")
	}
	if err != nil {
		log.Fatalf("run: %v", err)
	}
	if final != nil {
		for _, c := range final.Content {
			if t, ok := c.(types.TextContent); ok {
				log.Printf("final: %s", t.Text)
			}
		}
	}
}

// runLocal resumes or starts the run in a private directory. The revision
// names the agent configuration; change it when the provider, tools or
// policies change.
func runLocal(ctx context.Context, input []types.Message) (*types.AssistantMessage, error) {
	engine := local.New(filepath.Join(os.TempDir(), "saige-durable-example"))
	return engine.Run(ctx, "demo-turn-1", "researcher-v1", newAgent, input)
}

// runDuraturo runs the agent on Postgres tables through duraturo. The worker
// runs in this process here; a deployment can run it in separate processes
// that register the same workflow.
func runDuraturo(ctx context.Context, input []types.Message) (*types.AssistantMessage, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://localhost:5432/saige?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	lgr, err := pgledger.New(pool, pgledger.DefaultMapping())
	if err != nil {
		return nil, err
	}
	q, err := pgqueue.New(pool, pgqueue.DefaultMapping())
	if err != nil {
		return nil, err
	}
	if err := errors.Join(lgr.Validate(ctx), q.Validate(ctx)); err != nil {
		log.Printf("apply this schema, then run again:\n%s\n%s",
			pgledger.RecommendedDDL(pgledger.DefaultMapping()), pgqueue.RecommendedDDL(pgqueue.DefaultMapping()))
		return nil, err
	}

	engine := durableduraturo.New(lgr, q)
	wf := engine.Register("researcher.v1", func(string) *agentsdk.Agent { return newAgent() })

	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.Worker().Run(workerCtx)
	}()
	defer func() {
		stop()
		<-done
	}()

	return engine.Run(ctx, wf, "demo-turn-1", input)
}
