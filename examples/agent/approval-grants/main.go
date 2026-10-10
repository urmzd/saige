// Package main shows an approval policy that adapts to earlier decisions.
// The host approves the first write under /srv/app with an args grant, so
// later writes under that directory run without asking; a write elsewhere
// still asks. Reads never ask, and a destructive call always asks.
//
// The model is scripted so the run is deterministic and needs no API key:
//
//	go run ./examples/agent/approval-grants/
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

type WriteIn struct {
	Path string `json:"path" description:"File path"`
	Text string `json:"text" description:"Content"`
}

type PathIn struct {
	Path string `json:"path" description:"File path"`
}

func main() {
	write := agentsdk.Func("write_file", "Write a file",
		func(_ agentsdk.RunContext[agentsdk.NoDeps], in WriteIn) (string, error) {
			return "wrote " + in.Path, nil
		}, agentsdk.Capability(types.ToolCapabilityWrite))
	read := agentsdk.Func("read_file", "Read a file",
		func(_ agentsdk.RunContext[agentsdk.NoDeps], in PathIn) (string, error) {
			return "contents of " + in.Path, nil
		}, agentsdk.Capability(types.ToolCapabilityRead))
	remove := agentsdk.Func("delete_file", "Delete a file",
		func(_ agentsdk.RunContext[agentsdk.NoDeps], in PathIn) (string, error) {
			return "deleted " + in.Path, nil
		}, agentsdk.Capability(types.ToolCapabilityDestructive))

	model := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "write_file", map[string]any{"path": "/srv/app/a.txt", "text": "a"}),
		agenttest.ToolCallResponse("c2", "write_file", map[string]any{"path": "/srv/app/b.txt", "text": "b"}),
		agenttest.ToolCallResponse("c3", "read_file", map[string]any{"path": "/srv/app/a.txt"}),
		agenttest.ToolCallResponse("c4", "write_file", map[string]any{"path": "/etc/hosts", "text": "x"}),
		agenttest.ToolCallResponse("c5", "delete_file", map[string]any{"path": "/srv/app/a.txt"}),
		agenttest.TextResponse("Done."),
	}}

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:     "editor",
		Provider: model,
		Tools:    types.NewToolRegistry(write, read, remove),
	}, agentsdk.WithApprovalPolicy(agentsdk.ApprovalPolicy{RiskDefaults: true, DenyAfter: 3}))

	stream := agent.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("Set up the app files."))})
	for d := range stream.Deltas() {
		switch d := d.(type) {
		case types.MarkerDelta:
			res := agentsdk.Resolution{Approved: true, Approver: "user:ada"}
			if d.ToolCallID == "c1" {
				// Approve this write and every later write under /srv/app
				// for the next hour.
				res.Grant = &types.GrantRequest{
					Scope:     types.GrantArgs,
					Match:     []types.ArgMatch{{Field: "path", PathPrefix: "/srv/app"}},
					ExpiresAt: time.Now().Add(time.Hour),
				}
			}
			fmt.Printf("asked about %s %v: approved\n", d.ToolName, d.Arguments["path"])
			if err := stream.ResolveMarkerErr(d.ToolCallID, res); err != nil {
				log.Fatal(err)
			}
		case types.ToolExecEndDelta:
			fmt.Printf("  %s: %s%s\n", d.Name, d.Result, d.Error)
		}
	}
	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}

	msgs, err := agent.Tree().FlattenBranch(agent.Tree().Active())
	if err != nil {
		log.Fatal(err)
	}
	for _, g := range agentsdk.Grants(msgs) {
		fmt.Printf("grant %s: %s on %s, by %s\n", g.ID, g.Scope, g.Tool, g.GrantedBy)
	}
}
