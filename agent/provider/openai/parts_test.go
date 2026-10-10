package openai

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

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

const partsModel = "gpt-6-luna"

// golden compares got with testdata/<name>, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("%s differs from golden:\n got: %s\nwant: %s", name, got, want)
	}
}

func indentJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(out.Bytes(), '\n')
}

// fixtureServer serves a recorded SSE body as is.
func fixtureServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// streamOutcome is the assembled turn of a stream, for goldens.
type streamOutcome struct {
	Parts []json.RawMessage `json:"parts"`
	Error string            `json:"error,omitempty"`
}

func assemble(t *testing.T, ch <-chan types.Delta) streamOutcome {
	t.Helper()
	var deltas []types.Delta
	asm := types.NewPartAssembler()
	var out streamOutcome
	for d := range ch {
		deltas = append(deltas, d)
		asm.Push(d)
		if e, ok := d.(types.ErrorDelta); ok {
			var pe *types.ProviderError
			if errors.As(e.Error, &pe) {
				out.Error = pe.Kind.String()
			} else {
				out.Error = e.Error.Error()
			}
		}
	}
	streamcheck.RunPartConformance(t, deltas)
	if n := asm.Violations(); n > 0 {
		t.Errorf("%d assembler violations", n)
	}
	for _, p := range asm.Parts() {
		b, err := types.MarshalPart(p)
		if err != nil {
			t.Fatal(err)
		}
		out.Parts = append(out.Parts, b)
	}
	return out
}

// TestRecordedStreams replays recorded streams (live gpt-6-luna recordings,
// and hand-written fixtures for shapes no live check covers) and compares
// the assembled parts with goldens.
func TestRecordedStreams(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "streams", "*.sse"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sse")
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			srv := fixtureServer(t, body)
			opts := []Option{WithBaseURL(srv.URL)}
			if strings.Contains(name, "audio_out") {
				opts = append(opts, WithAudioOutput("alloy", "pcm16"))
			}
			req := types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}}
			var ch <-chan types.Delta
			if strings.HasPrefix(name, "chat_") {
				ch, err = NewAdapter("k", partsModel, opts...).Stream(context.Background(), req)
			} else {
				ch, err = NewResponsesAdapter("k", partsModel, opts...).Stream(context.Background(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			golden(t, filepath.Join("streams", name+".golden.json"), indentJSON(t, assemble(t, ch)))
		})
	}
}

// TestStreamIndicesFollowVendorOrder checks that a Responses stream with a
// reasoning item, a message and a function call numbers its parts in
// output order, and that interleaved deltas target their own index.
func TestStreamIndicesFollowVendorOrder(t *testing.T) {
	body := strings.Join([]string{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`),
		event(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"plan"}`),
		event(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":1,"delta":"act"}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc","summary":[{"type":"summary_text","text":"plan"},{"type":"summary_text","text":"act"}]}}`),
		event(`{"type":"response.content_part.added","item_id":"m","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}`),
		event(`{"type":"response.output_text.delta","item_id":"m","output_index":1,"content_index":0,"delta":"hi"}`),
		event(`{"type":"response.output_text.done","item_id":"m","output_index":1,"content_index":0,"text":"hi"}`),
		callAdded("fc_1", "call_1", "read"), argsDone("fc_1", `{"p":1}`),
		completed,
	}, "")
	srv := sseServer(t, false, body)
	ch, err := NewResponsesAdapter("k", partsModel, WithBaseURL(srv.URL)).Stream(context.Background(),
		types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	got := assemble(t, ch)
	var kinds []string
	for _, p := range got.Parts {
		var k struct{ Type string }
		_ = json.Unmarshal(p, &k)
		kinds = append(kinds, k.Type)
	}
	if strings.Join(kinds, ",") != "thinking,text,tool_call" {
		t.Fatalf("parts = %v", kinds)
	}
	var th struct {
		Text, Signature string
		Summary         bool
	}
	_ = json.Unmarshal(got.Parts[0], &th)
	if th.Text != "plan\n\nact" || th.Signature != "enc" || !th.Summary {
		t.Fatalf("thinking = %+v", th)
	}
}

// ── Request mapping ──────────────────────────────────────────────────

var (
	pngData = []byte("\x89PNG\r\n\x1a\nfixture")
	pdfData = []byte("%PDF-1.4 fixture")
	wavData = []byte("RIFF\x00\x00\x00\x00WAVEfmt fixture")
	mp3Data = []byte("ID3fixture")
)

func openaiFile(id string, mt types.MediaType) types.Source {
	return types.VendorFileID(providerName, "", id, mt)
}

type mappingCase struct {
	name     string
	surfaces string // "chat", "responses" or "both"
	req      types.Request
	opts     []Option
}

func mappingCases() []mappingCase {
	imageCall := types.ToolCallPart{ID: "call_1", Name: "snapshot", Arguments: map[string]any{"q": "x"}}
	snapshot := []types.ToolDef{{Name: "snapshot", Description: "Takes a snapshot", Parameters: types.ParameterSchema{Type: "object"}}}
	jsonPart, _ := types.JSON(map[string]any{"ok": true})
	none := "none"
	schema := &types.ParameterSchema{Type: "object", Required: []string{"color"}, Properties: map[string]types.PropertyDef{"color": {Type: "string"}}}
	msgs := func(ms ...types.Message) []types.Message { return ms }
	return []mappingCase{
		{name: "text", surfaces: "both", req: types.Request{Messages: msgs(
			types.SystemMsg(types.Text("Be brief.")),
			types.UserMsg(types.Text("hi")),
			types.AssistantMsg(types.Text("hello")),
			types.UserMsg(types.Text("again"), types.Text(" please")),
		)}},
		{name: "image_url", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Text("what is it?"), types.Image(types.URL("https://example.com/a.png", types.MediaPNG), types.ImageMeta{Detail: "high"})))}},
		{name: "image_base64", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Image(types.Bytes(types.MediaPNG, pngData), types.ImageMeta{Detail: "low"})))}},
		// The Responses API takes the file ID; Chat Completions has no image
		// file input and falls back to the bytes the source also carries.
		{name: "image_file_id", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Image(openaiFile("file-img", types.MediaPNG).With(types.Bytes(types.MediaPNG, pngData)))))}},
		{name: "pdf_base64", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Text("summarize"), types.Document(types.Source{MediaType: types.MediaPDF, Filename: "paper.pdf"}.With(types.Bytes(types.MediaPDF, pdfData)))))}},
		{name: "pdf_unnamed", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Document(types.Bytes(types.MediaPDF, pdfData))))}},
		{name: "pdf_file_id", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(
			types.Document(openaiFile("file-pdf", types.MediaPDF))))}},
		{name: "document_url", surfaces: "responses", req: types.Request{Messages: msgs(types.UserMsg(
			types.Document(types.URL("https://example.com/paper.pdf", types.MediaPDF))))}},
		{name: "document_docx", surfaces: "responses", req: types.Request{Messages: msgs(types.UserMsg(
			types.Document(types.Source{MediaType: types.MediaDOCX, Filename: "plan.docx"}.With(types.Bytes(types.MediaDOCX, []byte("PK fixture"))))))}},
		{name: "document_text", surfaces: "responses", req: types.Request{Messages: msgs(types.UserMsg(
			types.Document(types.Bytes(types.MediaCSV, []byte("a,b\n1,2")))))}},
		{name: "audio_wav", surfaces: "chat", req: types.Request{Messages: msgs(types.UserMsg(
			types.Text("transcribe"), types.Audio(types.Bytes(types.MediaWAV, wavData))))}},
		{name: "audio_mp3", surfaces: "chat", req: types.Request{Messages: msgs(types.UserMsg(
			types.Audio(types.Bytes(types.MediaMP3, mp3Data))))}},
		{name: "tool_result_text", surfaces: "both", req: types.Request{Tools: snapshot, Messages: msgs(
			types.UserMsg(types.Text("snap")),
			types.AssistantMsg(types.Text("Taking it."), imageCall),
			types.ToolResults(types.ToolOK("call_1", types.Text("done"), jsonPart), types.ToolErr("call_2", "boom")),
		)}},
		{name: "tool_result_media", surfaces: "responses", req: types.Request{Tools: snapshot, Messages: msgs(
			types.UserMsg(types.Text("snap")),
			types.AssistantMsg(imageCall),
			types.ToolResults(types.ToolOK("call_1", types.Text("snapshot"), types.Image(types.Bytes(types.MediaPNG, pngData)),
				types.Document(openaiFile("file-pdf", types.MediaPDF)))),
		)}},
		{name: "human_tool_result_before_text", surfaces: "both", req: types.Request{Tools: snapshot, Messages: msgs(
			types.AssistantMsg(imageCall),
			types.UserMsg(types.ToolOK("call_1", types.Text("approved")), types.Text("and summarize")),
		)}},
		{name: "assistant_replay", surfaces: "responses", req: types.Request{Messages: msgs(
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(
				types.ThinkingPart{Text: "summary", Signature: "enc-1", Summary: true},
				types.ThinkingPart{Signature: "enc-2"},
				types.ThinkingPart{Text: "another vendor's thought", Signature: "sig"},
				types.Text("a"),
				types.CitationPart{Citation: types.NewCitation(types.CitationWeb, "https://example.com", "Ex")},
				types.RefusalPart{Text: "no"},
			),
			types.UserMsg(types.Text("next")),
		)}},
		{name: "assistant_replay", surfaces: "chat", req: types.Request{Messages: msgs(
			types.UserMsg(types.Text("q")),
			types.AssistantMsg(types.ThinkingPart{Text: "t", Signature: "s"}, types.Text("a"), types.RefusalPart{Text: "no"},
				types.AudioOutPart{VendorID: "audio_1", Transcript: "spoken"}),
			types.AssistantMsg(types.AudioOutPart{VendorID: "audio_old", Transcript: "earlier words", ExpiresAt: time.Unix(1, 0)}),
			types.UserMsg(types.Text("next")),
		)}},
		{name: "audio_output", surfaces: "chat", opts: []Option{WithAudioOutput("alloy", "pcm16")},
			req: types.Request{Messages: msgs(types.UserMsg(types.Text("say hi")))}},
		{name: "reasoning_summary", surfaces: "responses", opts: []Option{WithReasoningSummary("auto")},
			req: types.Request{Messages: msgs(types.UserMsg(types.Text("think")))}},
		{name: "schema_options", surfaces: "both", req: types.Request{Messages: msgs(types.UserMsg(types.Text("color?"))),
			Schema: schema, Options: &types.RequestOptions{ReasoningEffort: &none, MaxOutputTokens: ptr(int64(50))}}},
	}
}

// TestRequestMappingGoldens encodes every part mapping of both surfaces
// and compares the request body with goldens.
func TestRequestMappingGoldens(t *testing.T) {
	for _, tc := range mappingCases() {
		for _, surface := range []string{"chat", "responses"} {
			if tc.surfaces != "both" && tc.surfaces != surface {
				continue
			}
			t.Run(surface+"_"+tc.name, func(t *testing.T) {
				var body any
				if surface == "chat" {
					_, p, err := NewAdapter("k", partsModel, tc.opts...).chatParams(tc.req)
					if err != nil {
						t.Fatal(err)
					}
					body = p
				} else {
					_, p, err := NewResponsesAdapter("k", partsModel, tc.opts...).requestParams(tc.req)
					if err != nil {
						t.Fatal(err)
					}
					body = p
				}
				golden(t, filepath.Join("requests", surface+"_"+tc.name+".json"), indentJSON(t, body))
			})
		}
	}
}

// TestUnsupportedPartsRejected checks that a part a surface cannot send
// fails before the request, with the sentinel and the part's path.
func TestUnsupportedPartsRejected(t *testing.T) {
	call := types.ToolCallPart{ID: "call_1", Name: "f"}
	tools := []types.ToolDef{{Name: "f", Parameters: types.ParameterSchema{Type: "object"}}}
	user := func(p ...types.UserPart) []types.Message {
		return []types.Message{types.UserMsg(types.Text("x")), types.UserMsg(p...)}
	}
	toolOut := func(p ...types.ToolOutputPart) []types.Message {
		return []types.Message{types.UserMsg(types.Text("x")), types.AssistantMsg(call), types.ToolResults(types.ToolOK("call_1", p...))}
	}
	for _, tc := range []struct {
		name     string
		surface  string
		msgs     []types.Message
		sentinel error
		path     string
	}{
		{"chat video", "chat", user(types.Video(types.Bytes(types.MediaMP4, []byte{0}))), types.ErrModalityUnsupported, "part 1.0 (video video/mp4)"},
		{"responses video", "responses", user(types.Video(types.URL("https://x/v.mp4", types.MediaMP4))), types.ErrModalityUnsupported, "part 1.0 (video"},
		{"responses audio", "responses", user(types.Text("hear"), types.Audio(types.Bytes(types.MediaWAV, wavData))), types.ErrModalityUnsupported, "part 1.1 (audio audio/wav)"},
		{"chat audio by URL", "chat", user(types.Audio(types.URL("https://x/a.wav", types.MediaWAV))), types.ErrModalityUnsupported, "part 1.0 (audio"},
		{"chat audio flac", "chat", user(types.Audio(types.Bytes("audio/flac", []byte{1}))), types.ErrModalityUnsupported, "audio audio/flac"},
		{"chat docx", "chat", user(types.Document(types.Bytes(types.MediaDOCX, []byte{1}))), types.ErrModalityUnsupported, "part 1.0 (document"},
		{"chat text document", "chat", user(types.Document(types.Bytes(types.MediaText, []byte("hi")))), types.ErrModalityUnsupported, "document text/plain"},
		{"chat pdf by URL", "chat", user(types.Document(types.URL("https://x/a.pdf", types.MediaPDF))), types.ErrModalityUnsupported, "accepted: file, inline"},
		{"chat opaque file", "chat", user(types.File(types.Bytes("application/zip", []byte{1}))), types.ErrModalityUnsupported, "(file application/zip)"},
		{"responses zip file", "responses", user(types.File(types.Bytes("application/zip", []byte{1}))), types.ErrModalityUnsupported, "(file application/zip)"},
		{"bmp image", "both", user(types.Image(types.Bytes("image/bmp", []byte{1}))), types.ErrModalityUnsupported, "image image/bmp"},
		{"gs image", "both", user(types.Image(types.URL("gs://bucket/a.png", types.MediaPNG))), types.ErrModalityUnsupported, "gs://bucket"},
		{"another vendor's file", "both", user(types.Image(types.VendorFileID("anthropic", "", "file_x", types.MediaPNG))), types.ErrModalityUnsupported, "another provider"},
		{"chat image file only", "chat", user(types.Image(openaiFile("file-img", types.MediaPNG))), types.ErrModalityUnsupported, "accepted: uri, inline"},
		{"workspace ref only", "both", user(types.Image(types.Artifact(types.ArtifactScheme+"abc", types.MediaPNG))), types.ErrMediaUnavailable, "workspace reference"},
		{"elided source", "both", user(types.Document(types.Source{MediaType: types.MediaPDF, Digest: "abc"})), types.ErrMediaUnavailable, "no locator"},
		{"unresolved source", "both", user(types.Image(types.Bytes(types.MediaPNG, pngData).Unavailable("fetch failed: 404"))), types.ErrMediaUnavailable, "fetch failed: 404"},
		{"chat tool image", "chat", toolOut(types.Text("ok"), types.Image(types.Bytes(types.MediaPNG, pngData))), types.ErrModalityUnsupported, "part 2.0.1 (image image/png)"},
		{"chat tool pdf", "chat", toolOut(types.Document(types.Bytes(types.MediaPDF, pdfData))), types.ErrModalityUnsupported, "text only"},
		{"responses tool audio", "responses", toolOut(types.Audio(types.Bytes(types.MediaWAV, wavData))), types.ErrModalityUnsupported, "part 2.0.0 (audio"},
		{"responses assistant audio", "responses", []types.Message{types.AssistantMsg(types.AudioOutPart{VendorID: "a"})}, types.ErrModalityUnsupported, "part 0.0 (audio_out"},
		{"chat assistant image", "chat", []types.Message{types.AssistantMsg(types.ImageOutPart{Source: types.Bytes(types.MediaPNG, pngData)})}, types.ErrModalityUnsupported, "image_out"},
		{"chat expired audio", "chat", []types.Message{types.AssistantMsg(types.AudioOutPart{VendorID: "a", ExpiresAt: time.Unix(1, 0)})}, types.ErrMediaUnavailable, "expired"},
	} {
		for _, surface := range []string{"chat", "responses"} {
			if tc.surface != "both" && tc.surface != surface {
				continue
			}
			t.Run(surface+" "+tc.name, func(t *testing.T) {
				var hits int
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
				defer srv.Close()
				req := types.Request{Messages: tc.msgs, Tools: tools}
				var err error
				if surface == "chat" {
					_, err = NewAdapter("k", partsModel, WithBaseURL(srv.URL)).Stream(context.Background(), req)
				} else {
					_, err = NewResponsesAdapter("k", partsModel, WithBaseURL(srv.URL)).Stream(context.Background(), req)
				}
				if err == nil {
					t.Fatal("expected a rejection")
				}
				if !errors.Is(err, tc.sentinel) || !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want %v", err, tc.sentinel)
				}
				if !strings.Contains(err.Error(), tc.path) {
					t.Fatalf("err = %q, want it to name %q", err, tc.path)
				}
				if hits != 0 {
					t.Fatal("rejected request reached the network")
				}
			})
		}
	}
}

// TestSurfaceOnlyOptionsRejected checks the options one surface lacks.
func TestSurfaceOnlyOptionsRejected(t *testing.T) {
	req := types.Request{Messages: []types.Message{types.UserMsg(types.Text("x"))}}
	if _, _, err := NewAdapter("k", partsModel, WithReasoningSummary("auto")).chatParams(req); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("chat reasoning summary: %v", err)
	}
	if _, _, err := NewResponsesAdapter("k", partsModel, WithAudioOutput("alloy", "wav")).requestParams(req); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("responses audio output: %v", err)
	}
	if _, _, err := NewResponsesAdapter("k", partsModel, WithReasoningSummary("verbose")).requestParams(req); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("bad summary mode: %v", err)
	}
	if _, _, err := NewAdapter("k", partsModel, WithAudioOutput("alloy", "ogg")).chatParams(req); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("bad audio format: %v", err)
	}
	if _, _, err := NewAdapter("k", partsModel, WithAudioOutput("", "wav")).chatParams(req); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Errorf("missing voice: %v", err)
	}
}

// TestBatchBodiesMapToParts checks that batch results carry the same parts
// a stream produces: refusals, audio, citations and reasoning.
func TestBatchBodiesMapToParts(t *testing.T) {
	chat := `{"id":"c","model":"gpt-audio","choices":[{"finish_reason":"stop","message":{"content":"Go 1.26 shipped.",
		"annotations":[{"type":"url_citation","url_citation":{"url":"https://go.dev","title":"Go","start_index":0,"end_index":7}}],
		"audio":{"id":"audio_1","data":"AAEC","transcript":"Go 1.26 shipped.","expires_at":1791609449}}}],"usage":{"total_tokens":3}}`
	var res types.BatchResult
	if err := NewAdapter("k", partsModel, WithAudioOutput("alloy", "wav")).decodeChatBody(json.RawMessage(chat), &res); err != nil {
		t.Fatal(err)
	}
	golden(t, "batch_chat.golden.json", indentJSON(t, marshalParts(t, res.Message.Parts)))

	refusal := `{"id":"c","model":"m","choices":[{"finish_reason":"stop","message":{"content":null,"refusal":"No."}}]}`
	res = types.BatchResult{}
	if err := NewAdapter("k", partsModel).decodeChatBody(json.RawMessage(refusal), &res); err != nil {
		t.Fatal(err)
	}
	if rp, ok := res.Message.Parts[0].(types.RefusalPart); !ok || rp.Text != "No." || res.FinishReason != finishContentFilter {
		t.Fatalf("refusal = %+v, finish %q", res.Message.Parts, res.FinishReason)
	}

	resp := `{"id":"r","model":"m","status":"completed","output":[
		{"type":"reasoning","encrypted_content":"enc","summary":[{"type":"summary_text","text":"thought"}]},
		{"type":"message","content":[{"type":"output_text","text":"See [1].","annotations":[{"type":"url_citation","url":"https://go.dev","title":"Go","start_index":4,"end_index":7}]},
			{"type":"refusal","refusal":"Not that."}]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}]}`
	res = types.BatchResult{}
	if err := decodeResponsesBody(json.RawMessage(resp), &res); err != nil {
		t.Fatal(err)
	}
	golden(t, "batch_responses.golden.json", indentJSON(t, marshalParts(t, res.Message.Parts)))
	if res.FinishReason != finishContentFilter {
		t.Fatalf("finish = %q", res.FinishReason)
	}
}

func marshalParts(t *testing.T, parts []types.AssistantPart) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(parts))
	for i, p := range parts {
		b, err := types.MarshalPart(p)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// TestBatchSubmitMapsParts checks that a batch request with a part the
// surface cannot send fails at submit, before the upload.
func TestBatchSubmitMapsParts(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	reqs := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Video(types.Bytes(types.MediaMP4, []byte{1})))}}}
	if _, err := NewAdapter("k", partsModel, WithBaseURL(srv.URL)).Submit(context.Background(), reqs, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("chat submit: %v", err)
	}
	if _, err := NewResponsesAdapter("k", partsModel, WithBaseURL(srv.URL)).Submit(context.Background(), reqs, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrModalityUnsupported) {
		t.Fatalf("responses submit: %v", err)
	}
	if hits != 0 {
		t.Fatal("rejected batch reached the network")
	}
}
