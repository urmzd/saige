package durablecodec

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// The gob blobs in this directory were written by the release before typed
// parts (see its fixturegen directory).
const legacyGob = "../testdata/legacy/durable/gob/"

var png = []byte("\x89PNG\r\n\x1a\nfixture-image")

func readBlob(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(legacyGob + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The messages the blobs hold, upgraded. Unlike a stored tree, a journal
// kept media bytes, so they survive the upgrade.
func legacySystem() types.SystemMessage {
	return types.SystemMsg(
		types.Text("You are terse."),
		types.ConfigPart{Target: types.ModelTarget("gpt-x"), MaxIter: 3,
			Compact:    &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 10, KeepLast: 2},
			ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceAuto},
			Dials:      &types.Dials{Creativity: ptr(types.CreativityFocused), MaxOutput: ptr(int64(512))}, Reason: "fixture"},
		types.HandoffPart{To: "b", From: "a", Reason: "r", Message: "m", Context: "c"},
		types.RoutePart{Profile: "fast", Provider: "openai", Model: "gpt-x", Reason: "primary", Preset: "p", ConfigHash: "h",
			CatalogRevision: "rev", Options: &types.RequestOptions{Temperature: ptr(0.2), MaxOutputTokens: ptr(int64(100))}},
		types.ApprovalPart{Event: types.ApprovalEventGranted, Tool: "write", ToolCallID: "call_w",
			Grant: &types.Grant{ID: "g1", Scope: types.GrantScope("session")}, Approver: "ops", Reason: "ok"},
		types.GuardrailPart{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionBlock, Reason: "ssn"},
		types.CompactionPart{Strategy: "summary", Steps: []string{"summary"}, Trigger: types.CompactionTriggerRule, TokensBefore: 100,
			TokensAfter: 10, FromBranch: "main", Kept: []types.NodeID{"k"}, SummaryNode: "s"},
	)
}

func richOutput() []types.ToolOutputPart {
	return []types.ToolOutputPart{
		types.Text("chart attached"),
		types.Image(types.Bytes(types.MediaPNG, png)),
		types.Image(types.Source{MediaType: types.MediaPNG, URI: "https://example.com/chart.png", Filename: "chart.png"}),
		types.Document(types.Source{MediaType: types.MediaPDF, URI: "s3://bucket/report.pdf", Filename: "report.pdf"}),
		types.JSONPart{JSON: json.RawMessage(`{"rows":2}`)},
	}
}

func legacyToolResults() types.SystemMessage {
	return types.ToolResults(
		types.ToolOK("call_plain", types.Text("plain result")),
		types.ToolErr("call_err", "boom"),
		types.ToolOK("call_rich", richOutput()...),
		types.ToolOK("call_media_only", types.Text("see image"), types.Image(types.Bytes(types.MediaPNG, png))),
		types.ToolResultPart{CallID: "call_cited", Parts: []types.ToolOutputPart{types.Text("sourced")},
			Citations: []types.Citation{types.NewCitation(types.CitationWeb, "https://example.com", "Example")}, ToolVersion: "2"},
	)
}

func legacyUser() types.UserMessage {
	pdf := types.Bytes(types.MediaPDF, []byte("%PDF-1.7 fixture"))
	pdf.Filename = "inline.pdf"
	return types.UserMsg(
		types.Text("Describe these."),
		types.Image(types.Source{URI: "https://example.com/a.png", MediaType: types.MediaPNG, Filename: "a.png"}),
		types.Document(pdf),
		types.Video(types.Source{URI: "gs://bucket/clip.mp4", MediaType: types.MediaMP4}),
		types.SteerPart{ID: "sub-1"},
		types.GuardrailPart{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionRewrite},
		types.ConfigPart{MaxIter: 2},
		types.HandoffPart{To: "a"},
	)
}

func legacyAssistant() types.AssistantMessage {
	return types.AssistantMsg(
		types.ThinkingPart{Text: "plan", Signature: "sig-1"},
		types.Text("Looking."),
		types.ServerToolCallPart{ID: "srv_1", ToolKind: types.ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go", "max": 3}},
		types.ServerToolResultPart{CallID: "srv_1", ToolKind: types.ServerToolWebSearch, Text: "results",
			Result:  json.RawMessage(`{"hits":[{"url":"https://go.dev"}]}`),
			Outputs: []types.Part{types.Image(types.Source{URI: "https://example.com/chart.png", MediaType: types.MediaPNG})}},
		types.ServerToolCallPart{ID: "srv_2", ToolKind: types.ServerToolCodeExecution, Name: "code_execution", Input: map[string]any{"code": "1+1"}},
		types.ToolCallPart{ID: "call_rich", Name: "snapshot", Arguments: map[string]any{"n": 3, "big": 12345678901234, "f": 1.5,
			"list": []any{1, map[string]any{"k": "v"}}}},
		types.ToolCallPart{ID: "call_bad", Name: "snapshot", ArgumentsError: "unexpected end of JSON input"},
		types.RoutePart{Provider: "anthropic", Model: "claude-x"},
		types.GuardrailPart{Guardrail: "tone", Phase: types.GuardrailPhaseOutput, Action: types.GuardrailActionPass},
		types.TruncationPart{Reason: "max_tokens"},
	)
}

func TestLegacyInputMessages(t *testing.T) {
	var got []types.Message
	if err := Decode(readBlob(t, "input_messages.gob"), &got); err != nil {
		t.Fatal(err)
	}
	want := []types.Message{legacySystem(), legacyUser(), legacyAssistant(), legacyToolResults(),
		types.UserToolResults(types.ToolOK("call_human", types.Text("human answer")))}
	if len(got) != len(want) {
		t.Fatalf("decoded %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("message %d =\n%#v\nwant\n%#v", i, got[i], want[i])
		}
	}
	// Re-recorded in the current format, the upgraded input reads back the
	// same, so an engine comparing a resumed run's input sees no change.
	raw, err := Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	var again []types.Message
	if err := Decode(raw, &again); err != nil {
		t.Fatal(err)
	}
	if len(again) != len(got) {
		t.Fatalf("re-recorded %d messages", len(again))
	}
}

func TestLegacyFinalMessage(t *testing.T) {
	var got types.AssistantMessage
	if err := Decode(readBlob(t, "final_message.gob"), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, legacyAssistant()) {
		t.Fatalf("final =\n%#v\nwant\n%#v", got, legacyAssistant())
	}
}

func TestLegacySteps(t *testing.T) {
	asst := legacyAssistant()
	user := legacyUser()
	tests := []struct {
		blob string
		want types.StepResult
	}{
		{"step_llm.gob", types.StepResult{Kind: types.StepKindLLM, Message: &asst,
			Usage:   &types.UsageDelta{AccountingID: "acct", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			Receipt: &types.BudgetReceipt{ID: "r1", Model: "gpt-x", Usage: types.TokenUsage{InputTokens: 10, OutputTokens: 5}}}},
		{"step_tool.gob", types.StepResult{Kind: types.StepKindTool, ToolCallID: "call_rich", ToolResult: "chart attached", ToolParts: richOutput()}},
		{"step_tool_error.gob", types.StepResult{Kind: types.StepKindTool, ToolCallID: "call_err", ToolError: "boom"}},
		{"step_approval.gob", types.StepResult{Kind: types.StepKindApproval, Approval: &types.ApprovalVerdict{Outcome: types.VerdictAsk, Reason: "needs review"}}},
		{"step_hook.gob", types.StepResult{Kind: types.StepKindHook, Hook: &types.HookRecord{Name: "redact", Changed: true, Message: &user,
			Arguments: map[string]any{"q": "x", "n": 2}, Text: "t", Action: types.GuardrailActionRewrite}}},
	}
	for _, tt := range tests {
		t.Run(tt.blob, func(t *testing.T) {
			var got types.StepResult
			if err := Decode(readBlob(t, tt.blob), &got); err != nil {
				t.Fatal(err)
			}
			if got.V != 1 {
				t.Fatalf("version = %d, want 1", got.V)
			}
			tt.want.V = 1
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("step =\n%#v\nwant\n%#v", got, tt.want)
			}
			// An upgraded step records in the current format.
			raw, err := Encode(got)
			if err != nil {
				t.Fatal(err)
			}
			var again types.StepResult
			if err := Decode(raw, &again); err != nil || again.V != Version {
				t.Fatalf("re-recorded = %d, %v", again.V, err)
			}
		})
	}
}
