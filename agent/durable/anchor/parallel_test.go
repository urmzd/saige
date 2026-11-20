package anchor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/adapters/file/filequeue"
	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type noArgs struct{}

// threeTools is one model response calling three tools, then an answer.
func threeTools() [][]types.Delta {
	var calls []types.Delta
	for _, id := range []string{"c1", "c2", "c3"} {
		calls = append(calls, agenttest.ToolCallResponse(id, "t"+id[1:], map[string]any{})...)
	}
	return [][]types.Delta{calls, agenttest.TextResponse("all three are done")}
}

func onFiles(t *testing.T, dir string) *Engine {
	t.Helper()
	lgr, err := fileledger.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	q, err := filequeue.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(lgr, q)
}

// toolRecords counts the tool steps the run has recorded results for.
func toolRecords(t *testing.T, dir, runID string) int {
	t.Helper()
	lgr, _ := fileledger.New(dir)
	recs, err := lgr.RecordsWithPrefix(context.Background(), runID, stepPrefix+"tool-")
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range recs {
		if r.Kind == run.KindStep && r.Status == run.RecordOK && filepath.Base(r.Key) == r.Key {
			n++
		}
	}
	return n
}

// Three tool calls of one model response run at once and finish out of
// order (the third first). The process dies after two have finished, with
// the second still running. On replay the two finished tools return their
// recorded results and only the second runs again.
func TestParallelToolsReplayOnlyTheUnfinishedOne(t *testing.T) {
	if dir := os.Getenv("SAIGE_PARALLEL_CRASH"); dir != "" {
		e := onFiles(t, dir)
		var finished sync.WaitGroup
		finished.Add(2)
		third := make(chan struct{})
		tools := types.NewToolRegistry(
			agent.Func("t1", "first", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) {
				<-third // finishes after the third
				defer finished.Done()
				return "one", nil
			}, agent.Idempotent()),
			agent.Func("t2", "second", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) {
				finished.Wait()
				for toolRecords(t, dir, "parallel") < 2 {
					time.Sleep(5 * time.Millisecond)
				}
				os.Exit(17) // dies while the second tool runs
				return "", nil
			}, agent.Idempotent()),
			agent.Func("t3", "third", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) {
				defer close(third)
				defer finished.Done()
				return "three", nil
			}, agent.Idempotent()),
		)
		wf := e.Register("parallel.v1", func(string) *agent.Agent {
			return must.Get(agent.New(agent.Config{Name: "fan", Provider: &agenttest.ScriptedProvider{Responses: threeTools()}, Tools: tools}))
		})
		ctx := context.Background()
		if err := e.Start(ctx, wf, "parallel", []types.Message{types.UserMsg(types.Text("go"))}); err != nil {
			os.Exit(2)
		}
		_ = e.Worker().RunOne(ctx, "parallel")
		os.Exit(3)
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestParallelToolsReplayOnlyTheUnfinishedOne$")
	cmd.Env = append(os.Environ(), "SAIGE_PARALLEL_CRASH="+dir)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 17 {
		t.Fatalf("the crashing process ended with %v:\n%s", err, out)
	}
	if n := toolRecords(t, dir, "parallel"); n != 2 {
		t.Fatalf("%d tool results recorded before the crash, want 2", n)
	}

	e := onFiles(t, dir)
	var ran [3]atomic.Int32
	tool := func(i int, out string) types.Tool {
		return agent.Func("t"+string(rune('1'+i)), "tool", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) {
			ran[i].Add(1)
			return out, nil
		}, agent.Idempotent())
	}
	tools := types.NewToolRegistry(tool(0, "one"), tool(1, "two"), tool(2, "three"))
	wf := e.Register("parallel.v1", func(string) *agent.Agent {
		// The first model call is recorded; only the answer is asked for.
		return must.Get(agent.New(agent.Config{Name: "fan", Provider: &agenttest.ScriptedProvider{Responses: threeTools()[1:]}, Tools: tools}))
	})
	startWorker(t, e)
	final, err := e.Run(testContext(t), wf, "parallel", []types.Message{types.UserMsg(types.Text("go"))})
	if err != nil {
		t.Fatal(err)
	}
	if got := [3]int32{ran[0].Load(), ran[1].Load(), ran[2].Load()}; got != [3]int32{0, 1, 0} {
		t.Fatalf("tool executions on replay %v, want only the second ([0 1 0])", got)
	}
	if final == nil || len(final.Parts) == 0 {
		t.Fatalf("final = %+v", final)
	}
	if txt, ok := final.Parts[0].(types.TextPart); !ok || txt.Text != "all three are done" {
		t.Fatalf("final = %+v", final.Parts)
	}
}
