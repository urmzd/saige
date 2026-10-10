package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
)

// batchModel answers each call from its last user text, so concurrent calls
// in a local batch are checked against their own inputs. A schema call
// answers JSON.
type batchModel struct {
	mu      sync.Mutex
	systems []string
	tools   int
}

func (m *batchModel) Name() string  { return "batch-model" }
func (m *batchModel) Model() string { return "bm-1" }

func (m *batchModel) reply(msgs []types.Message, tools []types.ToolDef, schema bool) (<-chan types.Delta, error) {
	m.mu.Lock()
	m.tools += len(tools)
	for _, msg := range msgs {
		if s, ok := msg.(types.SystemMessage); ok {
			m.systems = append(m.systems, s.Content[0].(types.TextContent).Text)
		}
	}
	m.mu.Unlock()
	text := batchUserText(msgs)
	answer := "re: " + text
	if schema {
		priority := "low"
		if strings.Contains(text, "down") {
			priority = "high"
		}
		if strings.Contains(text, "garbage") {
			priority = "unknown\""
		}
		answer = `{"priority":"` + priority + `"}`
	}
	out := make(chan types.Delta, 4)
	out <- types.TextStartDelta{}
	out <- types.TextContentDelta{Content: answer}
	out <- types.TextEndDelta{}
	close(out)
	return out, nil
}

func (m *batchModel) ChatStream(_ context.Context, msgs []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return m.reply(msgs, tools, false)
}

func (m *batchModel) ChatStreamWithSchema(_ context.Context, msgs []types.Message, tools []types.ToolDef, _ *types.ParameterSchema) (<-chan types.Delta, error) {
	return m.reply(msgs, tools, true)
}

func TestRunBatch(t *testing.T) {
	m := &batchModel{}
	tools := types.NewToolRegistry()
	a := NewAgent(AgentConfig{Name: "bulk", SystemPrompt: "Be brief.", Provider: m, Tools: tools})
	inputs := []BatchInput{
		{ID: "one", Messages: []types.Message{types.NewUserMessage("first")}},
		{Messages: []types.Message{types.NewUserMessage("second")}},
	}
	got, err := a.RunBatch(context.Background(), inputs, BatchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].CustomID != "one" || got[0].Text() != "re: first" || got[1].CustomID != "1" || got[1].Text() != "re: second" {
		t.Fatalf("results = %+v", got)
	}
	if len(m.systems) != 2 || m.systems[0] != "Be brief." {
		t.Fatalf("system prompts = %v", m.systems)
	}
	if m.tools != 0 {
		t.Fatal("a batch offered tools")
	}
}

// TestRunBatchUsesProviderBatchAPI checks that RunBatch finds the batch API
// behind the agent's provider and charges the agent's budget at batch rates.
func TestRunBatchUsesProviderBatchAPI(t *testing.T) {
	bp := &recordingBatch{Local: batch.NewLocal(&batchModel{}, 2)}
	b := types.NewBudget(types.BudgetPolicy{})
	a := NewAgent(AgentConfig{Name: "bulk", Provider: bp, Budget: b})
	if _, err := a.RunBatch(context.Background(), []BatchInput{{Messages: []types.Message{types.NewUserMessage("x")}}}, BatchConfig{}); err != nil {
		t.Fatal(err)
	}
	if bp.submits != 1 {
		t.Fatalf("submits = %d, want the provider's own batch API", bp.submits)
	}
}

// recordingBatch is a provider with a batch API of its own.
type recordingBatch struct {
	*batch.Local
	submits int
}

func (r *recordingBatch) ChatStream(ctx context.Context, msgs []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return nil, errors.New("streaming not expected")
}

func (r *recordingBatch) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	r.submits++
	return r.Local.Submit(ctx, reqs, opts)
}

func TestAIFuncBatch(t *testing.T) {
	m := &batchModel{}
	f, err := AIFunc[ticket, triage]("triage", "Triage a ticket", AIConfig{
		Prompt: "Triage: {{.Body}}", System: "You triage.", Provider: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := f.Batch(context.Background(), []ticket{{Body: "site down"}, {Body: "typo"}, {Body: "garbage"}}, BatchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Err != nil || items[0].Out.Priority != "high" || items[1].Out.Priority != "low" {
		t.Fatalf("items = %+v", items)
	}
	if !errors.Is(items[2].Err, ErrSchemaInvalid) {
		t.Fatalf("garbage answer err = %v, want ErrSchemaInvalid", items[2].Err)
	}
	if m.systems[0] != "You triage." {
		t.Fatalf("system = %v", m.systems)
	}
}

func batchUserText(msgs []types.Message) string {
	if u, ok := msgs[len(msgs)-1].(types.UserMessage); ok {
		return u.Content[0].(types.TextContent).Text
	}
	return ""
}
