package agent_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/anthropic"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// recordingProvider keeps the size of every request it forwards.
type recordingProvider struct {
	inner types.Provider
	mu    sync.Mutex
	sizes []int
}

func (p *recordingProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	msgs, tools := req.Messages, req.Tools
	p.mu.Lock()
	p.sizes = append(p.sizes, types.EstimateTokens(msgs))
	p.mu.Unlock()
	return p.inner.Stream(ctx, types.Request{Messages: msgs, Tools: tools})
}

// TestSubAgentReadsDocumentByReferenceLive gives a bounded child a large
// document by reference and asks one question about it. It runs only with
// SAIGE_LIVE=1 and an OPENAI_API_KEY or ANTHROPIC_API_KEY.
func TestSubAgentReadsDocumentByReferenceLive(t *testing.T) {
	if os.Getenv("SAIGE_LIVE") != "1" {
		t.Skip("set SAIGE_LIVE=1 and a provider key to call the provider")
	}
	var inner types.Provider
	switch {
	case os.Getenv("OPENAI_API_KEY") != "":
		inner = must.Get(openai.New(openai.Config{APIKey: os.Getenv("OPENAI_API_KEY"), Model: "gpt-6-luna"}))
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		inner = must.Get(anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: "claude-haiku-5-5"}))
	default:
		t.Skip("no OPENAI_API_KEY or ANTHROPIC_API_KEY")
	}
	var doc strings.Builder
	doc.WriteString("Warehouse inventory log.\n")
	for i := range 1500 {
		fmt.Fprintf(&doc, "Bin %04d holds %d crates of assorted spare parts.\n", i, (i*7)%50)
	}
	doc.WriteString("Bin 9999 holds the backup generator; its serial number is QX-7731.\n")
	for i := 1500; i < 3000; i++ {
		fmt.Fprintf(&doc, "Bin %04d holds %d crates of assorted spare parts.\n", i, (i*7)%50)
	}
	task := "Using the document below, what is the serial number of the backup generator? Reply with the serial number only.\n\n" + doc.String()

	provider := &recordingProvider{inner: inner}
	parent := must.Get(agent.New(agent.Config{Name: "lead", Provider: inner}, agent.WithSubAgents(agent.SubAgentDef{
		Name:         "reader",
		Description:  "Answers questions about documents.",
		SystemPrompt: "You answer questions about documents you are given by reference. Use search_artifact to find the passage you need.",
		Provider:     provider,
		MaxIter:      4,
	})))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := parent.InvokeSubAgent(ctx, "reader", task)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	r, err := s.SubAgentResult()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "QX-7731") {
		t.Fatalf("answer = %q", r.Output)
	}
	if r.Iterations > r.MaxIter+1 {
		t.Fatalf("iterations = %d over the budget of %d", r.Iterations, r.MaxIter)
	}
	docTokens := types.EstimateTokens([]types.Message{types.UserMsg(types.Text(task))})
	for i, n := range provider.sizes {
		if n > docTokens/4 {
			t.Fatalf("request %d sent about %d tokens; the document is %d", i, n, docTokens)
		}
	}
	t.Logf("answer %q in %d iterations (forced %v); request tokens %v vs document %d", r.Output, r.Iterations, r.Forced, provider.sizes, docTokens)
}
