package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/tools"
)

// TestHarnessToolsLive has a real model read a file and run a script to
// answer a question. Approvals are answered with a session grant. It runs
// only with SAIGE_LIVE=1 and an ANTHROPIC_API_KEY.
func TestHarnessToolsLive(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("SAIGE_LIVE") != "1" || key == "" {
		t.Skip("set SAIGE_LIVE=1 and ANTHROPIC_API_KEY to call the provider")
	}
	root := t.TempDir()
	var csv strings.Builder
	csv.WriteString("order,amount\n")
	total := 0
	for i := 1; i <= 40; i++ {
		amount := (i*7919)%997 + 13
		total += amount
		fmt.Fprintf(&csv, "A%03d,%d\n", i, amount)
	}
	if err := os.WriteFile(filepath.Join(root, "orders.csv"), []byte(csv.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	model := os.Getenv("SAIGE_LIVE_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-5-5"
	}
	a := must.Get(agent.New(agent.Config{
		Name:         "harness-live",
		SystemPrompt: "You are a careful analyst. Use your tools; never guess numbers.",
		Provider:     must.Get(anthropic.New(anthropic.Config{APIKey: key, Model: types.ModelID(model)})),
		MaxIter:      8,
	},
		agent.WithHarnessTools(tools.HarnessOptions{Root: root, Groups: []tools.Group{tools.GroupRead, tools.GroupExec}}),
		agent.WithApprovalPolicy(agent.ApprovalPolicy{}),
	))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("First look at orders.csv with read_file. Then compute the sum of the amount column by running a python " +
		"script with execute_code that reads the file. Reply with only the total."))})
	used := map[string]int{}
	asked := 0
	var answer strings.Builder
	for d := range stream.Deltas() {
		switch d := d.(type) {
		case types.MarkerDelta:
			asked++
			res := agent.Resolution{Approved: true, Approver: "test", Grant: &types.GrantRequest{Scope: types.GrantSession}}
			if err := stream.ResolveMarkerErr(d.ToolCallID, res); err != nil {
				t.Errorf("resolve: %v", err)
			}
		case types.ToolExecEndDelta:
			used[d.Name]++
			t.Logf("%s -> %.200q", d.Name, d.Result+d.Error)
		case types.PartDelta:
			answer.WriteString(d.Text)
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("answer %q, tools %v, approvals asked %d", answer.String(), used, asked)
	if used[tools.ReadFileName] == 0 || used[tools.ExecuteCodeName] == 0 {
		t.Errorf("tools used = %v, want read_file and execute_code", used)
	}
	if asked == 0 {
		t.Error("execute_code must ask for approval")
	}
	if !strings.Contains(strings.ReplaceAll(answer.String(), ",", ""), fmt.Sprint(total)) {
		t.Errorf("answer %q does not contain the total %d", answer.String(), total)
	}
}
