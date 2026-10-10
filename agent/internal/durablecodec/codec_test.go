package durablecodec

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func ptr[T any](v T) *T { return &v }

func messageCases() []types.Message {
	return []types.Message{
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.TextPart{Text: "partial"}, types.TruncationPart{Reason: "max_tokens"},
		}},
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ServerToolCallPart{ID: "s", ToolKind: types.ServerToolKind("web_search"), Name: "web_search", Input: map[string]any{"q": "go", "n": 2.0}},
			types.ServerToolResultPart{CallID: "s", ToolKind: types.ServerToolKind("web_search"), Text: "r", Result: json.RawMessage(`{"a":1}`)},
			types.RoutePart{Profile: "p", Options: &types.RequestOptions{Temperature: ptr(0.5)}},
		}},
		types.UserMessage{Parts: []types.UserPart{
			types.TextPart{Text: "x"}, types.SteerPart{ID: "sub"}, types.ConfigPart{Target: types.ModelTarget("m"), MaxIter: 2},
		}},
		types.UserMessage{Parts: []types.UserPart{
			types.Image(types.Bytes(types.MediaPNG, []byte{1, 2, 3}), types.ImageMeta{Detail: "high"}),
			types.ToolOK("c", types.Text("t"), types.JSONPart{JSON: []byte(`{"a":1}`)}, types.Document(types.URL("file:///d.pdf", types.MediaPDF)),
				types.Image(types.Bytes(types.MediaPNG, []byte{4, 5}))),
		}},
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.CitationPart{Citation: types.NewCitation(types.CitationWeb, "https://x", "x")}, types.RefusalPart{Text: "no"},
			types.ThinkingPart{Text: "t", Signature: "sig"},
		}},
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ToolCallPart{ID: "c", Name: "f", Arguments: map[string]any{"a": []any{"x", map[string]any{"b": 1.0}}, "n": 12345678901234.0}},
		}},
		types.SystemMessage{Parts: []types.SystemPart{types.Text("s"), types.CompactionPart{Strategy: "summary", Kept: []types.NodeID{"a"}}}},
		types.UserMessage{},
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	in := messageCases()
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	var out []types.Message
	if err := Decode(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip =\n%#v\nwant\n%#v", out, in)
	}
	// Media bytes survive, so a replayed step sends what the live one did.
	img := out[3].(types.UserMessage).Parts[0].(types.ImagePart)
	if !bytes.Equal(img.Source.Inline, []byte{1, 2, 3}) {
		t.Fatalf("lost bytes: %+v", img.Source)
	}
	// Tool arguments read as float64, as a provider stream decodes them.
	args := out[5].(types.AssistantMessage).Parts[0].(types.ToolCallPart).Arguments
	if _, ok := args["n"].(float64); !ok {
		t.Fatalf("argument n = %T", args["n"])
	}
}

func TestAssistantRoundTrip(t *testing.T) {
	for _, m := range messageCases() {
		a, ok := m.(types.AssistantMessage)
		if !ok {
			continue
		}
		raw, err := Encode(a)
		if err != nil {
			t.Fatal(err)
		}
		var out types.AssistantMessage
		if err := Decode(raw, &out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, a) {
			t.Fatalf("round trip = %#v, want %#v", out, a)
		}
	}
}

func stepCases() []types.StepResult {
	return []types.StepResult{
		{Kind: types.StepKindLLM, Message: &types.AssistantMessage{Parts: []types.AssistantPart{types.Text("hi"),
			types.ToolCallPart{ID: "c", Name: "f", Arguments: map[string]any{"x": 1.0}}}},
			Usage:   &types.UsageDelta{AccountingID: "a", PromptTokens: 3},
			Receipt: &types.BudgetReceipt{ID: "r", Model: "m"}, ConversionReceipts: []types.BudgetReceipt{{ID: "c1"}}},
		{Kind: types.StepKindLLM, Message: &types.AssistantMessage{}},
		{Kind: types.StepKindTool, ToolCallID: "c", ToolResult: "t", ToolParts: []types.ToolOutputPart{types.Text("t"),
			types.Image(types.Bytes(types.MediaPNG, []byte{9}))}},
		{Kind: types.StepKindTool, ToolCallID: "c", ToolError: "boom"},
		{Kind: types.StepKindTool, ToolCallID: "c", ToolResult: "r", ToolCitations: []types.Citation{types.NewCitation(types.CitationWeb, "https://x", "x")}},
		{Kind: types.StepKindApproval, Approval: &types.ApprovalVerdict{Outcome: types.VerdictAsk}},
		{Kind: types.StepKindHook, Hook: &types.HookRecord{Name: "h", Changed: true, Message: &types.UserMessage{Parts: []types.UserPart{types.Text("x")}},
			Arguments: map[string]any{"k": "v"}, Usage: []types.UsageDelta{{PromptTokens: 1}}}},
		{Kind: types.StepKindHook, Hook: &types.HookRecord{Abort: true, Name: "stop"}},
		{Kind: types.StepKindConvert, Conversion: &types.ConversionEntry{Parts: []types.Part{types.Text("transcript")}, Via: "stt@1"}},
	}
}

func TestStepRoundTrip(t *testing.T) {
	for _, in := range stepCases() {
		raw, err := Encode(in)
		if err != nil {
			t.Fatal(err)
		}
		var out types.StepResult
		if err := Decode(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.V != Version {
			t.Fatalf("decoded version = %d", out.V)
		}
		out.V = 0
		if !reflect.DeepEqual(out, in) {
			t.Fatalf("round trip =\n%#v\nwant\n%#v", out, in)
		}
	}
}

// TestRecordCarriesEveryField fails when StepResult or HookRecord gains a
// field its record does not store.
func TestRecordCarriesEveryField(t *testing.T) {
	check := func(src, rec reflect.Type, converted map[string]string) {
		t.Helper()
		for i := range src.NumField() {
			f := src.Field(i)
			name, conv := converted[f.Name]
			if !conv {
				name = f.Name
			}
			r, ok := rec.FieldByName(name)
			if !ok {
				t.Errorf("%s.%s has no field %s in %s", src.Name(), f.Name, name, rec.Name())
				continue
			}
			if !conv && r.Type != f.Type {
				t.Errorf("%s.%s is %s in the record, want %s", src.Name(), f.Name, r.Type, f.Type)
			}
		}
	}
	check(reflect.TypeFor[types.StepResult](), reflect.TypeFor[stepRecord](),
		map[string]string{"Message": "MessageParts", "ToolParts": "ToolOutput", "Hook": "Hook", "Conversion": "Conversion"})
	check(reflect.TypeFor[types.HookRecord](), reflect.TypeFor[hookRecord](), map[string]string{"Message": "MessageParts"})
	check(reflect.TypeFor[types.ConversionEntry](), reflect.TypeFor[conversionRecord](), map[string]string{"Parts": "Parts"})
}

func TestNewerVersionRejected(t *testing.T) {
	raw, err := gobEncode(stepRecord{V: Version + 1})
	if err != nil {
		t.Fatal(err)
	}
	var s types.StepResult
	if err := Decode(raw, &s); !errors.Is(err, ErrVersion) {
		t.Fatalf("err = %v", err)
	}
	raw, err = gobEncode(messagesRecord{V: Version + 1})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []types.Message
	if err := Decode(raw, &msgs); !errors.Is(err, ErrVersion) {
		t.Fatalf("err = %v", err)
	}
}

func TestOtherValuesArePlainGob(t *testing.T) {
	in := types.BudgetReceipt{ID: "r", Model: "m", Uncertain: true}
	raw, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	var plain types.BudgetReceipt
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&plain); err != nil || !reflect.DeepEqual(plain, in) {
		t.Fatalf("plain gob = %+v, %v", plain, err)
	}
}
