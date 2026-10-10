package types

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// summaryProvider answers every call with a fixed summary and records the
// conversations it was asked to summarize.
type summaryProvider struct {
	mu    sync.Mutex
	text  string
	calls []string
}

func (p *summaryProvider) Stream(_ context.Context, req Request) (<-chan Delta, error) {
	msgs := req.Messages
	p.mu.Lock()
	p.calls = append(p.calls, MessagesToText(msgs))
	p.mu.Unlock()
	ch := make(chan Delta, 1)
	ch <- PartDelta{Index: 0, Text: p.text}
	close(ch)
	return ch, nil
}

func call(id, name string) AssistantMessage {
	return AssistantMessage{Parts: []AssistantPart{ToolCallPart{ID: id, Name: name, Arguments: map[string]any{}}}}
}

func result(id, text string) SystemMessage {
	return ToolResults(ToolResultPart{CallID: id, Parts: []ToolOutputPart{Text(text)}})
}

// toolHistory is a system prompt, a task and turns of tool loops, including
// one call answered by results spread over two messages.
func toolHistory(turns int) []Message {
	msgs := []Message{SystemMsg(Text("sys")), UserMsg(Text("the task"))}
	for i := range turns {
		a, b := fmt.Sprintf("c%da", i), fmt.Sprintf("c%db", i)
		msgs = append(msgs,
			AssistantMessage{Parts: []AssistantPart{
				TextPart{Text: fmt.Sprintf("step %d", i)},
				ToolCallPart{ID: a, Name: "read", Arguments: map[string]any{}},
				ToolCallPart{ID: b, Name: "grep", Arguments: map[string]any{}},
			}},
			result(a, strings.Repeat("data ", 50)),
			result(b, strings.Repeat("more ", 50)),
		)
		if i%3 == 2 {
			msgs = append(msgs, UserMsg(Text(fmt.Sprintf("follow up %d", i))))
		}
	}
	return append(msgs, call("last", "read"), result("last", "tail"))
}

func strategyConfigs() map[string]CompactConfig {
	return map[string]CompactConfig{
		"keep_recent":           {Strategy: CompactKeepRecent, KeepTurns: 2},
		"summary":               {Strategy: CompactSummary, KeepTurns: 2},
		"relevant_plus_summary": {Strategy: CompactRelevantPlusSummary, KeepTurns: 2, SelectK: 2},
		"clear_tool_results":    {Strategy: CompactClearToolResults, KeepToolResults: 1},
		"sliding_window":        {Strategy: CompactSlidingWindow, WindowSize: 5},
		"summarize":             {Strategy: CompactSummarize, Threshold: 3, KeepLast: 4},
		"chain": {Strategy: CompactChain, Chain: []CompactConfig{
			{Strategy: CompactClearToolResults, KeepToolResults: 1},
			{Strategy: CompactRelevantPlusSummary, KeepTurns: 1, SelectK: 1},
			{Strategy: CompactKeepRecent, KeepTurns: 1},
		}},
	}
}

func TestStrategiesKeepToolPairs(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range strategyConfigs() {
		t.Run(name, func(t *testing.T) {
			s := cfg.ToStrategy()
			if s == nil {
				t.Fatal("no strategy")
			}
			for turns := 1; turns <= 12; turns++ {
				msgs := toolHistory(turns)
				res, err := s.CompactEntries(ctx, CompactRequest{
					Entries: NewCompactEntries(msgs), Query: "grep step", Force: true,
					Provider: &summaryProvider{text: "summary"},
				})
				if err != nil {
					t.Fatalf("turns %d: %v", turns, err)
				}
				out := EntryMessages(res.Entries)
				if err := ToolPairingError(out); err != nil {
					t.Fatalf("turns %d: %v\n%s", turns, err, MessagesToText(out))
				}
				if res.Entries[0].Index != 0 {
					t.Fatalf("turns %d: system prompt not first", turns)
				}
				if turns == 12 && !res.Changed() {
					t.Fatalf("a long history was not compacted")
				}
			}
		})
	}
}

// TestStrategiesKeepToolPairsRandom compacts random transcripts with every
// strategy and checks that no tool result loses its call.
func TestStrategiesKeepToolPairsRandom(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	for iter := range 200 {
		msgs := []Message{SystemMsg(Text("sys")), UserMsg(Text("task"))}
		for i := range 2 + rng.Intn(20) {
			switch rng.Intn(4) {
			case 0:
				msgs = append(msgs, UserMsg(Text(fmt.Sprintf("user %d", i))))
			case 1:
				msgs = append(msgs, AssistantMsg(Text(fmt.Sprintf("answer %d", i))))
			default:
				n := 1 + rng.Intn(3)
				am := AssistantMessage{}
				var ids []string
				for j := range n {
					id := fmt.Sprintf("r%d-%d-%d", iter, i, j)
					ids = append(ids, id)
					am.Parts = append(am.Parts, ToolCallPart{ID: id, Name: "t"})
				}
				msgs = append(msgs, am)
				for len(ids) > 0 {
					k := 1 + rng.Intn(len(ids))
					var rs []ToolResultPart
					for _, id := range ids[:k] {
						rs = append(rs, ToolResultPart{CallID: id, Parts: []ToolOutputPart{Text("out")}})
					}
					ids = ids[k:]
					msgs = append(msgs, ToolResults(rs...))
				}
			}
		}
		for name, cfg := range strategyConfigs() {
			cfg.KeepTurns = 1 + rng.Intn(3)
			res, err := cfg.ToStrategy().CompactEntries(ctx, CompactRequest{
				Entries: NewCompactEntries(msgs), Query: "answer user", Force: true,
				Provider: &summaryProvider{text: "s"},
			})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if err := ToolPairingError(EntryMessages(res.Entries)); err != nil {
				t.Fatalf("%s on iteration %d: %v", name, iter, err)
			}
		}
	}
}

func TestKeepRecent(t *testing.T) {
	msgs := toolHistory(5)
	res, err := NewKeepRecent(2).CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(msgs)})
	if err != nil {
		t.Fatal(err)
	}
	out := EntryMessages(res.Entries)
	if !reflect.DeepEqual(out[:2], msgs[:2]) {
		t.Fatal("system prompt and task must be kept")
	}
	if len(res.Dropped) == 0 || len(res.Summarized) != 0 {
		t.Fatalf("dropped %v, summarized %v", res.Dropped, res.Summarized)
	}
	if got := len(layoutOf(res.Entries).turns); got != 2 {
		t.Fatalf("turns kept = %d, want 2", got)
	}
}

func TestSummaryIsASystemMessageAndFoldsEarlierSummaries(t *testing.T) {
	p := &summaryProvider{text: "first summary"}
	s := &Summary{KeepTurns: 2}
	res, err := s.CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(toolHistory(6)), Force: true, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if !IsCompactionSummary(res.Entries[2].Message) || res.Entries[2].Index != -1 {
		t.Fatalf("entry 2 = %#v, want the summary", res.Entries[2])
	}
	// A second compaction folds the first summary into the new one.
	again := EntryMessages(res.Entries)
	again = append(again, call("x1", "read"), result("x1", "r"), call("x2", "read"), result("x2", "r"))
	p.text = "second summary"
	res2, err := s.CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(again), Force: true, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range res2.Entries {
		if IsCompactionSummary(e.Message) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("summaries = %d, want 1", n)
	}
	if !strings.Contains(p.calls[1], "first summary") {
		t.Fatal("the earlier summary was not summarized again")
	}
}

func TestSummaryNeedsATriggerUnlessForced(t *testing.T) {
	p := &summaryProvider{text: "s"}
	msgs := NewCompactEntries(toolHistory(6))
	res, err := (&Summary{}).CompactEntries(context.Background(), CompactRequest{Entries: msgs, Provider: p})
	if err != nil || res.Changed() || len(p.calls) != 0 {
		t.Fatalf("changed=%v calls=%d err=%v, want no summary without a threshold", res.Changed(), len(p.calls), err)
	}
	res, err = (&Summary{Threshold: 5}).CompactEntries(context.Background(), CompactRequest{Entries: msgs, Provider: p})
	if err != nil || !res.Changed() {
		t.Fatalf("changed=%v err=%v, want a summary over the threshold", res.Changed(), err)
	}
}

func TestRelevantPlusSummarySelectsTheRelevantOldTurn(t *testing.T) {
	msgs := []Message{SystemMsg(Text("sys")), UserMsg(Text("Help me plan the office move."))}
	topics := []string{"printer toner levels", "parking permits", "kitchen schedule", "desk layout", "network cabling"}
	for i, topic := range topics {
		msgs = append(msgs, UserMsg(Text("Note about "+topic+".")), AssistantMsg(Text("Noted the "+topic+".")))
		if i == 1 {
			msgs = append(msgs,
				call("vault", "lookup"),
				result("vault", "The server room door code is 4417, set by Imogen."),
				AssistantMsg(Text("Recorded the server room door code.")))
		}
	}
	msgs = append(msgs, UserMsg(Text("Recap the plan.")), AssistantMsg(Text("Plan recapped.")),
		UserMsg(Text("What is the server room door code Imogen set?")))

	p := &summaryProvider{text: "summary of the rest"}
	r := &RelevantPlusSummary{KeepTurns: 1, K: 1}
	res, err := r.CompactEntries(context.Background(), CompactRequest{
		Entries: NewCompactEntries(msgs), Query: "What is the server room door code Imogen set?", Force: true, Provider: p,
	})
	if err != nil {
		t.Fatal(err)
	}
	var selected []Message
	for _, e := range res.Entries {
		if e.Selected {
			selected = append(selected, e.Message)
		}
	}
	text := MessagesToText(selected)
	if !strings.Contains(text, "4417") {
		t.Fatalf("selected:\n%s\nwant the turn with the door code", text)
	}
	if err := ToolPairingError(EntryMessages(res.Entries)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.calls[0], "4417") {
		t.Fatal("the selected span was also summarized")
	}
	if len(res.Summarized) == 0 {
		t.Fatal("nothing summarized")
	}
}

func TestChainStopsAtTarget(t *testing.T) {
	msgs := toolHistory(8)
	p := &summaryProvider{text: "s"}
	chain := CompactConfig{Strategy: CompactChain, Chain: []CompactConfig{
		{Strategy: CompactClearToolResults, KeepToolResults: 1},
		{Strategy: CompactSummary, KeepTurns: 1},
	}}.ToStrategy()
	tok := EstimatingTokenizer{}
	full, _ := tok.CountTokens(context.Background(), msgs)

	// A target the first step reaches stops the chain there.
	cleared, err := (&ClearToolResultsCompactor{Keep: 1}).Compact(context.Background(), msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterClear, _ := tok.CountTokens(context.Background(), cleared)
	res, err := chain.CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(msgs), Target: afterClear, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Steps, ",") != "clear_tool_results" || len(p.calls) != 0 {
		t.Fatalf("steps = %v, summary calls = %d; want only clearing", res.Steps, len(p.calls))
	}
	// A smaller target runs the summary too.
	res, err = chain.CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(msgs), Target: afterClear / 4, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Steps, ",") != "clear_tool_results,summary" {
		t.Fatalf("steps = %v", res.Steps)
	}
	if full <= afterClear {
		t.Fatalf("clearing did not shrink the history: %d -> %d", full, afterClear)
	}
	if got := chain.Name(); got != "chain(clear_tool_results,summary)" {
		t.Fatalf("name = %q", got)
	}
}

func TestCompactorStrategyReportsDroppedAndSummarized(t *testing.T) {
	msgs := toolHistory(4)
	res, err := AsStrategy(NewSlidingWindowCompactor(4)).CompactEntries(context.Background(), CompactRequest{Entries: NewCompactEntries(msgs)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dropped) == 0 || len(res.Summarized) != 0 {
		t.Fatalf("sliding window: dropped %v summarized %v", res.Dropped, res.Summarized)
	}
	res, err = AsStrategy(NewSummarizeCompactor(3, 2)).CompactEntries(context.Background(), CompactRequest{
		Entries:  NewCompactEntries([]Message{SystemMsg(Text("s")), UserMsg(Text("a")), AssistantMsg(Text("b")), UserMsg(Text("c")), AssistantMsg(Text("d")), UserMsg(Text("e"))}),
		Provider: &summaryProvider{text: "sum"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Summarized) != 3 || len(res.Dropped) != 0 {
		t.Fatalf("summarize: summarized %v dropped %v", res.Summarized, res.Dropped)
	}
}
