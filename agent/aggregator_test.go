package agent

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestDefaultAggregatorToolCallPairing(t *testing.T) {
	argsA := map[string]any{"x": 1}
	argsB := map[string]any{"y": 2}
	useA := types.ToolUseContent{ID: "a", Name: "fa", Arguments: argsA}
	useB := types.ToolUseContent{ID: "b", Name: "fb", Arguments: argsB}
	tests := []struct {
		name   string
		deltas []types.Delta
		want   []types.AssistantContent
	}{
		{
			name: "sequential without IDs",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "fa"}, types.ToolCallEndDelta{Arguments: argsA},
				types.ToolCallStartDelta{ID: "b", Name: "fb"}, types.ToolCallEndDelta{Arguments: argsB},
			},
			want: []types.AssistantContent{useA, useB},
		},
		{
			name: "next call announced before previous ends, no IDs",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "fa"},
				types.ToolCallStartDelta{ID: "b", Name: "fb"}, types.ToolCallEndDelta{Arguments: argsA},
				types.ToolCallEndDelta{Arguments: argsB},
			},
			want: []types.AssistantContent{useA, useB},
		},
		{
			name: "interleaved with IDs, ends out of order",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "fa"},
				types.ToolCallStartDelta{ID: "b", Name: "fb"},
				types.ToolCallArgumentDelta{ID: "b", Content: `{"y":2}`},
				types.ToolCallArgumentDelta{ID: "a", Content: `{"x":1}`},
				types.ToolCallEndDelta{ID: "b", Arguments: argsB},
				types.ToolCallEndDelta{ID: "a", Arguments: argsA},
			},
			want: []types.AssistantContent{useB, useA},
		},
		{
			name: "end for unknown ID is ignored",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "fa"},
				types.ToolCallEndDelta{ID: "zzz", Arguments: argsB},
				types.ToolCallEndDelta{ID: "a", Arguments: argsA},
			},
			want: []types.AssistantContent{useA},
		},
		{
			name:   "end without start is ignored",
			deltas: []types.Delta{types.ToolCallEndDelta{Arguments: argsA}},
			want:   nil,
		},
		{
			name: "repeated start restarts the call",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "old"},
				types.ToolCallStartDelta{ID: "a", Name: "fa"},
				types.ToolCallEndDelta{ID: "a", Arguments: argsA},
				types.ToolCallEndDelta{Arguments: argsB},
			},
			want: []types.AssistantContent{useA},
		},
		{
			name: "text and tools keep order",
			deltas: []types.Delta{
				types.TextStartDelta{}, types.TextContentDelta{Content: "hi"}, types.TextEndDelta{},
				types.ToolCallStartDelta{ID: "a", Name: "fa"}, types.ToolCallEndDelta{ID: "a", Arguments: argsA},
			},
			want: []types.AssistantContent{types.TextContent{Text: "hi"}, useA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			msg := agg.Message()
			if tt.want == nil {
				if msg != nil {
					t.Fatalf("message = %#v, want nil", msg)
				}
				return
			}
			got := msg.(types.AssistantMessage).Content
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestDefaultAggregatorFlush(t *testing.T) {
	done := types.ToolUseContent{ID: "a", Name: "fa", Arguments: map[string]any{"x": 1}}
	tests := []struct {
		name          string
		deltas        []types.Delta
		want          []types.AssistantContent
		wantTruncated bool
	}{
		{
			name:   "complete turn",
			deltas: []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "ok"}, types.TextEndDelta{}},
			want:   []types.AssistantContent{types.TextContent{Text: "ok"}},
		},
		{
			name:          "open text is kept",
			deltas:        []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "par"}},
			want:          []types.AssistantContent{types.TextContent{Text: "par"}},
			wantTruncated: true,
		},
		{
			name: "open thinking is dropped",
			deltas: []types.Delta{
				types.TextStartDelta{}, types.TextContentDelta{Content: "a"}, types.TextEndDelta{},
				types.ThinkingStartDelta{}, types.ThinkingContentDelta{Content: "hmm"},
			},
			want:          []types.AssistantContent{types.TextContent{Text: "a"}},
			wantTruncated: true,
		},
		{
			name: "open tool call is dropped, finished one kept",
			deltas: []types.Delta{
				types.ToolCallStartDelta{ID: "a", Name: "fa"}, types.ToolCallEndDelta{ID: "a", Arguments: done.Arguments},
				types.ToolCallStartDelta{ID: "b", Name: "fb"}, types.ToolCallArgumentDelta{ID: "b", Content: `{"y":`},
			},
			want:          []types.AssistantContent{done},
			wantTruncated: true,
		},
		{
			name:          "only an open tool call",
			deltas:        []types.Delta{types.ToolCallStartDelta{ID: "b", Name: "fb"}},
			want:          nil,
			wantTruncated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			if agg.Truncated() != tt.wantTruncated {
				t.Errorf("Truncated() before flush = %v", agg.Truncated())
			}
			msg, truncated := agg.Flush()
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			var got []types.AssistantContent
			if msg != nil {
				got = msg.(types.AssistantMessage).Content
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
			if agg.Truncated() || agg.OpenToolCalls() != nil {
				t.Error("blocks still open after Flush")
			}
			again, truncatedAgain := agg.Flush()
			if truncatedAgain || !reflect.DeepEqual(again, msg) {
				t.Errorf("second Flush changed the result: %#v, %v", again, truncatedAgain)
			}
		})
	}
}

func TestDefaultAggregatorOpenToolCalls(t *testing.T) {
	agg := NewDefaultAggregator()
	agg.Push(types.ToolCallStartDelta{ID: "a", Name: "fa"})
	agg.Push(types.ToolCallStartDelta{ID: "b", Name: "fb"})
	if got := agg.OpenToolCalls(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("open = %v", got)
	}
	agg.Push(types.ToolCallEndDelta{ID: "a"})
	if got := agg.OpenToolCalls(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("open = %v", got)
	}
	agg.Reset()
	if agg.OpenToolCalls() != nil || agg.Message() != nil {
		t.Error("Reset left state behind")
	}
}

func TestDefaultAggregatorServerTools(t *testing.T) {
	search := types.ServerToolKind("web_search")
	tests := []struct {
		name   string
		deltas []types.Delta
		want   []types.AssistantContent
	}{
		{
			name: "result fills in its call",
			deltas: []types.Delta{
				types.ServerToolCallDelta{ID: "s1", Kind: search, Name: "web_search", Input: map[string]any{"q": "go"}},
				types.ServerToolResultDelta{ID: "s1", Kind: search, Text: "found"},
				types.TextStartDelta{}, types.TextContentDelta{Content: "answer"}, types.TextEndDelta{},
			},
			want: []types.AssistantContent{
				types.ServerToolContent{ID: "s1", Kind: search, Name: "web_search", Input: map[string]any{"q": "go"}, Text: "found"},
				types.TextContent{Text: "answer"},
			},
		},
		{
			name:   "result without a call is still recorded",
			deltas: []types.Delta{types.ServerToolResultDelta{ID: "s2", Kind: search, Text: "r"}},
			want:   []types.AssistantContent{types.ServerToolContent{ID: "s2", Kind: search, Text: "r"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			msg, _ := agg.Message().(types.AssistantMessage)
			if !reflect.DeepEqual(msg.Content, tt.want) {
				t.Fatalf("content = %#v, want %#v", msg.Content, tt.want)
			}
		})
	}
}
