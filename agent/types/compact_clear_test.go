package types

import (
	"context"
	"testing"
)

func TestClearToolResultsCompactor(t *testing.T) {
	call := func(id, name string) Message {
		return AssistantMessage{Parts: []AssistantPart{ToolCallPart{ID: id, Name: name}}}
	}
	result := func(id, text string) Message {
		return ToolResults(ToolResultPart{CallID: id, Parts: []ToolOutputPart{Text(text)}})
	}
	history := []Message{
		SystemMsg(Text("sys")), UserMsg(Text("go")),
		call("1", "search"), result("1", "first"),
		call("2", "memory"), result("2", "second"),
		call("3", "search"), result("3", "third"),
		call("4", "search"), result("4", "fourth"),
	}
	tests := []struct {
		name    string
		keep    int
		exclude []string
		cleared []string // call IDs whose results become stubs
	}{
		{name: "default keeps three", cleared: []string{"1"}},
		{name: "keep one", keep: 1, cleared: []string{"1", "2", "3"}},
		{name: "excluded tools are kept", keep: 1, exclude: []string{"memory"}, cleared: []string{"1", "3"}},
		{name: "nothing to clear", keep: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CompactConfig{Strategy: CompactClearToolResults, KeepToolResults: tt.keep, ExcludeTools: tt.exclude}.ToCompactor()
			out, err := c.Compact(context.Background(), history, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != len(history) {
				t.Fatalf("len = %d, want %d: calls and results must stay paired", len(out), len(history))
			}
			names := toolCallNames(history)
			want := map[string]bool{}
			for _, id := range tt.cleared {
				want[id] = true
			}
			for i, m := range out {
				for _, r := range toolResults(m) {
					stub := r.Text() == ClearedToolResultText(names[r.CallID], r.CallID)
					if stub != want[r.CallID] {
						t.Fatalf("result %s cleared = %v, want %v", r.CallID, stub, want[r.CallID])
					}
					if !stub && r.Text() != toolResults(history[i])[0].Text() {
						t.Fatalf("kept result %s changed", r.CallID)
					}
				}
			}
			// Clearing is idempotent: a second pass changes nothing.
			again, _ := c.Compact(context.Background(), out, nil)
			for i := range out {
				if MessagesToText(again[i:i+1]) != MessagesToText(out[i:i+1]) {
					t.Fatal("second pass changed the history")
				}
			}
			if len(tt.cleared) == 0 && &out[0] != &history[0] {
				t.Fatal("a no-op must return the input slice")
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		msgs []Message
		want int
	}{
		{name: "empty", want: 0},
		{name: "text", msgs: []Message{UserMsg(Text("abcdefgh"))}, want: estimateMessageOverhead + 2},
		{name: "file", msgs: []Message{UserMsg(Image(URL("file:///a.png")))}, want: estimateMessageOverhead + estimateFileTokens},
		{
			name: "tool call and result",
			msgs: []Message{
				AssistantMessage{Parts: []AssistantPart{ToolCallPart{ID: "1", Name: "ab", Arguments: map[string]any{"k": "v"}}}},
				ToolResults(ToolResultPart{CallID: "1", Parts: []ToolOutputPart{Text("abcd")}}),
			},
			// "ab" + `{"k":"v"}` + "abcd" = 15 chars, rounded up to 4 tokens.
			want: 2*estimateMessageOverhead + 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateTokens(tt.msgs); got != tt.want {
				t.Fatalf("EstimateTokens = %d, want %d", got, tt.want)
			}
			n, err := EstimatingTokenizer{}.CountTokens(context.Background(), tt.msgs)
			if err != nil || n != tt.want {
				t.Fatalf("CountTokens = %d, %v", n, err)
			}
		})
	}
}

func TestToolChoiceForced(t *testing.T) {
	for mode, want := range map[ToolChoiceMode]bool{
		"": false, ToolChoiceAuto: false, ToolChoiceNone: false, ToolChoiceRequired: true, ToolChoiceNamed: true,
	} {
		if got := (ToolChoice{Mode: mode}).Forced(); got != want {
			t.Errorf("%q.Forced() = %v, want %v", mode, got, want)
		}
	}
}
