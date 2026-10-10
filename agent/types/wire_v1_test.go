package types

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The golden stream was written by the wire version 1 encoder: text,
// thinking, two interleaved tool calls (one ending without arguments), a
// server tool call and result, a model citation, usage, a tool execution
// with nested child text and rich output, a tool citation, and done.
const wireV1Golden = "testdata/wire_v1_stream.jsonl"

func readGoldenLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// wantUpgraded is the part stream the golden v1 stream means.
func wantUpgraded() []Delta {
	img := wireFile{URI: "file:///a.png", MediaType: MediaPNG, Filename: "a.png", Data: []byte{1, 2, 3}}.source()
	return []Delta{
		PartStart{Index: 0, Kind: KindText},
		PartDelta{Index: 0, Text: "hel"},
		PartDelta{Index: 0, Text: "lo"},
		PartEnd{Index: 0},
		PartStart{Index: 1, Kind: KindThinking},
		PartDelta{Index: 1, Thinking: "hmm"},
		PartDelta{Index: 1, Signature: "sig"},
		PartEnd{Index: 1},
		PartStart{Index: 2, Kind: KindToolCall, ID: "c1", Name: "search"},
		PartStart{Index: 3, Kind: KindToolCall, ID: "c2", Name: "fetch"},
		PartDelta{Index: 3, Args: `{"url":"x"}`},
		PartDelta{Index: 2, Args: `{"q":"go",`},
		PartDelta{Index: 2, Args: `"n":3}`},
		PartEnd{Index: 2, Part: ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{"q": "go", "n": json.Number("3")}}},
		PartEnd{Index: 3, Part: ToolCallPart{ID: "c2", Name: "fetch", Arguments: map[string]any{"url": "x"}}},
		PartStart{Index: 4, Kind: KindServerToolCall, ID: "st1", Name: "web_search"},
		PartEnd{Index: 4, Part: ServerToolCallPart{ID: "st1", ToolKind: ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go"}}},
		PartStart{Index: 5, Kind: KindServerToolResult, ID: "st1"},
		PartEnd{Index: 5, Part: ServerToolResultPart{CallID: "st1", ToolKind: ServerToolWebSearch, Text: "found", Result: json.RawMessage(`{"n":1}`)}},
		PartStart{Index: 6, Kind: KindCitation},
		PartEnd{Index: 6, Part: CitationPart{Citation: Citation{Kind: CitationWeb, URI: "https://x", Title: "X", Start: -1, End: -1}}},
		UsageDelta{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, FinishReasons: []string{"tool_use"}},
		ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		ToolExecDelta{ToolCallID: "c1", Inner: PartStart{Index: 0, Kind: KindText}},
		ToolExecDelta{ToolCallID: "c1", Inner: PartDelta{Index: 0, Text: "child"}},
		ToolExecDelta{ToolCallID: "c1", Inner: PartEnd{Index: 0}},
		ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "ok", Parts: []ToolOutputPart{Text("ok"), Image(img)}},
		CitationDelta{Citation: Citation{Ordinal: 1, Kind: CitationTool, URI: "https://t", Start: -1, End: -1}, ToolCallID: "c1"},
		DoneDelta{},
	}
}

func decodeGolden(t *testing.T, lines []string) []Delta {
	t.Helper()
	var out []Delta
	for _, line := range lines {
		env, err := UnmarshalEnvelope([]byte(line))
		if err != nil {
			t.Fatalf("UnmarshalEnvelope(%s): %v", line, err)
		}
		if env.V != 1 {
			t.Fatalf("golden line is not v1: %s", line)
		}
		d, err := env.Delta()
		if err != nil {
			t.Fatalf("Delta(%s): %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

func TestV1UpgraderGolden(t *testing.T) {
	v1 := decodeGolden(t, readGoldenLines(t, wireV1Golden))
	up := NewV1Upgrader()
	var got []Delta
	for _, d := range v1 {
		got = append(got, up(d)...)
	}
	want := wantUpgraded()
	if len(got) != len(want) {
		t.Fatalf("upgraded %d deltas, want %d:\n%#v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("delta %d:\n got: %#v\nwant: %#v", i, got[i], want[i])
		}
	}
	asm := NewPartAssembler()
	for _, d := range got {
		asm.Push(d)
	}
	if asm.Violations() != 0 || asm.Truncated() {
		t.Fatalf("upgraded stream breaks the part protocol: %d violations, truncated %v", asm.Violations(), asm.Truncated())
	}
	if n := len(asm.Parts()); n != 7 {
		t.Fatalf("assembled %d parts, want 7", n)
	}
}

func TestDecoderUpgradesV1(t *testing.T) {
	dec := NewDecoder()
	var got []Delta
	for _, d := range []Delta{v1TextContent{Content: "no start"}, v1TextEnd{}, v1ToolCallArgument{Content: "orphan"}, DoneDelta{}} {
		env, err := NewDeltaEnvelope(d)
		if err != nil {
			t.Fatal(err)
		}
		ds, err := dec.Decode(env)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ds...)
	}
	want := []Delta{
		PartStart{Index: 0, Kind: KindText}, PartDelta{Index: 0, Text: "no start"}, PartEnd{Index: 0}, DoneDelta{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestV1UpgraderToolCallRules(t *testing.T) {
	up := NewV1Upgrader()
	var got []Delta
	for _, d := range []Delta{
		v1ToolCallStart{ID: "a", Name: "fa"},
		v1ToolCallStart{ID: "b", Name: "fb"},
		v1ToolCallArgument{Content: `{"x":1}`}, // no ID: the newest call
		v1ToolCallStart{ID: "a", Name: "fa"},   // restart of an open call
		v1ToolCallEnd{},                        // no ID: the oldest call
		v1ToolCallEnd{ID: "a", ArgumentsError: "bad"},
	} {
		got = append(got, up(d)...)
	}
	want := []Delta{
		PartStart{Index: 0, Kind: KindToolCall, ID: "a", Name: "fa"},
		PartStart{Index: 1, Kind: KindToolCall, ID: "b", Name: "fb"},
		PartDelta{Index: 1, Args: `{"x":1}`},
		PartStart{Index: 0, Kind: KindToolCall, ID: "a", Name: "fa"},
		PartEnd{Index: 1, Part: ToolCallPart{ID: "b", Name: "fb", Arguments: map[string]any{"x": 1.0}}},
		PartEnd{Index: 0, Part: ToolCallPart{ID: "a", Name: "fa", ArgumentsError: "bad"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func encodeAll(t *testing.T, enc *Encoder, ds []Delta) []string {
	t.Helper()
	var out []string
	for _, d := range ds {
		envs, err := enc.Encode(d)
		if err != nil {
			t.Fatalf("Encode(%#v): %v", d, err)
		}
		for _, env := range envs {
			b, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
	}
	return out
}

func TestV1DowngraderGolden(t *testing.T) {
	enc, err := NewEncoder(EncodeOptions{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := encodeAll(t, enc, wantUpgraded())
	want := readGoldenLines(t, wireV1Golden)
	// A v1 end without arguments left them in the fragments; the part
	// stream carries the decoded call, so the downgrade writes them out.
	want[13] = `{"v":1,"kind":"tool.call.end","data":{"id":"c2","arguments":{"url":"x"}}}`
	if len(got) != len(want) {
		t.Fatalf("downgraded %d envelopes, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func TestV1DowngraderUnrepresentable(t *testing.T) {
	down := NewV1Downgrader()
	var got []Delta
	for _, d := range []Delta{
		PartStart{Index: 0, Kind: KindRefusal},
		PartDelta{Index: 0, Refusal: "no"},
		PartEnd{Index: 0, Part: RefusalPart{Text: "no"}},
		PartEnd{Index: 1, Part: ImageOutPart{Source: Bytes(MediaPNG, []byte{1})}}, // an end with no start
		ConversionDelta{Profile: "p"},
		ToolExecDelta{ToolCallID: "c", Inner: PartStart{Index: 0, Kind: KindAudioOut, MediaType: MediaWAV}},
		PartStart{Index: 2, Kind: KindServerToolResult, ID: "s"},
		PartEnd{Index: 2, Part: ServerToolResultPart{CallID: "s", Outputs: []Part{ImageOutPart{Source: URL("https://x")}}}},
		PartStart{Index: 3, Kind: KindText},
		PartEnd{Index: 3, Part: TextPart{Text: "whole"}},
	} {
		got = append(got, down(d)...)
	}
	var errs int
	for _, d := range got {
		if ed, ok := d.(ErrorDelta); ok {
			errs++
			if !errors.Is(ed.Error, ErrWireUnrepresentable) {
				t.Errorf("error %v does not match ErrWireUnrepresentable", ed.Error)
			}
		}
	}
	if errs != 4 {
		t.Fatalf("got %d unrepresentable errors, want 4: %#v", errs, got)
	}
	tail := got[len(got)-3:]
	if !reflect.DeepEqual(tail, []Delta{v1TextStart{}, v1TextContent{Content: "whole"}, v1TextEnd{}}) {
		t.Errorf("text with an authoritative end = %#v", tail)
	}

	// On the wire the error carries its stable code.
	b, err := MarshalDelta(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"wire_unrepresentable"`) {
		t.Errorf("error envelope lacks its code: %s", b)
	}
	back, err := UnmarshalDelta(b)
	if err != nil || !errors.Is(back.(ErrorDelta).Error, ErrWireUnrepresentable) {
		t.Errorf("decoded error = %v, %v", back, err)
	}
}

func TestV1RoundTripThroughBothVersions(t *testing.T) {
	// v2 -> v1 -> v2 keeps every part a v1 client could see.
	down := NewV1Downgrader()
	up := NewV1Upgrader()
	var back []Delta
	for _, d := range wantUpgraded() {
		for _, v1 := range down(d) {
			back = append(back, up(v1)...)
		}
	}
	a, b := NewPartAssembler(), NewPartAssembler()
	for _, d := range wantUpgraded() {
		a.Push(d)
	}
	for _, d := range back {
		b.Push(d)
	}
	if !reflect.DeepEqual(a.Parts(), b.Parts()) {
		t.Fatalf("parts after a v1 round trip:\n got: %#v\nwant: %#v", b.Parts(), a.Parts())
	}
}
