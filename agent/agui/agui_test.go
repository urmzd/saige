package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = string(ev.Type)
	}
	return out
}

func mapAll(t *testing.T, m *Mapper, deltas []types.Delta) []Event {
	t.Helper()
	var out []Event
	for _, d := range deltas {
		events, err := m.Map(d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, events...)
	}
	return append(out, m.Close()...)
}

func TestMapperSequences(t *testing.T) {
	tests := []struct {
		name   string
		deltas []types.Delta
		want   []EventType
	}{
		{
			name: "text message",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "hi"}, types.PartEnd{Index: 0}, types.DoneDelta{},
			},
			want: []EventType{TextMessageStart, TextMessageContent, TextMessageEnd, RunFinished},
		},
		{
			name:   "content without a start opens a message and done closes it",
			deltas: []types.Delta{types.PartDelta{Index: 1, Text: "hi"}, types.PartDelta{Index: 1}, types.DoneDelta{}},
			want:   []EventType{TextMessageStart, TextMessageContent, TextMessageEnd, RunFinished},
		},
		{
			name: "tool call and result",
			deltas: []types.Delta{
				types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "c1", Name: "read"},
				types.PartDelta{Index: 2, Args: `{"path":"a"}`},
				types.PartEnd{Index: 2, Part: types.ToolCallPart{ID: "c1", Name: "read", Arguments: map[string]any{"path": "a"}}},
				types.ToolExecStartDelta{ToolCallID: "c1", Name: "read"},
				types.ToolExecEndDelta{ToolCallID: "c1", Name: "read", Result: "contents"},
				types.DoneDelta{},
			},
			want: []EventType{ToolCallStart, ToolCallArgs, ToolCallEnd, Custom, ToolCallResult, RunFinished},
		},
		{
			name: "thinking then text",
			deltas: []types.Delta{
				types.PartStart{Index: 3, Kind: types.KindThinking}, types.PartDelta{Index: 3, Thinking: "hmm"},
				types.PartDelta{Index: 1, Text: "answer"}, types.DoneDelta{},
			},
			want: []EventType{
				ThinkingStart, ThinkingTextMessageStart, ThinkingTextMessageContent, ThinkingTextMessageEnd, ThinkingEnd,
				TextMessageStart, TextMessageContent, TextMessageEnd, RunFinished,
			},
		},
		{
			name:   "error ends the run and later deltas are dropped",
			deltas: []types.Delta{types.PartDelta{Index: 1, Text: "x"}, types.ErrorDelta{Error: errors.New("boom")}, types.DoneDelta{}},
			want:   []EventType{TextMessageStart, TextMessageContent, TextMessageEnd, RunError},
		},
		{
			name:   "stream that closes early is an error",
			deltas: []types.Delta{types.PartDelta{Index: 1, Text: "x"}},
			want:   []EventType{TextMessageStart, TextMessageContent, TextMessageEnd, RunError},
		},
		{
			name:   "other deltas become custom events",
			deltas: []types.Delta{types.UsageDelta{PromptTokens: 3}, types.HandoffDelta{From: "a", To: "b"}, types.DoneDelta{}},
			want:   []EventType{Custom, Custom, RunFinished},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eventTypes(mapAll(t, NewMapper("thread", "run"), tt.deltas))
			want := make([]string, len(tt.want))
			for i, w := range tt.want {
				want[i] = string(w)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("events = %v, want %v", got, want)
			}
		})
	}
}

func TestMapperFields(t *testing.T) {
	m := NewMapper("thread", "run")
	events := mapAll(t, m, []types.Delta{
		types.PartDelta{Index: 0, Text: "let me look"},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "read"},
		types.ToolExecEndDelta{ToolCallID: "c1", Error: "denied"},
		types.ErrorDelta{Error: types.ErrResponseTruncated},
	})
	byType := map[EventType]Event{}
	for _, ev := range events {
		byType[ev.Type] = ev
	}
	msg := byType[TextMessageStart]
	if msg.MessageID != "run-msg-1" || msg.Role != roleAssistant {
		t.Errorf("message start = %+v", msg)
	}
	if call := byType[ToolCallStart]; call.ParentMessageID != msg.MessageID || call.ToolCallName != "read" {
		t.Errorf("tool call start = %+v", call)
	}
	if res := byType[ToolCallResult]; res.Content != "denied" || res.Role != roleTool || res.MessageID == "" {
		t.Errorf("tool result = %+v", res)
	}
	if runErr := byType[RunError]; runErr.Code == "" || runErr.Message == "" {
		t.Errorf("run error = %+v", runErr)
	}
}

func TestCustomEventCarriesEnvelope(t *testing.T) {
	m := NewMapper("thread", "run")
	inner := types.ToolExecDelta{ToolCallID: "c1", Inner: types.PartDelta{Index: 0, Text: "from child"}}
	events, err := m.Map(inner)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != Custom || events[0].Name != CustomEventName {
		t.Fatalf("events = %+v", events)
	}
	env, err := types.UnmarshalEnvelope(events[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Path) != 1 || env.Path[0] != "c1" {
		t.Fatalf("path = %v", env.Path)
	}
	d, err := env.Delta()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := d.(types.PartDelta); !ok || got.Text != "from child" {
		t.Fatalf("delta = %#v", d)
	}
}

func TestStream(t *testing.T) {
	deltas := make(chan types.Delta, 4)
	deltas <- types.PartDelta{Index: 0, Text: "hi"}
	deltas <- types.DoneDelta{}
	close(deltas)

	var buf bytes.Buffer
	if err := Stream(context.Background(), &buf, "thread", "run", deltas); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, frame := range strings.Split(strings.TrimSpace(buf.String()), "\n\n") {
		raw, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			t.Fatalf("frame %q is not a data line", frame)
		}
		var ev Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		got = append(got, string(ev.Type))
	}
	want := "RUN_STARTED,TEXT_MESSAGE_START,TEXT_MESSAGE_CONTENT,TEXT_MESSAGE_END,RUN_FINISHED"
	if strings.Join(got, ",") != want {
		t.Fatalf("events = %v, want %s", got, want)
	}
}

func TestStreamStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Stream(ctx, &bytes.Buffer{}, "thread", "run", make(chan types.Delta))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
