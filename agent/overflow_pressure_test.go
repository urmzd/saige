package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// countingTokenizer sizes a request at one token per character.
type countingTokenizer struct{}

func (countingTokenizer) CountTokens(_ context.Context, messages []types.Message) (int, error) {
	return len(types.MessagesToText(messages)), nil
}

// summarizingProvider answers summary requests with a summary and any other
// request with "done", recording the size of each non-summary request.
type summarizingProvider struct {
	mu        sync.Mutex
	summaries int
	turns     []int // size of each non-summary request
}

func (p *summarizingProvider) ChatStream(_ context.Context, messages []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	text := "done"
	if strings.Contains(types.MessagesToText(messages[:1]), "Summarize the following conversation") {
		p.summaries++
		text = fmt.Sprintf("summary %d", p.summaries)
	} else {
		n, _ := countingTokenizer{}.CountTokens(context.Background(), messages)
		p.turns = append(p.turns, n)
	}
	p.mu.Unlock()
	ch := make(chan types.Delta, 8)
	for _, d := range agenttest.TextResponse(text) {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// A turn still over MaxInputTokens after one compaction is compacted again
// until it fits, rather than sent over the limit.
func TestPressureCompactionLoopsUntilUnderLimit(t *testing.T) {
	provider := &summarizingProvider{}
	const limit = 1200
	a := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		MaxIter:      1,
		Tokenizer:    countingTokenizer{},
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummarize, MaxInputTokens: limit},
	})
	var input []types.Message
	for i := range 6 {
		long := strings.Repeat(fmt.Sprint(i), 200)
		input = append(input, types.NewUserMessage(long),
			types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: long}}})
	}
	input = append(input, types.NewUserMessage("final question"))
	stream := a.Invoke(context.Background(), input)
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if provider.summaries < 2 {
		t.Fatalf("summaries = %d, want at least 2", provider.summaries)
	}
	if len(provider.turns) != 1 || provider.turns[0] > limit {
		t.Fatalf("turn sizes = %v, want one turn within %d tokens (summaries %d)", provider.turns, limit, provider.summaries)
	}
}

// A compaction that does not shrink the input ends the loop: the turn is
// sent instead of compacted forever.
func TestPressureCompactionStopsWithoutProgress(t *testing.T) {
	provider := &summarizingProvider{}
	a := NewAgent(AgentConfig{
		Provider:     provider,
		SystemPrompt: "sys",
		MaxIter:      1,
		Tokenizer:    countingTokenizer{},
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummarize, MaxInputTokens: 1},
	})
	stream := a.Invoke(context.Background(), []types.Message{
		types.NewUserMessage("a"),
		types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "b"}}},
		types.NewUserMessage("c"),
	})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if provider.summaries > maxPressureCompactions || len(provider.turns) != 1 {
		t.Fatalf("summaries = %d, turns = %d", provider.summaries, len(provider.turns))
	}
}
