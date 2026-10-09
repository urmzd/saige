package agent

import (
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Transcript summarizes one drained EventStream. It covers this agent's own
// turns: deltas a sub-agent forwards inside ToolExecDelta are passed to the
// callback but not counted here.
type Transcript struct {
	// Text is the text of the last model turn, which is the answer of a run
	// that finished normally.
	Text string
	// AllText joins the text of every turn in order.
	AllText string
	// ToolCalls lists every tool call the model requested, in order.
	ToolCalls []types.ToolUseContent
	// ToolErrors counts tool executions that ended with an error.
	ToolErrors int
	// Turns counts model turns. The loop reports usage once per turn.
	Turns int
	// TTFT is the time from the start of collection to the first text,
	// thinking, or tool call delta. It is zero when none arrived.
	TTFT time.Duration
	// Total is the time from the start of collection to the end of the run.
	Total time.Duration
	// Usage sums the token counts and latency of every turn. Response
	// metadata is taken from the last turn that reported it.
	Usage types.UsageDelta
}

// Collect drains s, calling onDelta (when non-nil) for every delta in order,
// and returns the run's transcript with the error Wait reports. Start it
// right after Invoke so TTFT and Total measure the run.
func Collect(s *EventStream, onDelta func(types.Delta)) (Transcript, error) {
	start := time.Now()
	var (
		t   Transcript
		agg = NewDefaultAggregator()
		all strings.Builder
	)
	endTurn := func() {
		msg, _ := agg.Flush()
		agg.Reset()
		am, ok := msg.(types.AssistantMessage)
		if !ok {
			return
		}
		var text strings.Builder
		for _, block := range am.Content {
			switch b := block.(type) {
			case types.TextContent:
				text.WriteString(b.Text)
			case types.ToolUseContent:
				t.ToolCalls = append(t.ToolCalls, b)
			}
		}
		t.Text = text.String()
		all.WriteString(t.Text)
	}
	for d := range s.Deltas() {
		if onDelta != nil {
			onDelta(d)
		}
		switch v := d.(type) {
		case types.TextContentDelta, types.ThinkingContentDelta, types.ToolCallStartDelta:
			if t.TTFT == 0 {
				t.TTFT = time.Since(start)
			}
			agg.Push(d)
		case types.UsageDelta:
			t.Turns++
			addUsage(&t.Usage, v)
			endTurn()
		case types.ToolExecEndDelta:
			if v.Error != "" {
				t.ToolErrors++
			}
		case types.ToolExecDelta, types.PartialJSONDelta:
			// Sub-agent and derived deltas are not this agent's turn.
		default:
			agg.Push(d)
		}
	}
	// A run that ended without reporting usage for its last turn, such as a
	// cancelled one, still has that turn's committed text.
	endTurn()
	err := s.Wait()
	t.AllText = all.String()
	t.Total = time.Since(start)
	return t, err
}

// CollectText drains s and returns the text of its last model turn, with the
// error Wait reports.
func CollectText(s *EventStream) (string, error) {
	t, err := Collect(s, nil)
	return t.Text, err
}

// addUsage adds one turn's usage to a running total.
func addUsage(total *types.UsageDelta, u types.UsageDelta) {
	total.PromptTokens += u.PromptTokens
	total.CachedPromptTokens += u.CachedPromptTokens
	total.CacheWriteTokens += u.CacheWriteTokens
	total.CompletionTokens += u.CompletionTokens
	total.TotalTokens += u.TotalTokens
	total.Latency += u.Latency
	if u.ResponseModel != "" {
		total.ResponseModel = u.ResponseModel
	}
	if u.ResponseID != "" {
		total.ResponseID = u.ResponseID
	}
	if len(u.FinishReasons) > 0 {
		total.FinishReasons = u.FinishReasons
	}
}
