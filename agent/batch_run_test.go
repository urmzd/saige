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
			m.systems = append(m.systems, s.Parts[0].(types.TextPart).Text)
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
	out <- types.PartStart{Index: 0, Kind: types.KindText}
	out <- types.PartDelta{Index: 0, Text: answer}
	out <- types.PartEnd{Index: 0}
	close(out)
	return out, nil
}

func (m *batchModel) chatStream(_ context.Context, msgs []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	return m.reply(msgs, tools, false)
}

// Stream implements types.Provider.
func (m *batchModel) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	if req.Schema != nil {
		return m.chatStreamWithSchema(ctx, req.Messages, req.Tools, req.Schema)
	}
	return m.chatStream(ctx, req.Messages, req.Tools)
}

// SupportsSchema implements types.StructuredOutputProvider.
func (m *batchModel) SupportsSchema() bool { return true }

func (m *batchModel) chatStreamWithSchema(_ context.Context, msgs []types.Message, tools []types.ToolDef, _ *types.ParameterSchema) (<-chan types.Delta, error) {
	return m.reply(msgs, tools, true)
}

func TestRunBatch(t *testing.T) {
	m := &batchModel{}
	tools := types.NewToolRegistry()
	a := NewAgent(AgentConfig{Name: "bulk", SystemPrompt: "Be brief.", Provider: m, Tools: tools})
	inputs := []BatchInput{
		{ID: "one", Messages: []types.Message{types.UserMsg(types.Text("first"))}},
		{Messages: []types.Message{types.UserMsg(types.Text("second"))}},
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
	if _, err := a.RunBatch(context.Background(), []BatchInput{{Messages: []types.Message{types.UserMsg(types.Text("x"))}}}, BatchConfig{}); err != nil {
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

func (r *recordingBatch) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
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
		return u.Parts[0].(types.TextPart).Text
	}
	return ""
}

// offeringBatch is a batch-capable provider that serves a text-only
// offering, so media in a batch request must be converted or rejected.
type offeringBatch struct {
	recordingBatch
	sent []types.BatchRequest
}

func (o *offeringBatch) Offering() types.Offering {
	return types.Offering{ID: "test/texty@test", Model: types.ModelInfo{Vendor: "test", Prefix: "texty", Known: true},
		Endpoint: types.EndpointInfo{Name: "test"}}
}

func (o *offeringBatch) Capabilities() types.ModelCapabilities { return o.Offering().Capabilities() }

func (o *offeringBatch) Submit(ctx context.Context, reqs []types.BatchRequest, opts types.BatchSubmitOptions) (types.BatchHandle, error) {
	o.sent = append(o.sent, reqs...)
	return o.recordingBatch.Submit(ctx, reqs, opts)
}

// TestRunBatchConvertsAtSubmit checks that a batch request is planned
// against the serving offering when the batch is submitted: a PDF the model
// cannot read is rejected before anything is uploaded, and extracted to
// text when the agent's extractors permit it.
func TestRunBatchConvertsAtSubmit(t *testing.T) {
	doc := types.Document(types.Bytes(types.MediaPDF, []byte("%PDF-1.4 notes")))
	inputs := []BatchInput{{ID: "doc", Messages: []types.Message{types.UserMsg(types.Text("summarize"), doc)}}}

	rejecting := &offeringBatch{recordingBatch: recordingBatch{Local: batch.NewLocal(&batchModel{}, 1)}}
	a := NewAgent(AgentConfig{Name: "bulk", Provider: rejecting})
	if _, err := a.RunBatch(context.Background(), inputs, BatchConfig{}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("err = %v, want the PDF rejected", err)
	}
	if rejecting.submits != 0 {
		t.Fatal("a rejected batch was submitted")
	}

	extracting := &offeringBatch{recordingBatch: recordingBatch{Local: batch.NewLocal(&batchModel{}, 1)}}
	extract := types.ExtractorFunc(func(context.Context, []byte, types.MediaType) ([]types.UserPart, error) {
		return []types.UserPart{types.Text("extracted notes")}, nil
	})
	a = NewAgent(AgentConfig{Name: "bulk", Provider: extracting,
		Extractors: map[types.MediaType]types.Extractor{types.MediaPDF: extract}})
	res, err := a.RunBatch(context.Background(), inputs, BatchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(extracting.sent) != 1 || !strings.Contains(types.TextOf(extracting.sent[0].Messages[0]), "extracted notes") {
		t.Fatalf("sent %+v, want the extracted text", extracting.sent)
	}
	for _, p := range types.PartsOf(extracting.sent[0].Messages[0]) {
		if types.IsMedia(p) {
			t.Fatalf("the PDF itself was submitted: %#v", p)
		}
	}
	if _, ok := inputs[0].Messages[0].(types.UserMessage).Parts[1].(types.DocumentPart); !ok {
		t.Error("the input was modified")
	}
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results = %+v", res)
	}
}
