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
	adapter, err := ollama.New(ollama.Config{Host: "http://localhost:11434", Model: "llama3.2"})
	if err != nil {
		log.Fatal(err)
	}

	// The triage agent is the entry agent. WithHandoffs registers the group; the
	// triage agent automatically gains handoff_to_billing and handoff_to_tech
	// tools, and each specialist can hand back to triage.
	agent, err := agentsdk.New(agentsdk.Config{
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
	if err != nil {
		log.Fatal(err)
	}

	stream := agent.Invoke(context.Background(), []types.Message{
		types.UserMsg(types.Text("I was double-charged on my last invoice. Can you help?")),
	})

	for delta := range stream.Deltas() {
		switch d := delta.(type) {
		case types.PartDelta:
			fmt.Print(d.Text)
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
