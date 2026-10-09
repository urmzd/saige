package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// One Agent serves concurrent runs on distinct branches of one tree, and the
// same tool instance runs concurrently across them. Run it with -race.
func TestConcurrentInvokeOnDistinctBranches(t *testing.T) {
	const runs = 16
	responses := make([][]types.Delta, 0, 2*runs)
	for i := 0; i < runs; i++ {
		responses = append(responses, agenttest.ToolCallResponse(fmt.Sprintf("c%d", i), "count", nil))
	}
	for i := 0; i < runs; i++ {
		responses = append(responses, agenttest.TextResponse("done"))
	}
	provider := &agenttest.ScriptedProvider{Responses: responses}
	var mu sync.Mutex
	calls := 0
	count := &types.ToolFunc{Def: types.ToolDef{Name: "count"}, Fn: func(context.Context, map[string]any) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "ok", nil
	}}
	a := NewAgent(AgentConfig{Provider: provider, Tools: types.NewToolRegistry(count)})
	tr := a.Tree()
	root := tr.Root().ID

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	branches := make([]types.BranchID, runs)
	for i := range branches {
		b, _, err := tr.Branch(ctx, root, fmt.Sprintf("b%d", i), types.NewUserMessage(fmt.Sprintf("task %d", i)))
		if err != nil {
			t.Fatal(err)
		}
		branches[i] = b
	}

	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for _, b := range branches {
		wg.Add(1)
		go func(b types.BranchID) {
			defer wg.Done()
			_, err := CollectText(a.Invoke(ctx, nil, b))
			errs <- err
		}(b)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != runs {
		t.Fatalf("tool calls = %d, want %d", calls, runs)
	}
	for _, b := range branches {
		tip, err := tr.Tip(b)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := tip.Message.(types.AssistantMessage); !ok {
			t.Fatalf("branch %s ends with %T", b, tip.Message)
		}
	}
}
