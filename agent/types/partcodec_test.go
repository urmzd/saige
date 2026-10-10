package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// partCases holds one populated value per part kind. Numbers inside maps are
// json.Number because the codec keeps their text.
func partCases() []Part {
	src := Bytes(MediaPNG, []byte{1, 2, 3}).With(Source{URI: "https://x/a.png", Filename: "a.png",
		Ref: "saige-artifact://abc", Files: []VendorFile{{Provider: "anthropic", Endpoint: "anthropic", ID: "file_1",
			ExpiresAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}})
	args := map[string]any{"q": "go", "n": json.Number("3")}
	return []Part{
		Text("hello"),
		JSONPart{JSON: json.RawMessage(`{"a":1}`)},
		Image(src, ImageMeta{Width: 10, Height: 20, Detail: "high"}),
		Audio(URL("gs://b/a.wav", MediaWAV), AudioMeta{Duration: 3 * time.Second, SampleRate: 16000, Channels: 1, Format: "wav"}),
		Video(URL("https://youtu.be/x", MediaMP4), VideoMeta{FPS: 1, ClipStart: time.Second, ClipEnd: 2 * time.Second}),
		Document(Artifact("saige-artifact://d1", MediaPDF), DocumentMeta{Pages: 3, Title: "T", Citations: true}),
		File(VendorFileID("openai", "openai-responses", "file-9", "application/zip")),
		ToolOK("c1", Text("ok"), Image(URL("https://x/b.png", MediaPNG)), JSONPart{JSON: json.RawMessage(`[1]`)}),
		ToolResultPart{CallID: "c2", Parts: []ToolOutputPart{Text("boom")}, IsError: true,
			Citations: []Citation{{Ordinal: 1, Kind: CitationTool, URI: "u", Start: -1, End: -1}}, ToolVersion: "2"},
		ThinkingPart{Text: "hmm", Signature: "sig", Summary: true},
		ThinkingPart{Signature: "enc", Redacted: true},
		ToolCallPart{ID: "c1", Name: "search", Arguments: args},
		ToolCallPart{ID: "c3", Name: "bad", ArgumentsError: "unexpected end of JSON input"},
		ServerToolCallPart{ID: "s1", ToolKind: ServerToolWebSearch, Name: "web_search", Input: args},
		ServerToolResultPart{CallID: "s1", ToolKind: ServerToolCodeExecution, Text: "42", Result: json.RawMessage(`{"stdout":"42"}`),
			IsError: true, Outputs: []Part{Document(URL("file:///o.csv", MediaCSV))}},
		CitationPart{Citation: Citation{Ordinal: 2, Kind: CitationWeb, URI: "https://w", Title: "W", Start: -1, End: -1},
			Anchor: &Anchor{PartIndex: 0, Start: 1, End: 4}},
		AudioOutPart{Source: URL("https://x/o.wav", MediaWAV), Transcript: "hi", VendorID: "aud_1",
			ExpiresAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		ImageOutPart{Source: URL("https://x/o.png", MediaPNG), ImageMeta: ImageMeta{Width: 64}, RevisedPrompt: "a cat"},
		VideoOutPart{Source: URL("gs://b/v.mp4", MediaMP4), Operation: "op/1"},
		RefusalPart{Text: "no", Category: "safety"},
		ConfigPart{Model: "m", MaxIter: 3},
		RoutePart{Profile: "p", Model: "m", Conversions: &ConversionReport{Offering: "o", Hash: "h"}},
		SteerPart{ID: "s"},
		TruncationPart{Reason: "max_tokens", Dropped: []PartKind{KindToolCall}},
		ApprovalPart{Event: ApprovalEventGranted, Tool: "rm", ToolCallID: "c1"},
		HandoffPart{To: "b", From: "a"},
		FeedbackPart{TargetNodeID: "n", Rating: RatingPositive},
		CompactionPart{Strategy: "summary", Trigger: CompactionTriggerRule, TokensBefore: 9, TokensAfter: 3},
		GuardrailPart{Guardrail: "pii", Phase: GuardrailPhaseInput, Action: GuardrailActionBlock},
	}
}

// withoutInline drops the bytes of every source in p, as the persisted form
// does.
func withoutInline(p Part) Part {
	strip := func(s Source) Source { s.Inline = nil; return s }
	switch v := p.(type) {
	case ImagePart:
		v.Source = strip(v.Source)
		return v
	case ToolResultPart:
		parts := make([]ToolOutputPart, len(v.Parts))
		for i, x := range v.Parts {
			parts[i] = withoutInline(x).(ToolOutputPart)
		}
		v.Parts = parts
		return v
	}
	return p
}

func TestPartCodecRoundTrip(t *testing.T) {
	kinds := map[PartKind]bool{}
	for _, p := range partCases() {
		kinds[p.Kind()] = true
		t.Run(fmt.Sprintf("%T", p), func(t *testing.T) {
			b, err := MarshalPart(p)
			if err != nil {
				t.Fatalf("MarshalPart: %v", err)
			}
			if !strings.HasPrefix(string(b), `{"type":"`+string(p.Kind())+`"`) {
				t.Errorf("encoding does not lead with its type tag: %s", b)
			}
			if strings.Contains(string(b), `"data":`) {
				t.Errorf("persisted form carries inline bytes: %s", b)
			}
			got, err := UnmarshalPart(b)
			if err != nil {
				t.Fatalf("UnmarshalPart(%s): %v", b, err)
			}
			if want := withoutInline(p); !reflect.DeepEqual(got, want) {
				t.Errorf("round trip mismatch\n got: %#v\nwant: %#v\njson: %s", got, want, b)
			}

			wire, err := MarshalPartInline(p)
			if err != nil {
				t.Fatalf("MarshalPartInline: %v", err)
			}
			back, err := UnmarshalPart(wire)
			if err != nil {
				t.Fatalf("UnmarshalPart(inline %s): %v", wire, err)
			}
			if !reflect.DeepEqual(back, p) {
				t.Errorf("inline round trip mismatch\n got: %#v\nwant: %#v", back, p)
			}
		})
	}
	for _, k := range []PartKind{KindText, KindImage, KindAudio, KindVideo, KindDocument, KindFile, KindToolResult, KindJSON,
		KindThinking, KindToolCall, KindServerToolCall, KindServerToolResult, KindCitation, KindAudioOut, KindImageOut,
		KindVideoOut, KindRefusal, KindConfig, KindRoute, KindSteer, KindTruncation, KindApproval, KindHandoff,
		KindFeedback, KindCompaction, KindGuardrail} {
		if !kinds[k] {
			t.Errorf("no round-trip case for %s", k)
		}
	}
}

func TestPartCodecShapes(t *testing.T) {
	tests := []struct {
		p    Part
		want string
	}{
		{Image(Artifact("saige-artifact://9f", MediaPNG), ImageMeta{Width: 1024, Height: 768}),
			`{"type":"image","source":{"media_type":"image/png","sha256":"9f","ref":"saige-artifact://9f"},"image":{"width":1024,"height":768}}`},
		{ToolOK("c1", Text("ok")), `{"type":"tool_result","call_id":"c1","parts":[{"type":"text","text":"ok"}]}`},
		{ToolCallPart{ID: "call_9", Name: "search", Arguments: map[string]any{"q": "go"}},
			`{"type":"tool_call","id":"call_9","name":"search","arguments":{"q":"go"}}`},
		{ToolResultPart{CallID: "c"}, `{"type":"tool_result","call_id":"c","parts":null}`},
	}
	for _, tt := range tests {
		b, err := MarshalPart(tt.p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tt.want {
			t.Errorf("MarshalPart = %s\nwant          %s", b, tt.want)
		}
	}
}

func TestUnmarshalPartRejects(t *testing.T) {
	tests := []struct {
		in   string
		want error
	}{
		{`{"type":"telepathy"}`, ErrUnknownPartKind},
		{`{"text":"no tag"}`, ErrUnknownPartKind},
		{`{"type":"tool_result","call_id":"c","parts":[{"type":"tool_call","name":"f"}]}`, ErrPartRole},
	}
	for _, tt := range tests {
		if _, err := UnmarshalPart([]byte(tt.in)); !errors.Is(err, tt.want) {
			t.Errorf("UnmarshalPart(%s) = %v, want %v", tt.in, err, tt.want)
		}
	}
	if _, err := UnmarshalPart([]byte(`not json`)); err == nil {
		t.Error("invalid JSON decoded")
	}
	if _, err := MarshalPart(nil); !errors.Is(err, ErrUnknownPartKind) {
		t.Errorf("MarshalPart(nil) = %v", err)
	}
}

func TestUnmarshalPartIgnoresUnknownFields(t *testing.T) {
	p, err := UnmarshalPart([]byte(`{"type":"text","text":"hi","added_later":{"x":1}}`))
	if err != nil || p != Part(Text("hi")) {
		t.Fatalf("UnmarshalPart = %#v, %v", p, err)
	}
}

func TestUnmarshalRolePart(t *testing.T) {
	call := []byte(`{"type":"tool_call","id":"c","name":"f"}`)
	if _, err := UnmarshalRolePart[AssistantPart](call); err != nil {
		t.Errorf("assistant part: %v", err)
	}
	if _, err := UnmarshalRolePart[UserPart](call); !errors.Is(err, ErrPartRole) {
		t.Errorf("tool call as user part: %v", err)
	}
	img := []byte(`{"type":"image","source":{"media_type":"image/png","uri":"https://x"}}`)
	if p, err := UnmarshalRolePart[ToolOutputPart](img); err != nil || p.Kind() != KindImage {
		t.Errorf("image as tool output: %v, %v", p, err)
	}
	if _, err := UnmarshalRolePart[AssistantPart](img); !errors.Is(err, ErrPartRole) {
		t.Errorf("image as assistant part: %v", err)
	}
}

func FuzzUnmarshalPart(f *testing.F) {
	for _, p := range partCases() {
		b, err := MarshalPartInline(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"type":"tool_result","parts":[{"type":"tool_result","parts":[]}]}`))
	f.Add([]byte(`{"type":"image","source":{"data":"!!"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := UnmarshalPart(b)
		if err != nil {
			return
		}
		// A part that decodes encodes again, and that encoding decodes to
		// the same part.
		again, err := MarshalPartInline(p)
		if err != nil {
			t.Fatalf("decoded %#v from %q but cannot encode it: %v", p, b, err)
		}
		back, err := UnmarshalPart(again)
		if err != nil {
			t.Fatalf("re-encoded %s does not decode: %v", again, err)
		}
		if back.Kind() != p.Kind() {
			t.Fatalf("kind changed: %s -> %s", p.Kind(), back.Kind())
		}
	})
}
