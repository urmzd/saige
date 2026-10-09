package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

func TestRunAskResolvesMarkers(t *testing.T) {
	tests := []struct {
		name      string
		allow     bool
		json      bool
		wantCalls int32
	}{
		{name: "default deny", wantCalls: 0},
		{name: "deny in json mode", json: true, wantCalls: 0},
		{name: "allow", allow: true, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			tool := &types.ToolFunc{
				Def: types.ToolDef{Name: "rag_delete"},
				Fn: func(context.Context, map[string]any) (string, error) {
					calls.Add(1)
					return "deleted", nil
				},
			}
			provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "rag_delete", map[string]any{"uuid": "doc"}),
				agenttest.TextResponse("finished"),
			}}
			agent := agentsdk.NewAgent(agentsdk.AgentConfig{
				Provider: provider,
				Tools:    types.NewToolRegistry(types.WithMarkers(tool, types.Marker{Kind: "human_approval"})),
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var buf bytes.Buffer
			out := tui.ResolveOutputWriters(tt.json, tui.TemplateMinimal, &buf, &buf)

			if err := runAsk(ctx, agent, "delete doc", out, tt.allow); err != nil {
				t.Fatalf("runAsk: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("ask did not finish before the deadline")
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("tool ran %d times, want %d", got, tt.wantCalls)
			}
			if !strings.Contains(buf.String(), "finished") {
				t.Fatalf("missing final answer in output %q", buf.String())
			}
		})
	}
}

func TestRunReturnsErrorsInsteadOfExiting(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		code    int
		wantErr string
	}{
		{name: "rag search without query", args: []string{"rag", "search"}, code: 1},
		{name: "kg ingest without text", args: []string{"kg", "ingest", "--name", "x"}, code: 1},
		{name: "bad approve value", args: []string{"ask", "--approve", "maybe", "hi"}, code: 1, wantErr: "--approve"},
		{name: "unknown flag", args: []string{"ask", "--raw", "hi"}, code: 1, wantErr: "unknown flag"},
		{name: "version", args: []string{"version"}, code: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errW bytes.Buffer
			if code := run(context.Background(), tt.args, &errW); code != tt.code {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.code, errW.String())
			}
			if tt.wantErr != "" && !strings.Contains(errW.String(), tt.wantErr) {
				t.Fatalf("stderr %q does not mention %q", errW.String(), tt.wantErr)
			}
		})
	}
}
