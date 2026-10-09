// Package main builds tools from typed Go functions. agent.Func derives the
// schema from the input struct, decodes the model's arguments strictly, and
// hands the function the host's dependencies through a typed RunContext.
// agent.AIFunc is a typed function the model computes, usable on its own or
// as a tool. Both report a version derived from their content, which the
// agent records next to each result.
//
//	OPENAI_API_KEY=... go run ./examples/agent/func-tools/
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
)

// Inventory is the host dependency the tools share.
type Inventory struct{ stock map[string]int }

type StockIn struct {
	SKU string `json:"sku" description:"Product SKU, such as A-100"`
}

type StockOut struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type ReserveIn struct {
	SKU      string `json:"sku" description:"Product SKU"`
	Quantity int    `json:"quantity" description:"Units to reserve"`
}

type Review struct {
	Text string `json:"text"`
}

type Sentiment struct {
	Label string `json:"label" enum:"positive,neutral,negative"`
}

func main() {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		log.Fatal("set OPENAI_API_KEY")
	}
	llm := openai.NewAdapter(key, "gpt-6-luna")

	stock := agentsdk.Func("stock", "Units in stock for a SKU",
		func(rc agentsdk.RunContext[*Inventory], in StockIn) (StockOut, error) {
			return StockOut{SKU: in.SKU, Quantity: rc.Deps.stock[in.SKU]}, nil
		}, agentsdk.Capability(types.ToolCapabilityRead))

	reserve := agentsdk.Func("reserve", "Reserve units of a SKU",
		func(rc agentsdk.RunContext[*Inventory], in ReserveIn) (string, error) {
			if rc.Deps.stock[in.SKU] < in.Quantity {
				return "", fmt.Errorf("only %d units of %s left", rc.Deps.stock[in.SKU], in.SKU)
			}
			rc.Deps.stock[in.SKU] -= in.Quantity
			return fmt.Sprintf("reserved %d of %s (call %s)", in.Quantity, in.SKU, rc.Call.ID), nil
		}, agentsdk.Capability(types.ToolCapabilityWrite), agentsdk.Approval("Reserve stock"), agentsdk.Idempotent())

	sentiment, err := agentsdk.AIFunc[Review, Sentiment]("sentiment", "Classify the sentiment of a review",
		agentsdk.AIConfig{Prompt: "Classify the sentiment of this review:\n{{.Text}}", Provider: llm})
	if err != nil {
		log.Fatal(err)
	}

	// An AIFunc is a plain typed function too.
	s, err := sentiment.Call(context.Background(), Review{Text: "Arrived early and works great."})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("sentiment %s (version %s)\n", s.Label, sentiment.Version())

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "clerk",
		SystemPrompt: "You manage inventory. Use the tools.",
		Provider:     llm,
		Tools:        types.NewToolRegistry(stock, reserve, sentiment.Tool()),
	}, agentsdk.WithDeps(&Inventory{stock: map[string]int{"A-100": 7}}))

	stream := agent.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("How many A-100 are in stock? Reserve 2 of them."),
	})
	for d := range stream.Deltas() {
		switch d := d.(type) {
		case types.TextContentDelta:
			fmt.Print(d.Content)
		case types.MarkerDelta:
			fmt.Printf("[approving %s %v]\n", d.ToolName, d.Arguments)
			_ = stream.ResolveMarkerErr(d.ToolCallID, agentsdk.Resolution{Approved: true, Approver: "example"})
		case types.ToolExecEndDelta:
			fmt.Printf("[%s v%s] %s\n", d.Name, d.Version, strings.TrimSpace(d.Result+d.Error))
		}
	}
	fmt.Println()
	if err := stream.Wait(); err != nil {
		log.Fatal(err)
	}
}
