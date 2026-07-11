package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// TestAgentToolCalling runs the full agent loop against a live model: the LLM
// must invoke the add tool and fold its result into the final answer.
func TestAgentToolCalling(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 10*time.Minute)

	tool, calls := addTool()
	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "calculator",
		SystemPrompt: "You are a calculator. You must use the add tool for any addition; never compute it yourself.",
		Provider:     ollama.NewAdapter(client),
		Tools:        types.NewToolRegistry(tool),
	})

	stream := agent.Invoke(ctx, []types.Message{
		types.NewUserMessage("What is 2 + 3? Use the add tool."),
	})
	text, toolCalls, err := drainStream(stream)
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}

	if *calls == 0 {
		t.Fatalf("add tool was never executed (LLM tool calls seen: %v)", toolCalls)
	}
	if !strings.Contains(text, "5") {
		t.Errorf("expected final answer to contain 5, got: %q", text)
	}
	t.Logf("tool executed %d time(s), answer: %q", *calls, strings.TrimSpace(text))
}

// TestAgentHandoff verifies control transfer between agents sharing one tree:
// the triage agent must call handoff_to_math and the math agent must answer.
func TestAgentHandoff(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 10*time.Minute)
	adapter := ollama.NewAdapter(client)

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name: "triage",
		SystemPrompt: "You are a triage agent. You must never answer math questions yourself. " +
			"For any question involving numbers or arithmetic, immediately call the handoff_to_math tool.",
		Provider: adapter,
	}, agentsdk.WithHandoffs(agentsdk.HandoffDef{
		Name:         "math",
		Description:  "Expert at arithmetic. Transfer every math question to this agent.",
		SystemPrompt: "You are a math expert. Answer arithmetic questions directly and concisely.",
		Provider:     adapter,
	}))

	stream := agent.Invoke(ctx, []types.Message{
		types.NewUserMessage("What is 2 + 3?"),
	})
	text, toolCalls, err := drainStream(stream)
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}

	handedOff := false
	for _, name := range toolCalls {
		if strings.HasPrefix(name, "handoff_to_") {
			handedOff = true
		}
	}
	if !handedOff {
		t.Fatalf("expected a handoff_to_* tool call, saw: %v (answer: %q)", toolCalls, text)
	}
	if !strings.Contains(text, "5") {
		t.Errorf("expected final answer to contain 5, got: %q", text)
	}
	t.Logf("handoff chain %v, answer: %q", toolCalls, strings.TrimSpace(text))
}

// TestAgentStructuredResponse verifies WithResponseSchema constrains the
// agent's final output to parseable JSON.
func TestAgentStructuredResponse(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 10*time.Minute)

	agent := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:         "extractor",
		SystemPrompt: "Extract the requested fields and respond in JSON.",
		Provider:     ollama.NewAdapter(client),
	}, agentsdk.WithResponseSchema(&types.ParameterSchema{
		Type:     "object",
		Required: []string{"city", "population_millions"},
		Properties: map[string]types.PropertyDef{
			"city":                {Type: "string", Description: "The city name"},
			"population_millions": {Type: "number", Description: "Approximate population in millions"},
		},
	}))

	stream := agent.Invoke(ctx, []types.Message{
		types.NewUserMessage("Tokyo has a metropolitan population of roughly 37 million people."),
	})
	text, _, err := drainStream(stream)
	if err != nil {
		t.Fatalf("agent run: %v", err)
	}

	var out struct {
		City               string  `json:"city"`
		PopulationMillions float64 `json:"population_millions"`
	}
	raw := strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("final output is not valid JSON: %v\nraw: %s", err, raw)
	}
	if !strings.Contains(strings.ToLower(out.City), "tokyo") {
		t.Errorf("expected city Tokyo, got %q", out.City)
	}
	if out.PopulationMillions <= 0 {
		t.Errorf("expected positive population, got %v", out.PopulationMillions)
	}
}
