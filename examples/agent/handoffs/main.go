// Package main demonstrates agent handoffs via a stable root. A triage agent
// shares one conversation tree with two specialists. When the triage agent calls
// a handoff_to_<name> tool, control transfers to that specialist, which continues
// the SAME conversation (full context preserved) rather than starting fresh like
// a delegated sub-agent. The consumer sees one continuous stream with a
// HandoffDelta marking each transfer.
package main

import (
	"context"
	"fmt"
	"log"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func main() {
	client := ollama.NewClient("http://localhost:11434", "llama3.2", "")
	adapter := ollama.NewAdapter(client)

	// The triage agent is the entry agent. WithHandoffs registers the group; the
	// triage agent automatically gains handoff_to_billing and handoff_to_tech
	// tools, and each specialist can hand back to triage.
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "triage",
		SystemPrompt: "You triage customer questions. Hand off billing questions to the billing agent and technical questions to the tech agent.",
		Provider:     adapter,
	}, agentsdk.WithHandoffs(
		agentsdk.HandoffDef{
			Name:         "billing",
			Description:  "Handles billing, invoices, refunds, and payment questions.",
			SystemPrompt: "You are a billing specialist. Answer billing questions precisely.",
			Provider:     adapter,
		},
		agentsdk.HandoffDef{
			Name:         "tech",
			Description:  "Handles technical support and troubleshooting.",
			SystemPrompt: "You are a technical support specialist.",
			Provider:     adapter,
		},
	))

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("I was double-charged on my last invoice. Can you help?"),
	})

	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.TextContentDelta:
			fmt.Print(d.Content)
		case types.HandoffDelta:
			fmt.Printf("\n[handoff %s → %s]\n", d.From, d.To)
		case types.ErrorDelta:
			log.Fatal(d.Error)
		case types.DoneDelta:
			fmt.Println()
		}
	}
	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
}
