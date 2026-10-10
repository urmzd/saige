package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

// checkGolden compares got with testdata/name, rewriting it with -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; review the change and run go test -run %s -update\ngot:\n%s", name, t.Name(), got)
	}
}

// rawChat records the raw body of every chat request and replies with a
// finished, empty stream.
func rawChat(t *testing.T) (*httptest.Server, *[]byte, *atomic.Int32) {
	t.Helper()
	var body []byte
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(ChatChunk{Done: true})
	}))
	t.Cleanup(server.Close)
	return server, &body, &hits
}

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\nfake")
	jpegBytes = []byte("\xff\xd8\xfffake")
)

// TestRequestMappingGolden covers every request row of the Ollama mapping:
// text, images, tool results (text, JSON and images), thinking and tool
// calls replayed, and a schema sent with options.
func TestRequestMappingGolden(t *testing.T) {
	weather := []types.ToolDef{{Name: "get_weather", Description: "Get weather",
		Parameters: types.ParameterSchema{Type: "object", Required: []string{"city"},
			Properties: map[string]types.PropertyDef{"city": {Type: "string"}}}}}
	call := types.ToolCallPart{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"city": "Paris"}}
	jsonOut, err := types.JSON(map[string]any{"temp_c": 18})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		req   types.Request
		model string
	}{
		{name: "text", req: types.Request{Messages: []types.Message{
			types.SystemMsg(types.Text("Be brief.")),
			types.UserMsg(types.Text("Hello "), types.Text("there")),
		}}},
		{name: "image", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("What color?"),
				types.Image(types.Bytes(types.MediaPNG, pngBytes)),
				types.Image(types.Bytes(types.MediaJPEG, jpegBytes))),
		}}},
		{name: "image_inline_and_uri", req: types.Request{Messages: []types.Message{
			// Inline bytes win; the URI is not fetched.
			types.UserMsg(types.Image(types.Bytes(types.MediaPNG, pngBytes).With(types.URL("https://example.com/a.png", types.MediaPNG)))),
		}}},
		{name: "tool_call_and_result", req: types.Request{Tools: weather, Messages: []types.Message{
			types.UserMsg(types.Text("Weather in Paris?")),
			types.AssistantMsg(call),
			types.ToolResults(types.ToolOK("call_1", types.Text("sunny"), jsonOut)),
		}}},
		{name: "tool_error", req: types.Request{Tools: weather, Messages: []types.Message{
			types.UserMsg(types.Text("Weather in Paris?")),
			types.AssistantMsg(call),
			types.UserToolResults(types.ToolErr("call_1", "upstream down")),
		}}},
		{name: "tool_result_image", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("Fetch the photo.")),
			types.AssistantMsg(types.ToolCallPart{ID: "call_2", Name: "get_photo"}),
			types.ToolResults(types.ToolOK("call_2", types.Text("photo attached"), types.Image(types.Bytes(types.MediaPNG, pngBytes)))),
		}}},
		{name: "thinking_replay", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("2+2?")),
			types.AssistantMsg(
				types.ThinkingPart{Text: "Add the numbers.", Signature: "sig-from-elsewhere"},
				types.ThinkingPart{Redacted: true, Signature: "opaque"},
				types.Text("4")),
			types.UserMsg(types.Text("And 3+3?")),
		}}},
		{name: "assistant_refusal_and_citation", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.Text("Per the source, "), types.CitationPart{Citation: types.Citation{URI: "https://example.com"}},
				types.RefusalPart{Text: "I cannot say more."}),
		}}},
		{name: "metadata_not_sent", req: types.Request{Messages: []types.Message{
			types.SystemMsg(types.Text("sys"), types.ConfigPart{}),
			types.UserMsg(types.Text("hi"), types.SteerPart{}),
			types.AssistantMsg(types.Text("hello"), types.RoutePart{}),
		}}},
		{name: "schema_and_options", model: "qwen3.5:4b", req: types.Request{
			Tools:    append(weather, types.ToolDef{Name: "other", Parameters: types.ParameterSchema{Type: "object"}}),
			Messages: []types.Message{types.UserMsg(types.Text("Weather in Paris as JSON."))},
			Schema: &types.ParameterSchema{Type: "object", Required: []string{"temp_c"},
				Properties: map[string]types.PropertyDef{"temp_c": {Type: "number"}}},
			Options: &types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "get_weather"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, body, _ := rawChat(t)
			model := tc.model
			if model == "" {
				model = "qwen3.5:4b"
			}
			ch, err := NewAdapter(NewClient(server.URL, model, "")).Stream(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			var all []types.Delta
			for d := range ch {
				all = append(all, d)
			}
			streamcheck.RunPartConformance(t, all)
			var out bytes.Buffer
			if err := json.Indent(&out, *body, "", "  "); err != nil {
				t.Fatal(err)
			}
			out.WriteByte('\n')
			checkGolden(t, filepath.Join("requests", tc.name+".json"), out.Bytes())
		})
	}
}

// TestRequestRejections checks that a part the chat API cannot carry fails
// before any request, with the sentinel and the part's path, kind and media
// type in the error.
func TestRequestRejections(t *testing.T) {
	pdf := types.Bytes(types.MediaPDF, []byte("%PDF-1.4"))
	for _, tc := range []struct {
		name     string
		msgs     []types.Message
		sentinel error
		want     string
	}{
		{"document", []types.Message{types.UserMsg(types.Text("x"), types.Document(pdf))},
			types.ErrModalityUnsupported, "part 0.1 (document application/pdf)"},
		{"audio", []types.Message{types.UserMsg(types.Audio(types.Bytes("audio/wav", []byte("RIFF"))))},
			types.ErrModalityUnsupported, "part 0.0 (audio audio/wav)"},
		{"video", []types.Message{types.UserMsg(types.Video(types.Bytes("video/mp4", []byte("mp4"))))},
			types.ErrModalityUnsupported, "part 0.0 (video video/mp4)"},
		{"file", []types.Message{types.UserMsg(types.File(types.Bytes("application/zip", []byte("PK"))))},
			types.ErrModalityUnsupported, "part 0.0 (file application/zip)"},
		{"image by URL", []types.Message{types.UserMsg(types.Image(types.URL("https://example.com/a.png", types.MediaPNG)))},
			types.ErrModalityUnsupported, "inline image bytes only"},
		{"image by workspace ref", []types.Message{types.UserMsg(types.Image(types.Artifact(types.ArtifactScheme+"abc", types.MediaPNG)))},
			types.ErrModalityUnsupported, "inline image bytes only"},
		{"image by vendor file", []types.Message{types.UserMsg(types.Image(types.VendorFileID("anthropic", "", "file_1", types.MediaPNG)))},
			types.ErrModalityUnsupported, "inline image bytes only"},
		{"image webp", []types.Message{types.UserMsg(types.Image(types.Bytes(types.MediaWebP, []byte("RIFF"))))},
			types.ErrModalityUnsupported, "JPEG and PNG only"},
		{"image elided", []types.Message{types.UserMsg(types.Image(types.Source{MediaType: types.MediaPNG, Digest: "9f"}))},
			types.ErrMediaUnavailable, "part 0.0 (image image/png)"},
		{"image unresolved", []types.Message{types.UserMsg(types.Image(types.URL("file:///gone.png", types.MediaPNG).Unavailable("not found")))},
			types.ErrMediaUnavailable, "not found"},
		{"tool result document", []types.Message{
			types.UserMsg(types.Text("x")),
			types.AssistantMsg(types.ToolCallPart{ID: "c", Name: "t"}),
			types.ToolResults(types.ToolOK("c", types.Text("ok"), types.Document(pdf))),
		}, types.ErrModalityUnsupported, "part 2.0.1 (document application/pdf)"},
		{"tool result image by URL", []types.Message{
			types.ToolResults(types.ToolOK("c", types.Image(types.URL("https://example.com/a.png", types.MediaPNG)))),
		}, types.ErrModalityUnsupported, "part 0.0.0 (image image/png)"},
		{"server tool call", []types.Message{types.AssistantMsg(types.ServerToolCallPart{ID: "s", Name: "web_search"})},
			types.ErrModalityUnsupported, "part 0.0 (server_tool_call)"},
		{"server tool result", []types.Message{types.AssistantMsg(types.Text("a"), types.ServerToolResultPart{CallID: "s"})},
			types.ErrModalityUnsupported, "part 0.1 (server_tool_result)"},
		{"audio out", []types.Message{types.AssistantMsg(types.AudioOutPart{Source: types.Bytes("audio/wav", []byte("RIFF"))})},
			types.ErrModalityUnsupported, "part 0.0 (audio_out audio/wav)"},
		{"image out", []types.Message{types.AssistantMsg(types.ImageOutPart{Source: types.Bytes(types.MediaPNG, pngBytes)})},
			types.ErrModalityUnsupported, "part 0.0 (image_out image/png)"},
		{"video out", []types.Message{types.AssistantMsg(types.VideoOutPart{Source: types.Bytes("video/mp4", []byte("mp4"))})},
			types.ErrModalityUnsupported, "part 0.0 (video_out video/mp4)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _, hits := rawChat(t)
			_, err := NewAdapter(NewClient(server.URL, "test-model", "")).Stream(context.Background(), types.Request{Messages: tc.msgs})
			if !errors.Is(err, tc.sentinel) || !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("err = %v, want %v", err, tc.sentinel)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.want)
			}
			var pe *types.ProviderError
			if !errors.As(err, &pe) || pe.Kind != types.ErrorKindPermanent || types.IsTransient(err) {
				t.Fatalf("err = %#v, want a permanent provider error", err)
			}
			if hits.Load() != 0 {
				t.Fatal("a rejected request reached the server")
			}
		})
	}
}

// renderDelta is a stable one-line form of a delta for stream goldens.
func renderDelta(d types.Delta, ids map[string]string) string {
	id := func(s string) string {
		if r, ok := ids[s]; ok {
			return r
		}
		return s
	}
	switch v := d.(type) {
	case types.PartStart:
		s := fmt.Sprintf("start %d %s", v.Index, v.Kind)
		if v.ID != "" {
			s += fmt.Sprintf(" id=%s name=%s", id(v.ID), v.Name)
		}
		return s
	case types.PartDelta:
		for _, f := range []struct{ k, v string }{{"text", v.Text}, {"thinking", v.Thinking}, {"signature", v.Signature}, {"args", v.Args}} {
			if f.v != "" {
				return fmt.Sprintf("delta %d %s=%q", v.Index, f.k, f.v)
			}
		}
		return fmt.Sprintf("delta %d", v.Index)
	case types.PartEnd:
		if v.Part == nil {
			return fmt.Sprintf("end %d", v.Index)
		}
		if tc, ok := v.Part.(types.ToolCallPart); ok {
			tc.ID = id(tc.ID)
			v.Part = tc
		}
		b, _ := types.MarshalPart(v.Part)
		return fmt.Sprintf("end %d %s", v.Index, b)
	case types.UsageDelta:
		return fmt.Sprintf("usage prompt=%d completion=%d finish=%v", v.PromptTokens, v.CompletionTokens, v.FinishReasons)
	case types.ErrorDelta:
		return "error " + v.Error.Error()
	default:
		return fmt.Sprintf("%T", d)
	}
}

// TestStreamFixtures replays recorded chat streams and checks the part
// deltas (sequential indices) and the assembled parts against goldens.
func TestStreamFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "streams", "*.ndjson"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no stream fixtures: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".ndjson")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			server := lineServer(t, 0, false, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
			ch, err := NewAdapter(NewClient(server.URL, "qwen3.5:4b", "")).Stream(context.Background(),
				types.Request{Messages: []types.Message{types.UserMsg(types.Text("hi"))}})
			if err != nil {
				t.Fatal(err)
			}
			var all []types.Delta
			asm := types.NewPartAssembler()
			for d := range ch {
				all = append(all, d)
				asm.Push(d)
			}
			streamcheck.RunPartConformance(t, all)
			if asm.Violations() != 0 {
				t.Fatalf("assembler violations: %d", asm.Violations())
			}
			// A call without a server ID gets a generated one; name it
			// stably for the golden.
			ids := map[string]string{}
			for _, d := range all {
				if s, ok := d.(types.PartStart); ok && s.ID != "" && !strings.HasPrefix(s.ID, "call_") {
					ids[s.ID] = fmt.Sprintf("generated-%d", len(ids))
				}
			}
			var out strings.Builder
			out.WriteString("# deltas\n")
			for _, d := range all {
				out.WriteString(renderDelta(d, ids) + "\n")
			}
			out.WriteString("# parts\n")
			for _, p := range asm.Parts() {
				if tc, ok := p.(types.ToolCallPart); ok {
					if r, ok := ids[tc.ID]; ok {
						tc.ID = r
					}
					p = tc
				}
				b, err := types.MarshalPart(p)
				if err != nil {
					t.Fatal(err)
				}
				out.WriteString(string(b) + "\n")
			}
			checkGolden(t, filepath.Join("streams", name+".golden"), []byte(out.String()))
		})
	}
}
