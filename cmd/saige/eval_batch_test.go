package main

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval/harness"
)

type nopProvider struct{}

func (nopProvider) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	ch := make(chan types.Delta)
	close(ch)
	return ch, nil
}

func TestApplyEvalBatch(t *testing.T) {
	if err := applyEvalBatch(&harness.Client{}, &harness.Runner{}, 3, "", "s"); err == nil {
		t.Fatal("--batch accepted the OpenAI-compatible HTTP transport")
	}
	client := harness.NewProviderClient(nopProvider{})
	runner := &harness.Runner{Concurrency: 1}
	if err := applyEvalBatch(client, runner, 3, t.TempDir(), "suite"); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.Provider.(*batch.Coalescer); !ok {
		t.Fatalf("provider = %T, want a coalescer", client.Provider)
	}
	if runner.Concurrency != 3 {
		t.Fatalf("concurrency = %d, want every script at once", runner.Concurrency)
	}
	cmd := newEvalRunCmd(context.Background())
	for _, name := range []string{"batch", "batch-store"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("flag --%s missing", name)
		}
	}
}
