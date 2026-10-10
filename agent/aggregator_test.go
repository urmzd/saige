package agent

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestDefaultAggregatorInterleavedParts(t *testing.T) {
	agg := NewDefaultAggregator()
	for _, d := range []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "b", Name: "fb"},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "a", Name: "fa"},
		types.PartDelta{Index: 2, Args: `{"y":`},
		types.PartDelta{Index: 0, Text: "he"},
		types.PartDelta{Index: 1, Args: `{"x":1}`},
		types.PartDelta{Index: 2, Args: `2}`},
		types.PartEnd{Index: 2},
		types.PartDelta{Index: 0, Text: "llo"},
		types.PartEnd{Index: 1},
		types.PartEnd{Index: 0},
	} {
		agg.Push(d)
	}
	want := []types.AssistantPart{
		types.TextPart{Text: "hello"},
		types.ToolCallPart{ID: "a", Name: "fa", Arguments: map[string]any{"x": 1.0}},
		types.ToolCallPart{ID: "b", Name: "fb", Arguments: map[string]any{"y": 2.0}},
	}
	if got := agg.Message().(types.AssistantMessage).Parts; !reflect.DeepEqual(got, want) {
		t.Fatalf("parts = %#v\nwant %#v", got, want)
	}
	if agg.Violations() != 0 || agg.Truncated() {
		t.Fatalf("violations = %d, truncated = %v", agg.Violations(), agg.Truncated())
	}
}

func TestDefaultAggregatorViolations(t *testing.T) {
	tests := []struct {
		name   string
		deltas []types.Delta
		want   []types.AssistantPart
		count  int
	}{
		{
			name:   "delta without a start is dropped",
			deltas: []types.Delta{types.PartDelta{Index: 0, Text: "x"}},
			count:  1,
		},
		{
			name:   "end without a start is ignored",
			deltas: []types.Delta{types.PartEnd{Index: 3, Part: types.TextPart{Text: "x"}}},
			count:  1,
		},
		{
			name: "start for a closed index is ignored",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "a"}, types.PartEnd{Index: 0},
				types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "b"}, types.PartEnd{Index: 0},
			},
			want:  []types.AssistantPart{types.TextPart{Text: "a"}},
			count: 3,
		},
		{
			name: "start for an open index restarts it",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "a", Name: "f"}, types.PartDelta{Index: 0, Args: `{"x":`},
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "a", Name: "f"}, types.PartDelta{Index: 0, Args: `{}`},
				types.PartEnd{Index: 0},
			},
			want: []types.AssistantPart{types.ToolCallPart{ID: "a", Name: "f", Arguments: map[string]any{}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := NewDefaultAggregator()
			for _, d := range tt.deltas {
				agg.Push(d)
			}
			var got []types.AssistantPart
			if m := agg.Message(); m != nil {
				got = m.(types.AssistantMessage).Parts
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parts = %#v, want %#v", got, tt.want)
			}
			if agg.Violations() != tt.count {
				t.Errorf("violations = %d, want %d", agg.Violations(), tt.count)
			}
		})
	}
}

func TestDefaultAggregatorFlush(t *testing.T) {
	done := types.ToolCallPart{ID: "a", Name: "fa", Arguments: map[string]any{"x": 1}}
	text := func(i int, s string) []types.Delta {
		return []types.Delta{types.PartStart{Index: i, Kind: types.KindText}, types.PartDelta{Index: i, Text: s}}
	}
	tests := []struct {
		name          string
		deltas        []types.Delta
		want          []types.AssistantPart
		wantTruncated bool
		wantDropped   []types.PartKind
	}{
		{
			name:   "complete turn",
			deltas: append(text(0, "ok"), types.PartEnd{Index: 0}),
			want:   []types.AssistantPart{types.TextPart{Text: "ok"}},
		},
		{
			name:          "open text is kept",
			deltas:        text(0, "par"),
			want:          []types.AssistantPart{types.TextPart{Text: "par"}},
			wantTruncated: true,
		},
		{
			name: "open thinking is dropped",
			deltas: append(append(text(0, "a"), types.PartEnd{Index: 0}),
				types.PartStart{Index: 1, Kind: types.KindThinking}, types.PartDelta{Index: 1, Thinking: "hmm"}),
			want:          []types.AssistantPart{types.TextPart{Text: "a"}},
			wantTruncated: true,
			wantDropped:   []types.PartKind{types.KindThinking},
		},
		{
			name: "open tool call is dropped, finished one kept",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "a", Name: "fa"}, types.PartEnd{Index: 0, Part: done},
				types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "b", Name: "fb"}, types.PartDelta{Index: 1, Args: `{"y":`},
			},
			want:          []types.AssistantPart{done},
			wantTruncated: true,
			wantDropped:   []types.PartKind{types.KindToolCall},
		},
		{
			name:          "only an open tool call",
			deltas:        []types.Delta{types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "b", Name: "fb"}},
			wantTruncated: true,
			wantDropped:   []types.PartKind{types.KindToolCall},
		},
		{
			name: "open media and refusal are dropped",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindAudioOut, MediaType: types.MediaWAV}, types.PartDelta{Index: 0, Data: []byte{1}},
				types.PartStart{Index: 1, Kind: types.KindRefusal}, types.PartDelta{Index: 1, Refusal: "no"},
			},
			wantTruncated: true,
			wantDropped:   []types.PartKind{types.KindAudioOut, types.KindRefusal},
		},
		{
			name: "interleaved open text keeps its index order",
			deltas: append(append(text(1, "second"), text(0, "first")...),
				types.PartStart{Index: 2, Kind: types.KindThinking}),
			want:          []types.AssistantPart{types.TextPart{Text: "first"}, types.TextPart{Text: "second"}},
			wantTruncated: true,
			wantDropped:   []types.PartKind{types.KindThinking},
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
			msg, truncated, dropped := agg.FlushDropped()
			if truncated != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", truncated, tt.wantTruncated)
			}
			if !reflect.DeepEqual(dropped, tt.wantDropped) {
				t.Errorf("dropped = %v, want %v", dropped, tt.wantDropped)
			}
			var got []types.AssistantPart
			if msg != nil {
				got = msg.(types.AssistantMessage).Parts
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
			if agg.Truncated() || agg.OpenToolCalls() != nil {
				t.Error("parts still open after Flush")
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
	agg.Push(types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "b", Name: "fb"})
	agg.Push(types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "a", Name: "fa"})
	if got := agg.OpenToolCalls(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("open = %v", got)
	}
	agg.Push(types.PartEnd{Index: 0})
	if got := agg.OpenToolCalls(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("open = %v", got)
	}
	agg.Reset()
	if agg.OpenToolCalls() != nil || agg.Message() != nil || agg.Violations() != 0 {
		t.Error("Reset left state behind")
	}
}

func TestDefaultAggregatorServerToolsStaySeparate(t *testing.T) {
	search := types.ServerToolKind("web_search")
	call := types.ServerToolCallPart{ID: "s1", ToolKind: search, Name: "web_search", Input: map[string]any{"q": "go"}}
	result := types.ServerToolResultPart{CallID: "s1", ToolKind: search, Text: "found"}
	agg := NewDefaultAggregator()
	for _, d := range append(types.PartDeltas(0, call), types.PartDeltas(1, result)...) {
		agg.Push(d)
	}
	got := agg.Message().(types.AssistantMessage).Parts
	if !reflect.DeepEqual(got, []types.AssistantPart{call, result}) {
		t.Fatalf("parts = %#v", got)
	}
	pairs := types.PairServerTools(got)
	if len(pairs) != 1 || pairs[0].Result == nil || pairs[0].Result.Text != "found" {
		t.Fatalf("pairs = %#v", pairs)
	}
}

func TestDefaultAggregatorBuildsMediaFromChunks(t *testing.T) {
	agg := NewDefaultAggregator()
	for _, d := range []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindAudioOut, MediaType: types.MediaWAV},
		types.PartDelta{Index: 0, Data: []byte("RI")},
		types.PartDelta{Index: 0, Transcript: "hi"},
		types.PartDelta{Index: 0, Data: []byte("FF")},
		types.PartEnd{Index: 0},
	} {
		agg.Push(d)
	}
	got := agg.Message().(types.AssistantMessage).Parts[0].(types.AudioOutPart)
	if string(got.Source.Inline) != "RIFF" || got.Source.MediaType != types.MediaWAV || got.Transcript != "hi" {
		t.Fatalf("audio = %+v", got)
	}
	if got.Source.Digest == "" || got.Source.Size != 4 {
		t.Fatalf("source digest and size not set: %+v", got.Source)
	}
}

// genPart is one part of a random turn and the deltas that stream it.
type genPart struct {
	part   types.AssistantPart
	deltas []types.Delta
}

func randomTurn(r *rand.Rand, n int) []genPart {
	parts := make([]genPart, n)
	for i := range parts {
		switch r.IntN(3) {
		case 0:
			text := fmt.Sprintf("text-%d-%d", i, r.IntN(1000))
			ds := []types.Delta{types.PartStart{Index: i, Kind: types.KindText}}
			for j := 0; j < len(text); j += 3 {
				ds = append(ds, types.PartDelta{Index: i, Text: text[j:min(j+3, len(text))]})
			}
			parts[i] = genPart{types.TextPart{Text: text}, append(ds, types.PartEnd{Index: i})}
		case 1:
			th := fmt.Sprintf("think-%d", i)
			parts[i] = genPart{types.ThinkingPart{Text: th, Signature: "sig" + fmt.Sprint(i)}, []types.Delta{
				types.PartStart{Index: i, Kind: types.KindThinking},
				types.PartDelta{Index: i, Thinking: th[:2]}, types.PartDelta{Index: i, Thinking: th[2:]},
				types.PartDelta{Index: i, Signature: "sig"}, types.PartDelta{Index: i, Signature: fmt.Sprint(i)},
				types.PartEnd{Index: i},
			}}
		default:
			id := fmt.Sprintf("call-%d", i)
			raw := fmt.Sprintf(`{"n":%d,"s":"v%d"}`, i, i)
			ds := []types.Delta{types.PartStart{Index: i, Kind: types.KindToolCall, ID: id, Name: "f"}}
			for j := 0; j < len(raw); j += 4 {
				ds = append(ds, types.PartDelta{Index: i, Args: raw[j:min(j+4, len(raw))]})
			}
			parts[i] = genPart{
				types.ToolCallPart{ID: id, Name: "f", Arguments: map[string]any{"n": float64(i), "s": fmt.Sprintf("v%d", i)}},
				append(ds, types.PartEnd{Index: i}),
			}
		}
	}
	return parts
}

// interleave merges the per-part delta lists in a random order that keeps
// each part's own deltas in sequence.
func interleave(r *rand.Rand, parts []genPart) []types.Delta {
	pos := make([]int, len(parts))
	var out []types.Delta
	for {
		var live []int
		for i, p := range parts {
			if pos[i] < len(p.deltas) {
				live = append(live, i)
			}
		}
		if len(live) == 0 {
			return out
		}
		i := live[r.IntN(len(live))]
		out = append(out, parts[i].deltas[pos[i]])
		pos[i]++
	}
}

// Any interleaving of N concurrent parts builds the same message as
// streaming them one after another.
func TestDefaultAggregatorInterleavingProperty(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		parts := randomTurn(r, 1+r.IntN(8))

		seq := NewDefaultAggregator()
		var want []types.AssistantPart
		for _, p := range parts {
			want = append(want, p.part)
			for _, d := range p.deltas {
				seq.Push(d)
			}
		}
		mixed := NewDefaultAggregator()
		for _, d := range interleave(r, parts) {
			mixed.Push(d)
		}
		got := mixed.Message().(types.AssistantMessage).Parts
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, seq.Message().(types.AssistantMessage).Parts) {
			t.Fatalf("seed %d: interleaved = %#v\nwant %#v", seed, got, want)
		}
		if mixed.Violations() != 0 || mixed.Truncated() {
			t.Fatalf("seed %d: violations = %d, truncated = %v", seed, mixed.Violations(), mixed.Truncated())
		}
	}
}
