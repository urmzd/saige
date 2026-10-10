package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Tiny stand-ins for media bytes. The mapping never decodes them.
var (
	pngBytes = []byte("\x89PNG fake")
	pdfBytes = []byte("%PDF-1.4 fake")
	wavBytes = []byte("RIFF fake")
)

// testAdapter builds an adapter for the Gemini API, or for Vertex AI, whose
// requests are recorded into bodies.
func testAdapter(t *testing.T, vertex bool, model string, bodies *[]map[string]any, opts ...Option) *Adapter {
	t.Helper()
	opts = append([]Option{WithHTTPClient(&http.Client{Transport: captureTransport{events: []string{doneEvent}, bodies: bodies}})}, opts...)
	if vertex {
		opts = append(opts, WithVertex("test-project", "us-central1"))
	}
	a, err := New(context.Background(), Config{APIKey: "k", Model: types.ModelID(model)}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// sendRequest streams req and returns the request body sent.
func sendRequest(t *testing.T, a *Adapter, req types.Request, bodies *[]map[string]any) map[string]any {
	t.Helper()
	ch, err := a.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatal(e.Error)
		}
	}
	if len(*bodies) != 1 {
		t.Fatalf("%d requests sent, want 1", len(*bodies))
	}
	return (*bodies)[0]
}

func checkGolden(t *testing.T, name string, body map[string]any) {
	t.Helper()
	got, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "requests", name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixed test path
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("request body differs from %s (run with -update after checking):\n%s", path, got)
	}
}

func sig(s string) string { return encodeSignature([]byte(s)) }

// TestRequestGoldens pins the wire body for each Google row of the part
// mapping: text and system, image, document, audio and video by every
// locator, tool results with media, assistant replay with signatures,
// server tools, generated media, a schema with options, and output
// modalities.
func TestRequestGoldens(t *testing.T) {
	videoClip := types.Video(types.URL("gs://bucket/clip.mp4", types.MediaMP4),
		types.VideoMeta{ClipStart: 2 * time.Second, ClipEnd: 7500 * time.Millisecond, FPS: 0.5})
	call := types.ToolCallPart{ID: "c1", Name: "take_photo", Arguments: map[string]any{"zoom": 2}}
	temp := 0.2
	for _, tc := range []struct {
		name   string
		vertex bool
		model  string
		req    types.Request
		opts   []Option
	}{
		{name: "text_system", req: types.Request{Messages: []types.Message{
			types.SystemMsg(types.Text("Be brief.")), types.UserMsg(types.Text("hi"))}}},
		{name: "image_inline", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("what is it"), types.Image(types.Bytes(types.MediaPNG, pngBytes)))}}},
		{name: "image_https", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Image(types.URL("https://example.com/a.png", types.MediaPNG).With(types.Bytes(types.MediaPNG, pngBytes))))}}},
		{name: "image_files_api", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Image(types.VendorFileID("google", "", "files/abc123", types.MediaJPEG)),
				types.Image(types.VendorFileID("google", "", "https://generativelanguage.googleapis.com/v1beta/files/def456", types.MediaWebP)))}}},
		{name: "image_gs_vertex", vertex: true, req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Image(types.URL("gs://bucket/a.png", types.MediaPNG)),
				types.Image(types.VendorFileID("google", "", "gs://bucket/b.jpg", types.MediaJPEG)))}}},
		{name: "document_pdf", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Document(types.Bytes(types.MediaPDF, pdfBytes)),
				types.Document(types.URL("https://example.com/notes.txt", types.MediaText)))}}},
		{name: "audio", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Audio(types.Bytes(types.MediaWAV, wavBytes)), types.Audio(types.URL("https://example.com/a.mp3", types.MediaMP3)))}}},
		{name: "video_metadata_vertex", vertex: true, req: types.Request{Messages: []types.Message{types.UserMsg(videoClip)}}},
		{name: "video_youtube", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Video(types.URL("https://www.youtube.com/watch?v=abc"), types.VideoMeta{ClipEnd: 5 * time.Second}))}}},
		{name: "tool_result_media", model: "gemini-3.1-flash-lite", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("take a photo")),
			types.AssistantMsg(types.ThinkingPart{Signature: sig("call-sig")}, call),
			types.ToolResults(types.ToolOK("c1", types.Text("done"), types.JSONPart{JSON: json.RawMessage(`{"n":1}`)},
				types.Image(types.Bytes(types.MediaPNG, pngBytes)), types.Document(types.Bytes(types.MediaPDF, pdfBytes)))),
		}, Tools: []types.ToolDef{{Name: "take_photo", Parameters: types.ParameterSchema{Type: "object"}}}}},
		{name: "tool_result_uri_vertex", vertex: true, model: "gemini-3.1-flash-lite", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("take a photo")),
			types.AssistantMsg(call),
			types.UserToolResults(types.ToolOK("c1", types.Image(types.URL("gs://bucket/photo.png", types.MediaPNG)))),
			types.UserMsg(types.Text("and now?")),
		}}},
		{name: "tool_result_error", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("go")),
			types.AssistantMsg(call, types.ToolCallPart{ID: "c2", Name: "write", Arguments: map[string]any{}}),
			types.ToolResults(types.ToolErr("c1", "camera offline"), types.ToolOK("c2", types.Text("ok"))),
		}}},
		{name: "assistant_replay", model: "gemini-3.1-flash-lite", req: types.Request{Messages: []types.Message{
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(
				types.ThinkingPart{Text: "let me think", Signature: sig("thought-sig")},
				types.TextPart{Text: "Answer."},
				types.ThinkingPart{Signature: sig("text-sig")},
				types.CitationPart{Citation: types.NewCitation(types.CitationWeb, "https://a.example", "A"), Anchor: &types.Anchor{End: 3}},
				types.ServerToolCallPart{ID: "s1", ToolKind: types.ServerToolCodeExecution, Name: "code_execution",
					Input: map[string]any{"code": "print(1)", "language": "PYTHON"}},
				types.ServerToolResultPart{CallID: "s1", ToolKind: types.ServerToolCodeExecution, Text: "1\n",
					Result: json.RawMessage(`{"outcome":"OUTCOME_OK","output":"1\n"}`)},
				types.ServerToolCallPart{ID: "s2", ToolKind: types.ServerToolWebSearch, Name: "google_search"},
				types.ServerToolResultPart{CallID: "s2", ToolKind: types.ServerToolWebSearch, Text: "A https://a.example"},
				types.ImageOutPart{Source: types.Bytes(types.MediaPNG, pngBytes), Signature: sig("image-sig")},
				types.AudioOutPart{Source: types.Bytes("audio/L16;codec=pcm;rate=24000", wavBytes)},
				types.AudioOutPart{Source: types.Source{MediaType: types.MediaWAV, Digest: "abc"}, Transcript: "hello there"},
				types.ThinkingPart{Text: "redacted", Redacted: true},
				types.RefusalPart{Text: "I cannot help with that."},
				types.ThinkingPart{Signature: sig("call-sig")},
				types.ToolCallPart{ID: "c9", Name: "lookup", Arguments: map[string]any{"q": "x"}},
			),
			types.UserMsg(types.Text("next")),
		}}},
		{name: "schema_and_options", req: types.Request{Messages: []types.Message{types.UserMsg(types.Text("capital of France"))},
			Schema:  &types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"capital": {Type: "string"}}},
			Options: &types.RequestOptions{Temperature: &temp}}},
		{name: "response_modalities", model: "gemini-2.5-flash", req: types.Request{Messages: []types.Message{types.UserMsg(types.Text("say hi"))}},
			opts: []Option{WithResponseModalities(genai.ModalityAudio), WithSpeechConfig(&genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: "Kore"}}})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "gemini-3.1-flash-lite"
			}
			var bodies []map[string]any
			a := testAdapter(t, tc.vertex, model, &bodies, tc.opts...)
			checkGolden(t, tc.name, sendRequest(t, a, tc.req, &bodies))
		})
	}
}

// TestRequestRejections checks that a part the request cannot carry fails
// before any request, with an error that names the part and matches the
// rejection class.
func TestRequestRejections(t *testing.T) {
	call := types.AssistantMsg(types.ToolCallPart{ID: "c1", Name: "f", Arguments: map[string]any{}})
	toolTurn := func(out ...types.ToolOutputPart) []types.Message {
		return []types.Message{types.UserMsg(types.Text("go")), call, types.ToolResults(types.ToolOK("c1", out...))}
	}
	big := make([]byte, maxInlinePart+1)
	for _, tc := range []struct {
		name   string
		vertex bool
		model  string
		msgs   []types.Message
		want   error
		path   string
	}{
		{name: "opaque file", msgs: []types.Message{types.UserMsg(types.File(types.Bytes("application/zip", []byte("PK"))))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0 (file, application/zip)"},
		{name: "gs uri on the Gemini API", msgs: []types.Message{types.UserMsg(types.Text("x"), types.Image(types.URL("gs://b/a.png", types.MediaPNG)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 1 (image, image/png)"},
		{name: "files api uri on Vertex", vertex: true,
			msgs: []types.Message{types.UserMsg(types.Image(types.URL("https://generativelanguage.googleapis.com/v1beta/files/abc", types.MediaPNG)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0"},
		{name: "files api name on Vertex", vertex: true,
			msgs: []types.Message{types.UserMsg(types.Image(types.VendorFileID("google", "", "files/abc", types.MediaPNG)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0"},
		{name: "another vendor's upload", msgs: []types.Message{types.UserMsg(types.Image(types.VendorFileID("openai", "", "file-1", types.MediaPNG)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0"},
		{name: "s3 uri", msgs: []types.Message{types.UserMsg(types.Document(types.URL("s3://b/a.pdf", types.MediaPDF)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0 (document, application/pdf)"},
		{name: "elided source", msgs: []types.Message{types.UserMsg(types.Image(types.Source{MediaType: types.MediaPNG, Digest: "abc"}))},
			want: types.ErrMediaUnavailable, path: "message 0 part 0"},
		{name: "unresolved source", msgs: []types.Message{types.UserMsg(types.Image(types.URL("https://x/a.png", types.MediaPNG).Unavailable("fetch failed")))},
			want: types.ErrMediaUnavailable, path: "message 0 part 0"},
		{name: "workspace ref only", msgs: []types.Message{types.UserMsg(types.Image(types.Artifact(types.ArtifactScheme+"abc", types.MediaPNG)))},
			want: types.ErrMediaUnavailable, path: "message 0 part 0"},
		{name: "inline over 20 MB", msgs: []types.Message{types.UserMsg(types.Video(types.Bytes(types.MediaMP4, big)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0 (video, video/mp4)"},
		{name: "media type of another kind", msgs: []types.Message{types.UserMsg(types.Image(types.Bytes(types.MediaWAV, wavBytes)))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0 (image, audio/wav)"},
		{name: "unsupported image type", msgs: []types.Message{types.UserMsg(types.Image(types.Bytes("image/bmp", pngBytes)))},
			want: types.ErrModalityUnsupported, path: "(image, image/bmp)"},
		{name: "unsupported document type", msgs: []types.Message{types.UserMsg(types.Document(types.Bytes(types.MediaDOCX, pdfBytes)))},
			want: types.ErrModalityUnsupported, path: "(document, " + string(types.MediaDOCX) + ")"},
		{name: "no media type", msgs: []types.Message{types.UserMsg(types.Image(types.URL("https://x/a")))},
			want: types.ErrModalityUnsupported, path: "message 0 part 0 (image)"},
		{name: "tool media before Gemini 3", model: "gemini-2.5-flash", msgs: toolTurn(types.Image(types.Bytes(types.MediaPNG, pngBytes))),
			want: types.ErrModalityUnsupported, path: "message 2 part 0 output 0 (image, image/png)"},
		{name: "tool audio", msgs: toolTurn(types.Text("x"), types.Audio(types.Bytes(types.MediaWAV, wavBytes))),
			want: types.ErrModalityUnsupported, path: "message 2 part 0 output 1 (audio, audio/wav)"},
		{name: "tool file", msgs: toolTurn(types.File(types.Bytes(types.MediaPDF, pdfBytes))),
			want: types.ErrModalityUnsupported, path: "output 0 (file, application/pdf)"},
		{name: "tool uri on the Gemini API", msgs: toolTurn(types.Image(types.URL("https://x/a.png", types.MediaPNG))),
			want: types.ErrModalityUnsupported, path: "output 0 (image, image/png)"},
		{name: "tool elided media", msgs: toolTurn(types.Image(types.Source{MediaType: types.MediaPNG})),
			want: types.ErrMediaUnavailable, path: "output 0 (image, image/png)"},
		{name: "video output replay", msgs: []types.Message{types.UserMsg(types.Text("x")),
			types.AssistantMsg(types.VideoOutPart{Source: types.Bytes(types.MediaMP4, []byte("v"))})},
			want: types.ErrModalityUnsupported, path: "message 1 part 0 (video_out, video/mp4)"},
		{name: "elided audio output without transcript", msgs: []types.Message{types.UserMsg(types.Text("x")),
			types.AssistantMsg(types.TextPart{Text: "a"}, types.AudioOutPart{Source: types.Source{MediaType: types.MediaWAV}})},
			want: types.ErrMediaUnavailable, path: "message 1 part 1 (audio_out, audio/wav)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if model == "" {
				model = "gemini-3.1-flash-lite"
			}
			var bodies []map[string]any
			a := testAdapter(t, tc.vertex, model, &bodies)
			_, err := a.Stream(context.Background(), types.Request{Messages: tc.msgs})
			if !errors.Is(err, tc.want) || !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("err = %v, want it to name %q", err, tc.path)
			}
			if len(bodies) != 0 {
				t.Fatalf("%d requests sent, want none", len(bodies))
			}
		})
	}
}

// TestBatchRequestRejectsParts checks that the batch path applies the same
// part mapping as Stream.
func TestBatchRequestRejectsParts(t *testing.T) {
	var bodies []map[string]any
	a := testAdapter(t, false, "gemini-3.1-flash-lite", &bodies)
	_, err := a.Submit(context.Background(), []types.BatchRequest{{CustomID: "a",
		Messages: []types.Message{types.UserMsg(types.File(types.Bytes("application/zip", []byte("PK"))))}}}, types.BatchSubmitOptions{})
	if !errors.Is(err, types.ErrModalityUnsupported) || !strings.Contains(err.Error(), "message 0 part 0 (file") || len(bodies) != 0 {
		t.Fatalf("err = %v, requests = %d", err, len(bodies))
	}

	temp := 0.1
	contents, config, err := a.batchRequest(types.BatchRequest{CustomID: "b",
		Messages: []types.Message{types.UserMsg(types.Text("x"), types.Video(types.URL("https://www.youtube.com/watch?v=abc")))},
		Schema:   &types.ParameterSchema{Type: "object"}, Options: types.RequestOptions{Temperature: &temp}})
	if err != nil {
		t.Fatal(err)
	}
	if fd := contents[0].Parts[1].FileData; fd == nil || fd.MIMEType != "video/*" {
		t.Fatalf("video part = %+v", contents[0].Parts[1])
	}
	if config.ResponseSchema == nil || config.Temperature == nil || *config.Temperature != float32(temp) {
		t.Fatalf("config = %+v, want the schema and the temperature", config)
	}
}

func TestRestRequestCarriesOutputModalities(t *testing.T) {
	c := &genai.GenerateContentConfig{ResponseModalities: []string{"IMAGE"}, SpeechConfig: &genai.SpeechConfig{LanguageCode: "en-US"}}
	gen, _ := restRequest(nil, c, "k")["generationConfig"].(map[string]any)
	if gen["responseModalities"] == nil || gen["speechConfig"] == nil {
		t.Fatalf("generationConfig = %v", gen)
	}
}

func TestMediaToolResultsByModel(t *testing.T) {
	for model, want := range map[string]bool{"gemini-3.1-flash-lite": true, "gemini-3-pro": true, "gemini-10-flash": true,
		"gemini-2.5-flash": false, "gemini-2.0": false, "text-embedding-004": false} {
		if got := mediaToolResults(model); got != want {
			t.Errorf("mediaToolResults(%q) = %v, want %v", model, got, want)
		}
	}
}

// On Vertex AI the adapter reports the Vertex offering, which reads gs://
// URIs, so a Cloud Storage image is planned as native there and rejected on
// the Gemini API instead of reaching the model as a text notice.
func TestOfferingFollowsTheBackend(t *testing.T) {
	img := types.Image(types.URL("gs://bucket/cat.png", types.MediaPNG))
	msgs := []types.Message{types.UserMsg(types.Text("what is it?"), img)}
	vertex := &Adapter{model: "gemini-3.1-flash-lite", backend: backend{kind: genai.BackendVertexAI}}
	if got := vertex.Offering().Endpoint.Name; got != endpointVertex {
		t.Fatalf("vertex endpoint = %q", got)
	}
	if _, err := convert.PlanConversions(vertex.Offering(), msgs, types.ConversionPolicy{}); err != nil {
		t.Fatalf("gs:// on Vertex: %v", err)
	}
	gemini := &Adapter{model: "gemini-3.1-flash-lite", backend: backend{kind: genai.BackendGeminiAPI}}
	if got := gemini.Offering().Endpoint.Name; got != endpointGemini {
		t.Fatalf("gemini endpoint = %q", got)
	}
	if _, err := convert.PlanConversions(gemini.Offering(), msgs, types.ConversionPolicy{}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("gs:// on the Gemini API: err = %v, want a rejection", err)
	}
}
