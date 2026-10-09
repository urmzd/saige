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
	// Each run answers from its own conversation, not from a shared script
	// order: a run whose tool turn finishes first must not take another
	// run's tool call, so the result does not depend on scheduling.
	provider := branchScripted{}
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

// branchScripted calls the count tool on a conversation's first turn and
// answers once that call has a result, whatever order concurrent runs reach
// it in.
type branchScripted struct{}

func (branchScripted) ChatStream(_ context.Context, messages []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	deltas := agenttest.TextResponse("done")
	if !hasToolResult(messages) {
		deltas = agenttest.ToolCallResponse(fmt.Sprintf("c%d", len(messages)), "count", nil)
	}
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func hasToolResult(messages []types.Message) bool {
	for _, m := range messages {
		switch v := m.(type) {
		case types.SystemMessage:
			for _, c := range v.Content {
				if _, ok := c.(types.ToolResultContent); ok {
					return true
				}
			}
		case types.UserMessage:
			for _, c := range v.Content {
				if _, ok := c.(types.ToolResultContent); ok {
					return true
				}
			}
		}
	}
	return false
}
