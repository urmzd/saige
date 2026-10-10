package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// replayFixture streams testdata/streams/<name>.sse, a recorded (or, for
// shapes hard to provoke live, hand-written) Messages API stream, through the
// adapter. It checks part conformance and returns the assembled turn, the
// deltas and the stream's error.
func replayFixture(t *testing.T, name string, schema *types.ParameterSchema) ([]types.AssistantPart, []types.Delta, error) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "streams", name+".sse"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL)))
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("q"))}, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	asm := types.NewPartAssembler()
	var deltas []types.Delta
	var streamErr error
	for d := range ch {
		deltas = append(deltas, d)
		asm.Push(d)
		if e, ok := d.(types.ErrorDelta); ok {
			streamErr = e.Error
		}
	}
	streamcheck.RunPartConformance(t, deltas)
	if n := asm.Violations(); n != 0 {
		t.Fatalf("assembler violations = %d", n)
	}
	return asm.Parts(), deltas, streamErr
}

func kinds(parts []types.AssistantPart) []types.PartKind {
	out := make([]types.PartKind, len(parts))
	for i, p := range parts {
		out[i] = p.Kind()
	}
	return out
}

func wantKinds(t *testing.T, parts []types.AssistantPart, want ...types.PartKind) {
	t.Helper()
	got := kinds(parts)
	if len(got) != len(want) {
		t.Fatalf("parts = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("parts = %v, want %v", got, want)
		}
	}
}

// TestStreamFixtures replays recorded streams for an image, a PDF, a
// citation-enabled text and PDF document, a tool call and the answer to a
// tool result carrying an image, and thinking; and hand-written streams for
// redacted thinking, a refusal and web search citations.
func TestStreamFixtures(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		parts, _, err := replayFixture(t, "image", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindText)
		if parts[0].(types.TextPart).Text != "Red" {
			t.Fatalf("text = %+v", parts[0])
		}
	})
	t.Run("pdf", func(t *testing.T) {
		parts, _, err := replayFixture(t, "pdf", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindText)
	})
	t.Run("text document citations", func(t *testing.T) {
		parts, deltas, err := replayFixture(t, "citations", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindText, types.KindText, types.KindCitation)
		c := parts[2].(types.CitationPart)
		if c.Anchor == nil || *c.Anchor != (types.Anchor{PartIndex: 1, Start: 0, End: len(parts[1].(types.TextPart).Text)}) {
			t.Fatalf("anchor = %+v, want the whole second text part", c.Anchor)
		}
		if c.Citation.Kind != types.CitationDocument || c.Citation.Title != "Field notes" || c.Citation.Quote != "The sky is blue. " ||
			c.Citation.Producer != "anthropic" || c.Citation.Meta["location"] != "char_location" ||
			c.Citation.Meta["start_char_index"] != 20 || c.Citation.Meta["end_char_index"] != 37 || c.Citation.Meta["document_index"] != 0 {
			t.Fatalf("citation = %+v", c.Citation)
		}
		// Content parts take their content block index; the citation
		// follows them.
		for _, d := range deltas {
			if s, ok := d.(types.PartStart); ok && s.Kind == types.KindCitation && s.Index != 2 {
				t.Fatalf("citation index = %d, want 2", s.Index)
			}
		}
	})
	t.Run("pdf citations", func(t *testing.T) {
		parts, _, err := replayFixture(t, "pdf_citations", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindText, types.KindText, types.KindCitation)
		c := parts[2].(types.CitationPart).Citation
		if c.Meta["location"] != "page_location" || c.Meta["start_page_number"] != 1 || c.Meta["end_page_number"] != 2 || c.Title != "Memo" {
			t.Fatalf("citation = %+v", c)
		}
	})
	t.Run("tool call", func(t *testing.T) {
		parts, _, err := replayFixture(t, "tool_image_call", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindToolCall)
		if tc := parts[0].(types.ToolCallPart); tc.Name != "get_swatch" || tc.Arguments == nil || tc.ID == "" {
			t.Fatalf("call = %+v", tc)
		}
	})
	t.Run("answer after an image tool result", func(t *testing.T) {
		parts, _, err := replayFixture(t, "tool_image_answer", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindThinking, types.KindText)
		if th := parts[0].(types.ThinkingPart); th.Signature == "" {
			t.Fatalf("thinking = %+v, want a signature", th)
		}
	})
	t.Run("thinking signature", func(t *testing.T) {
		parts, _, err := replayFixture(t, "thinking", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindThinking, types.KindText)
		if parts[0].(types.ThinkingPart).Signature == "" {
			t.Fatal("signature not assembled")
		}
	})
	t.Run("redacted thinking", func(t *testing.T) {
		parts, _, err := replayFixture(t, "redacted_thinking", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindThinking, types.KindThinking, types.KindText)
		if th := parts[0].(types.ThinkingPart); th.Text != "Weighing it." || th.Signature != "c2lnLTE=" || th.Redacted {
			t.Fatalf("thinking = %+v", th)
		}
		if th := parts[1].(types.ThinkingPart); !th.Redacted || th.Signature != "RU5DUllQVEVE" || th.Text != "" {
			t.Fatalf("redacted = %+v", th)
		}
	})
	t.Run("refusal", func(t *testing.T) {
		parts, _, err := replayFixture(t, "refusal", nil)
		if !types.IsContentFilter(err) {
			t.Fatalf("err = %v, want a content-filter error", err)
		}
		wantKinds(t, parts, types.KindText, types.KindRefusal)
		if r := parts[1].(types.RefusalPart); r.Category != "cyber" || r.Text != "This request falls under a restricted category." {
			t.Fatalf("refusal = %+v", r)
		}
	})
	t.Run("web search citations", func(t *testing.T) {
		parts, _, err := replayFixture(t, "web_citations", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds(t, parts, types.KindServerToolCall, types.KindServerToolResult, types.KindText, types.KindCitation)
		if call := parts[0].(types.ServerToolCallPart); call.ToolKind != types.ServerToolWebSearch || call.Input["query"] != "go release" {
			t.Fatalf("call = %+v", call)
		}
		if res := parts[1].(types.ServerToolResultPart); res.CallID != "srvtoolu_9" || len(res.Result) == 0 {
			t.Fatalf("result = %+v", res)
		}
		c := parts[3].(types.CitationPart)
		if c.Citation.Kind != types.CitationWeb || c.Citation.URI != "https://go.dev/doc/devel/release" || c.Anchor.PartIndex != 2 ||
			c.Citation.Meta["encrypted_index"] != "Eo8BCi" {
			t.Fatalf("citation = %+v anchor %+v", c.Citation, c.Anchor)
		}
	})
}

// TestBatchMessageParts checks that a complete message maps to the same
// parts the stream assembles: redacted thinking, server tool results with
// code execution files, anchored citations, and a refusal.
func TestBatchMessageParts(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"refusal",
	"stop_details":{"type":"refusal","category":"bio","explanation":"declined"},
	"content":[
	 {"type":"redacted_thinking","data":"ENC"},
	 {"type":"server_tool_use","id":"srvtoolu_1","name":"bash_code_execution","input":{"command":"ls"}},
	 {"type":"bash_code_execution_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"bash_code_execution_result","stdout":"out.png","stderr":"","return_code":0,"content":[{"type":"bash_code_execution_output","file_id":"file_out"}]}},
	 {"type":"text","text":"Cited.","citations":[{"type":"content_block_location","cited_text":"x","document_index":1,"document_title":"T","start_block_index":0,"end_block_index":2}]}],
	"usage":{"input_tokens":1,"output_tokens":1}}`
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithEndpoint("ws1")))
	msg, err := a.assistantFromMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, msg.Parts, types.KindThinking, types.KindServerToolCall, types.KindServerToolResult, types.KindText, types.KindCitation, types.KindRefusal)
	if th := msg.Parts[0].(types.ThinkingPart); !th.Redacted || th.Signature != "ENC" {
		t.Fatalf("thinking = %+v", th)
	}
	res := msg.Parts[2].(types.ServerToolResultPart)
	if res.ToolKind != types.ServerToolCodeExecution || res.Text != "out.png" || len(res.Outputs) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if f, ok := res.Outputs[0].(types.FilePart).Source.VendorFile("anthropic", "ws1"); !ok || f.ID != "file_out" || f.Endpoint != "ws1" {
		t.Fatalf("output file = %+v", res.Outputs[0])
	}
	c := msg.Parts[4].(types.CitationPart)
	if *c.Anchor != (types.Anchor{PartIndex: 3, Start: 0, End: 6}) || c.Citation.Meta["start_block_index"] != 0 || c.Citation.Meta["end_block_index"] != 2 {
		t.Fatalf("citation = %+v anchor %+v", c.Citation, c.Anchor)
	}
	if r := msg.Parts[5].(types.RefusalPart); r.Category != "bio" || r.Text != "declined" {
		t.Fatalf("refusal = %+v", r)
	}
}
