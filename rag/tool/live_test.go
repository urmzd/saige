package tool_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	agenteval "github.com/urmzd/saige/agent/eval"
	"github.com/urmzd/saige/agent/provider"
	agenttypes "github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/extractor"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/tool"
	"github.com/urmzd/saige/rag/types"
)

// TestLiveRAGQuestionWithCitations asks gpt-6-luna a question it can only
// answer from the knowledge base. The run must call rag_search, record a
// retrieval citation anchored to the handbook chunk, and answer from it. It
// runs only with SAIGE_LIVE=1 and an OPENAI_API_KEY.
func TestLiveRAGQuestionWithCitations(t *testing.T) {
	if os.Getenv("SAIGE_LIVE") != "1" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set SAIGE_LIVE=1 and OPENAI_API_KEY to call the provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pipe, err := rag.NewPipeline(rag.WithStore(memstore.New()), rag.WithContentExtractor(extractor.NewAuto()), rag.WithBM25(nil))
	if err != nil {
		t.Fatal(err)
	}
	for uri, text := range map[string]string{
		"file://handbook.md": "Handbook\n\nZorblat refunds are issued within 17 business days of the return being received.",
		"file://menu.md":     "Menu\n\nThe cafeteria serves soup on Tuesdays.",
	} {
		if _, err := pipe.Ingest(ctx, &types.RawDocument{SourceURI: uri, MIMEType: "text/markdown", Data: []byte(text)}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := provider.Build(ctx, provider.Config{Provider: provider.OpenAI, Model: "gpt-6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	budget := agenttypes.NewBudget(agenttypes.BudgetPolicy{Limit: agenttypes.USD(0.05), PerCallCost: agenttypes.USD(0.01), MaxRequests: 6})
	a := agent.NewAgent(agent.AgentConfig{
		Provider:     p,
		Budget:       budget,
		Tools:        agenttypes.NewToolRegistry(tool.NewTools(pipe, tool.ReadOnly())...),
		SystemPrompt: "Answer only from the knowledge base. Search it with rag_search, read hits with rag_lookup, and answer in one sentence.",
	})
	stream := a.Invoke(ctx, []agenttypes.Message{agenttypes.UserMsg(agenttypes.Text("How long do Zorblat refunds take?"))})
	run := agenteval.CollectAgentRun(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	t.Logf("answer: %q; tools: %d; citations: %d", run.Text, len(run.ToolCalls), len(run.Citations))
	if !strings.Contains(run.Text, "17") {
		t.Fatalf("answer = %q", run.Text)
	}
	var handbook *agenttypes.Citation
	for i, c := range run.Citations {
		if c.URI == "file://handbook.md" {
			handbook = &run.Citations[i]
		}
	}
	if handbook == nil || handbook.Ordinal == 0 || handbook.Kind != agenttypes.CitationRetrieval || handbook.Meta["variant_uuid"] == "" {
		t.Fatalf("no anchored handbook citation in %+v", run.Citations)
	}

	obs := topeval.Observation{ID: "rag"}
	if err := agenteval.AnnotateObservation(&obs, run); err != nil {
		t.Fatal(err)
	}
	sc, err := agenteval.CitesScorer("handbook").Score(ctx, obs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cites:handbook = %.0f (%s)", sc.Value, sc.Reason)
	if sc.Value == 0 {
		t.Fatalf("the answer does not cite the handbook by marker: %s", sc.Reason)
	}
	for _, r := range budget.Breakdown() {
		t.Logf("spend: %s %d in / %d out tokens, %s", r.Model, r.Usage.InputTokens, r.Usage.OutputTokens, r.Cost)
	}
	t.Logf("total spend %s over %d calls", budget.Spent(), budget.Usage().Requests)
}
