package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite the request goldens in testdata/requests")

// toParams maps messages with a bare adapter and fails on an error.
func toParams(msgs []types.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	system, out, err := (&Adapter{}).toAnthropicParams(msgs)
	if err != nil {
		panic(err)
	}
	return system, out
}

var (
	pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	pdfBytes = []byte("%PDF-1.4 tiny")
)

// TestRequestGoldens checks the wire form of every Anthropic request row of
// the per-adapter mapping: text and system, images and documents by each
// locator, container uploads, tool results with media, and the replay of
// thinking, tool calls and server tool calls.
func TestRequestGoldens(t *testing.T) {
	webSearch := WithServerTools(types.ServerTool{Kind: types.ServerToolWebSearch})
	codeExec := WithServerTools(types.ServerTool{Kind: types.ServerToolCodeExecution})
	serverTurn := types.AssistantMsg(
		types.ServerToolCallPart{ID: "srvtoolu_1", ToolKind: types.ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go"}},
		types.ServerToolResultPart{CallID: "srvtoolu_1", ToolKind: types.ServerToolWebSearch, Text: "Go https://go.dev",
			Result: json.RawMessage(`[{"type":"web_search_result","url":"https://go.dev","title":"Go","encrypted_content":"abc"}]`)},
		types.Text("Go is at go.dev."),
	)
	for _, tc := range []struct {
		name string
		opts []Option
		msgs []types.Message
	}{
		{name: "text_system", msgs: []types.Message{types.SystemMsg(types.Text("be brief")), types.UserMsg(types.Text("hi"))}},
		{name: "image_base64", msgs: []types.Message{types.UserMsg(types.Text("what is this"), types.Image(types.Bytes(types.MediaPNG, pngBytes)))}},
		{name: "image_url", msgs: []types.Message{types.UserMsg(types.Image(types.URL("https://example.com/a.png", types.MediaPNG)))}},
		{name: "image_file", opts: []Option{WithEndpoint("ws1")}, msgs: []types.Message{types.UserMsg(types.Image(
			types.VendorFileID("anthropic", "ws1", "file_img", types.MediaPNG).With(types.URL("https://example.com/a.png"))))}},
		{name: "image_file_other_scope_falls_back", opts: []Option{WithEndpoint("ws1")}, msgs: []types.Message{types.UserMsg(types.Image(
			types.Bytes(types.MediaPNG, pngBytes).With(types.VendorFileID("anthropic", "ws2", "file_other", types.MediaPNG))))}},
		{name: "image_charset_param", msgs: []types.Message{types.UserMsg(types.Image(types.Bytes("IMAGE/PNG; q=1", pngBytes)))}},
		{name: "document_pdf_base64", msgs: []types.Message{types.UserMsg(types.Document(types.Bytes(types.MediaPDF, pdfBytes)))}},
		{name: "document_pdf_url", msgs: []types.Message{types.UserMsg(types.Document(types.URL("https://example.com/a.pdf", types.MediaPDF)))}},
		{name: "document_pdf_file", msgs: []types.Message{types.UserMsg(types.Document(types.VendorFileID("anthropic", "", "file_pdf", types.MediaPDF)))}},
		{name: "document_text_inline", msgs: []types.Message{types.UserMsg(types.Document(types.Bytes("text/plain; charset=utf-8", []byte("The sky is blue."))))}},
		{name: "document_text_file", msgs: []types.Message{types.UserMsg(types.Document(types.VendorFileID("anthropic", "", "file_txt", types.MediaText)))}},
		{name: "document_meta", msgs: []types.Message{types.UserMsg(types.Document(types.Bytes(types.MediaText, []byte("The sky is blue.")),
			types.DocumentMeta{Title: "Sky", Context: "a field note", Citations: true}))}},
		{name: "file_container_upload", opts: []Option{codeExec}, msgs: []types.Message{types.UserMsg(types.Text("analyze"),
			types.File(types.VendorFileID("anthropic", "", "file_csv", types.MediaCSV)))}},
		{name: "tool_result_text", msgs: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.ToolCallPart{ID: "toolu_1", Name: "lookup", Arguments: map[string]any{"q": "x"}}),
			types.ToolResults(types.ToolOK("toolu_1", types.Text("plain")))}},
		{name: "tool_result_media", msgs: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.ToolCallPart{ID: "toolu_1", Name: "render", Arguments: map[string]any{}}),
			types.ToolResults(types.ToolResultPart{CallID: "toolu_1", IsError: false, Parts: []types.ToolOutputPart{
				types.Text("rendered"), types.JSONPart{JSON: []byte(`{"n":1}`)},
				types.Image(types.Bytes(types.MediaPNG, pngBytes)), types.Document(types.Bytes(types.MediaPDF, pdfBytes))}})}},
		{name: "thinking_replay", msgs: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.ThinkingPart{Text: "hmm", Signature: "sig1"}, types.ThinkingPart{Redacted: true, Signature: "opaque"}, types.Text("answer")),
			types.UserMsg(types.Text("more"))}},
		{name: "server_tool_replay", opts: []Option{webSearch}, msgs: []types.Message{
			types.UserMsg(types.Text("where is go")), serverTurn, types.UserMsg(types.Text("thanks"))}},
		{name: "server_tool_not_offered", msgs: []types.Message{
			types.UserMsg(types.Text("where is go")), serverTurn, types.UserMsg(types.Text("thanks"))}},
		{name: "annotations_not_replayed", msgs: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.Text("cited"), types.CitationPart{Citation: types.NewCitation(types.CitationDocument, "", "Sky")},
				types.RefusalPart{Text: "no"}),
			types.UserMsg(types.Text("ok"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("k", testModel, tc.opts...)
			system, msgs, err := a.toAnthropicParams(tc.msgs)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(struct {
				System   []anthropic.TextBlockParam `json:"system,omitempty"`
				Messages []anthropic.MessageParam   `json:"messages"`
			}{system, msgs}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			// Round-trip through a generic value so the golden is stable.
			var v any
			_ = json.Unmarshal(got, &v)
			got, _ = json.MarshalIndent(v, "", "  ")
			got = append(got, '\n')
			path := filepath.Join("testdata", "requests", tc.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -run TestRequestGoldens -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("request differs from %s:\n%s", path, got)
			}
		})
	}
}

// TestUnsupportedPartsAreRejected checks that a part the API cannot take
// fails the request before any network I/O, with an error that names the
// part and matches the right sentinel; nothing is dropped or replaced.
func TestUnsupportedPartsAreRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a rejected request reached the API")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	user := func(p ...types.UserPart) []types.Message {
		return []types.Message{types.UserMsg(append([]types.UserPart{types.Text("look")}, p...)...)}
	}
	ref := types.Artifact(types.ArtifactScheme+"abc", types.MediaPNG)
	for _, tc := range []struct {
		name     string
		opts     []Option
		msgs     []types.Message
		sentinel error
		want     string
	}{
		{"audio", nil, user(types.Audio(types.Bytes(types.MediaWAV, []byte("RIFF")))), types.ErrModalityUnsupported, "part 0.1 (audio audio/wav)"},
		{"video", nil, user(types.Video(types.URL("https://example.com/v.mp4", types.MediaMP4))), types.ErrModalityUnsupported, "part 0.1 (video video/mp4)"},
		{"image type", nil, user(types.Image(types.Bytes("image/bmp", []byte("BM")))), types.ErrModalityUnsupported, "JPEG, PNG, GIF and WebP"},
		{"inline image without type", nil, user(types.Image(types.Source{Inline: pngBytes})), types.ErrModalityUnsupported, "need a media type"},
		{"document type", nil, user(types.Document(types.Bytes(types.MediaCSV, []byte("a,b")))), types.ErrModalityUnsupported, "PDF and text/plain"},
		{"gs uri", nil, user(types.Image(types.URL("gs://b/a.png", types.MediaPNG))), types.ErrModalityUnsupported, "not gs: URIs"},
		{"http uri", nil, user(types.Document(types.URL("http://example.com/a.pdf", types.MediaPDF))), types.ErrModalityUnsupported, "not http: URIs"},
		{"text by url", nil, user(types.Document(types.URL("https://example.com/a.txt", types.MediaText))), types.ErrModalityUnsupported, "cannot be sent by URL"},
		{"invalid utf8 text", nil, user(types.Document(types.Bytes(types.MediaText, []byte{0xff, 0xfe}))), types.ErrModalityUnsupported, "UTF-8"},
		{"workspace ref only", nil, user(types.Image(ref)), types.ErrMediaUnavailable, "resolve the workspace reference"},
		{"elided", nil, user(types.Image(types.Source{MediaType: types.MediaPNG, Digest: "abc"})), types.ErrMediaUnavailable, "no locator"},
		{"unresolved", nil, user(types.Image(types.URL("https://example.com/a.png", types.MediaPNG).Unavailable("fetch failed: 404"))), types.ErrMediaUnavailable, "fetch failed: 404"},
		{"file from another endpoint", []Option{WithEndpoint("ws1")}, user(types.Image(types.VendorFileID("anthropic", "ws2", "file_x", types.MediaPNG))),
			types.ErrModalityUnsupported, `scoped to endpoint "ws2", not this adapter's "ws1"`},
		{"unscoped file to a scoped adapter", []Option{WithEndpoint("ws1")}, user(types.Image(types.VendorFileID("anthropic", "", "file_x", types.MediaPNG))),
			types.ErrModalityUnsupported, `scoped to endpoint ""`},
		{"scoped file to an unscoped adapter", nil, user(types.Document(types.VendorFileID("anthropic", "ws2", "file_x", types.MediaPDF))),
			types.ErrModalityUnsupported, `scoped to endpoint "ws2"`},
		{"expired file", nil, user(types.Image(types.Source{MediaType: types.MediaPNG, Files: []types.VendorFile{{Provider: "anthropic", ID: "file_x", ExpiresAt: time.Unix(1, 0)}}})),
			types.ErrMediaUnavailable, "expired"},
		{"another vendor's file", nil, user(types.Image(types.VendorFileID("openai", "", "file-x", types.MediaPNG))), types.ErrModalityUnsupported, "other providers only"},
		{"opaque file without code execution", nil, user(types.File(types.VendorFileID("anthropic", "", "file_x", types.MediaCSV))), types.ErrModalityUnsupported, "does not enable code execution"},
		{"opaque file by bytes", []Option{WithServerTools(types.ServerTool{Kind: types.ServerToolCodeExecution})}, user(types.File(types.Bytes(types.MediaCSV, []byte("a")))),
			types.ErrModalityUnsupported, "needs an Anthropic file ID"},
		{"tool result audio", nil, []types.Message{types.UserMsg(types.Text("q")), types.AssistantMsg(types.ToolCallPart{ID: "t1", Name: "rec"}),
			types.ToolResults(types.ToolOK("t1", types.Text("clip"), types.Audio(types.Bytes(types.MediaWAV, []byte("RIFF")))))},
			types.ErrModalityUnsupported, "part 2.0.1 (audio audio/wav)"},
		{"assistant generated image", nil, []types.Message{types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.ImageOutPart{Source: types.Bytes(types.MediaPNG, pngBytes)})},
			types.ErrModalityUnsupported, "part 1.0 (image_out image/png)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAdapter("k", testModel, append([]Option{WithBaseURL(server.URL)}, tc.opts...)...)
			_, err := a.Stream(context.Background(), types.Request{Messages: tc.msgs})
			if err == nil {
				t.Fatal("request was accepted")
			}
			if !errors.Is(err, tc.sentinel) || !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("err = %v, want %v matching ErrInvalidModelConfig", err, tc.sentinel)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			// The batch path maps parts the same way.
			_, err = a.batchParams(types.BatchRequest{CustomID: "r1", Messages: tc.msgs})
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("batch err = %v, want %v", err, tc.sentinel)
			}
		})
	}
}

// TestSchemaWithOptions checks that a request carrying a schema and options
// together applies both: the options reach the wire and the schema goes out
// as native structured output or the hidden tool, by model.
func TestSchemaWithOptions(t *testing.T) {
	schema := &types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"answer": {Type: "string"}}, Required: []string{"answer"}}
	maxOut := int64(321)
	for _, tc := range []struct {
		model  string
		native bool
	}{
		{"claude-sonnet-5-5", true},
		{"claude-haiku-5-5", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := NewAdapter("k", tc.model, WithBaseURL(server.URL))
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("q"))},
				Schema: schema, Options: &types.RequestOptions{StopSequences: []string{"END"}, MaxOutputTokens: &maxOut}})
			if err != nil {
				t.Fatal(err)
			}
			drain(ch)
			body := (*bodies)[0]
			if stop, _ := body["stop_sequences"].([]any); len(stop) != 1 || stop[0] != "END" || body["max_tokens"] != float64(321) {
				t.Fatalf("options not applied: stop_sequences=%v max_tokens=%v", body["stop_sequences"], body["max_tokens"])
			}
			format := body["output_config"]
			if tc.native {
				f, _ := format.(map[string]any)["format"].(map[string]any)
				if f["type"] != "json_schema" || f["schema"] == nil {
					t.Fatalf("output_config = %v, want a json_schema format", format)
				}
				return
			}
			if format != nil {
				t.Fatalf("output_config = %v, want none on a hidden-tool model", format)
			}
			if tc, _ := body["tool_choice"].(map[string]any); tc["name"] != structuredToolName {
				t.Fatalf("tool_choice = %v, want the hidden tool", body["tool_choice"])
			}
		})
	}
}
