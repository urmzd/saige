package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/eval/harness"
)

// evalBatchWindow is how long the coalescer waits for more calls before it
// sends a batch. Every script runs at once under --batch, so the calls of a
// turn arrive together.
const evalBatchWindow = 3 * time.Second

// applyEvalBatch routes the eval client's calls through vendor batches:
// concurrent calls are gathered by a coalescer and sent as one batch on the
// provider's batch API, or run locally where the provider has none. Every
// script runs at once so each turn's calls share a batch. With a store
// directory the job records survive a restart, and re-running the same
// command resumes the batches instead of paying for them again.
func applyEvalBatch(client *harness.Client, runner *harness.Runner, scripts int, storeDir, suite string) error {
	if client == nil || client.Provider == nil {
		return errors.New("--batch needs a saige provider transport (--provider or the manifest subject provider)")
	}
	bp := agent.BatchProviderFor(client.Provider, runner.Concurrency)
	var store batch.Store = batch.NewMemoryStore()
	if storeDir != "" {
		fs, err := batch.NewFileStore(storeDir)
		if err != nil {
			return err
		}
		store = fs
	}
	jobs := batch.NewRunner(bp, store, batch.WithPollInterval(10*time.Second, time.Minute))
	client.Provider = batch.NewCoalescer(jobs, batch.WithWindow(evalBatchWindow), batch.WithJobPrefix("eval-"+suite))
	runner.Concurrency = max(scripts, 1)
	fmt.Fprintf(os.Stderr, "batch: %d scripts through %s batches (results may take minutes to hours)\n", scripts, client.ProviderName())
	return nil
}
