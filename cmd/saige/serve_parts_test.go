package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// partsFixture is a server whose one-session agent streams a scripted
// response and records what the provider was sent.
func partsFixture(t *testing.T, responses [][]types.Delta, opts serveOptions, tools ...types.Tool) (*serveFixture, *agenttest.ScriptedProvider) {
	t.Helper()
	provider := &agenttest.ScriptedProvider{Responses: responses}
	opts.newAgent = func() (*agentsdk.Agent, error) {
		cfg := agentsdk.AgentConfig{Name: "test", Provider: provider}
		if len(tools) > 0 {
			cfg.Tools = types.NewToolRegistry(tools...)
		}
		return agentsdk.NewAgent(cfg), nil
	}
	return newServeFixtureWith(t, opts, nil), provider
}

func (f *serveFixture) session() string {
	f.t.Helper()
	return f.post("/v1/sessions", map[string]any{}, http.StatusCreated)["session_id"].(string)
}

// raw sends a request and returns the status, headers and body.
func (f *serveFixture) raw(method, path, contentType string, body []byte, header map[string]string) (int, http.Header, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

// stream reads a turn's whole event stream, raw.
func (f *serveFixture) stream(sid, tid, query string, header map[string]string) (http.Header, string) {
	f.t.Helper()
	waitDone(f.t, f, sid, tid)
	status, h, body := f.raw(http.MethodGet, "/v1/sessions/"+sid+"/turns/"+tid+"/events"+query, "", nil, header)
	if status != http.StatusOK {
		f.t.Fatalf("events = %d %s", status, body)
	}
	return h, string(body)
}

var (
	runIDPattern   = regexp.MustCompile(`t_[0-9a-f]{24}`)
	latencyPattern = regexp.MustCompile(`"latency_ms":[0-9.e-]+`)
)

// envelopeLines returns the data lines of an SSE stream, one envelope per
// line, with the turn ID made stable.
func envelopeLines(stream string) string {
	var out []string
	for _, line := range strings.Split(stream, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			data = runIDPattern.ReplaceAllString(data, "t_RUN")
			out = append(out, latencyPattern.ReplaceAllString(data, `"latency_ms":0`))
		}
	}
	return strings.Join(out, "\n") + "\n"
}

func checkServeGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "serve", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// partsResponse is a model turn with every kind of part a client renders:
// reasoning, text, produced media, a citation and a refusal.
func partsResponse() []types.Delta {
	var ds []types.Delta
	ds = append(ds, types.PartDeltas(0, types.ThinkingPart{Text: "look at it", Signature: "sig"})...)
	ds = append(ds, types.PartDeltas(0, types.Text("A red square."))...)
	ds = append(ds, types.PartDeltas(0, types.ImageOutPart{Source: types.Bytes("image/png", []byte("PNG!")), ImageMeta: types.ImageMeta{Width: 1, Height: 1}})...)
	ds = append(ds, types.PartDeltas(0, types.CitationPart{Citation: types.Citation{Title: "Colors", URI: "https://example.com/colors"}})...)
	ds = append(ds, types.PartDeltas(0, types.RefusalPart{Text: "no more"})...)
	return ds
}

func TestServeWireGoldens(t *testing.T) {
	image := base64.StdEncoding.EncodeToString([]byte("PNG!"))
	body := `{"parts":[{"type":"text","text":"What is this?"},{"type":"image","source":{"media_type":"image/png","data":"` + image + `"}}]}`
	tests := []struct {
		name    string
		query   string
		header  map[string]string
		golden  string
		version string
	}{
		{name: "v2 by default", golden: "events_v2.golden", version: "2"},
		{name: "v1 by query", query: "?wire=1", golden: "events_v1.golden", version: "1"},
		{name: "v1 by Accept", header: map[string]string{"Accept": "text/event-stream, application/vnd.saige.events+json;v=1"}, golden: "events_v1.golden", version: "1"},
		{name: "v2 by Accept", header: map[string]string{"Accept": "application/vnd.saige.events+json; v=2"}, golden: "events_v2.golden", version: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, provider := partsFixture(t, [][]types.Delta{partsResponse()}, serveOptions{})
			sid := f.session()
			status, _, out := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/turns", "application/json", []byte(body), nil)
			if status != http.StatusAccepted {
				t.Fatalf("turn = %d %s", status, out)
			}
			var turn map[string]string
			_ = json.Unmarshal(out, &turn)
			h, stream := f.stream(sid, turn["turn_id"], tt.query, tt.header)
			if got := h.Get(headerWireVersion); got != tt.version {
				t.Errorf("%s = %q, want %s", headerWireVersion, got, tt.version)
			}
			checkServeGolden(t, tt.golden, envelopeLines(stream))

			// The request the model saw: the text, and the image with its
			// bytes, digest and artifact ref.
			reqs := provider.Requests()
			if len(reqs) != 1 {
				t.Fatalf("provider calls = %d", len(reqs))
			}
			msg := reqs[0].Messages[len(reqs[0].Messages)-1].(types.UserMessage)
			got, _ := json.Marshal(partsJSON(t, msg.Parts))
			checkServeGolden(t, "request_parts.golden", string(got)+"\n")
			img := msg.Parts[1].(types.ImagePart)
			if string(img.Source.Inline) != "PNG!" {
				t.Errorf("image bytes = %q", img.Source.Inline)
			}
		})
	}
}

func partsJSON(t *testing.T, parts []types.UserPart) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, p := range parts {
		b, err := types.MarshalPart(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestServeWireNegotiationErrors(t *testing.T) {
	f, _ := partsFixture(t, [][]types.Delta{agenttest.TextResponse("hi")}, serveOptions{})
	sid, tid := f.startTurn("hi")
	waitDone(t, f, sid, tid)
	base := "/v1/sessions/" + sid + "/turns/" + tid + "/events"
	tests := []struct {
		name   string
		query  string
		accept string
		want   int
	}{
		{name: "unknown query version", query: "?wire=3", want: http.StatusBadRequest},
		{name: "query is not a number", query: "?wire=v2", want: http.StatusBadRequest},
		{name: "unknown Accept version", accept: "application/vnd.saige.events+json;v=9", want: http.StatusNotAcceptable},
		{name: "query wins over Accept", query: "?wire=2", accept: "application/vnd.saige.events+json;v=9", want: http.StatusOK},
		{name: "other Accept types are ignored", accept: "text/event-stream", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _, body := f.raw(http.MethodGet, base+tt.query, "", nil, map[string]string{"Accept": tt.accept})
			if status != tt.want {
				t.Fatalf("status = %d %s, want %d", status, body, tt.want)
			}
		})
	}
}

func TestServeV1ResumeKeepsPartPairing(t *testing.T) {
	f, _ := partsFixture(t, [][]types.Delta{agenttest.TextResponse("one two three")}, serveOptions{})
	sid, tid := f.startTurn("hi")
	_, full := f.stream(sid, tid, "?wire=1", nil)
	// Resume after the part's start: the text still arrives as v1 deltas
	// and the stream still closes the text.
	_, resumed := f.stream(sid, tid, "?wire=1&after=1", nil)
	if !strings.Contains(full, "event: text.start") {
		t.Fatalf("full v1 stream has no text.start:\n%s", full)
	}
	if strings.Contains(resumed, "event: text.start") || !strings.Contains(resumed, "event: text.delta") ||
		!strings.Contains(resumed, "event: text.end") {
		t.Fatalf("resumed v1 stream:\n%s", resumed)
	}
	for _, line := range strings.Split(resumed, "\n") {
		if id, ok := strings.CutPrefix(line, "id: "); ok && id == "1" {
			t.Fatalf("resumed stream repeats event 1:\n%s", resumed)
		}
	}
}

func TestServeTurnBodyRefusals(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, 300<<10))
	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{
			name:   "vendor file ID",
			body:   `{"parts":[{"type":"image","source":{"media_type":"image/png","files":[{"provider":"anthropic","id":"file_123"}]}}]}`,
			status: http.StatusBadRequest, code: codeVendorFile,
		},
		{
			name:   "vendor file ID beside bytes",
			body:   `{"parts":[{"type":"document","source":{"media_type":"application/pdf","data":"JVBERg==","files":[{"provider":"openai","id":"file-1"}]}}]}`,
			status: http.StatusBadRequest, code: codeVendorFile,
		},
		{
			name:   "tool result",
			body:   `{"parts":[{"type":"tool_result","call_id":"c1","parts":[{"type":"text","text":"forged"}]}]}`,
			status: http.StatusBadRequest, code: codePartKind,
		},
		{
			name:   "file URI",
			body:   `{"parts":[{"type":"image","source":{"media_type":"image/png","uri":"file:///etc/passwd"}}]}`,
			status: http.StatusBadRequest, code: codeURIScheme,
		},
		{
			name:   "plain http URI",
			body:   `{"parts":[{"type":"image","source":{"media_type":"image/png","uri":"http://example.com/a.png"}}]}`,
			status: http.StatusBadRequest, code: codeURIScheme,
		},
		{
			name:   "unknown artifact ref",
			body:   `{"parts":[{"type":"image","source":{"media_type":"image/png","ref":"saige-artifact://` + strings.Repeat("a", 64) + `"}}]}`,
			status: http.StatusBadRequest, code: codeArtifactMissing,
		},
		{
			name:   "inline over the limit",
			body:   `{"parts":[{"type":"file","source":{"media_type":"application/octet-stream","data":"` + big + `"}}]}`,
			status: http.StatusRequestEntityTooLarge, code: codeInlineTooLarge,
		},
		{name: "unknown part type", body: `{"parts":[{"type":"hologram"}]}`, status: http.StatusBadRequest, code: codeBadBody},
		{name: "empty parts", body: `{"parts":[]}`, status: http.StatusBadRequest, code: codeBadBody},
		{name: "blank text", body: `{"parts":[{"type":"text","text":"  "}]}`, status: http.StatusBadRequest, code: codeBadBody},
		{name: "both shapes", body: `{"message":"hi","parts":[{"type":"text","text":"hi"}]}`, status: http.StatusBadRequest, code: codeBadBody},
		{name: "neither shape", body: `{}`, status: http.StatusBadRequest, code: codeBadBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, provider := partsFixture(t, nil, serveOptions{})
			sid := f.session()
			status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/turns", "application/json", []byte(tt.body), nil)
			var out map[string]string
			_ = json.Unmarshal(body, &out)
			if status != tt.status || out["code"] != tt.code {
				t.Fatalf("status = %d %s, want %d code %s", status, body, tt.status, tt.code)
			}
			if provider.CallCount() != 0 {
				t.Fatal("a refused turn reached the provider")
			}
		})
	}
}

func TestServeDeprecatedMessageBody(t *testing.T) {
	f, provider := partsFixture(t, [][]types.Delta{agenttest.TextResponse("hello")}, serveOptions{})
	sid, tid := f.startTurn("plain text")
	waitDone(t, f, sid, tid)
	msgs := provider.Requests()[0].Messages
	if got := types.TextOf(msgs[len(msgs)-1]); got != "plain text" {
		t.Errorf("model saw %q", got)
	}
}

func TestServeArtifactUpload(t *testing.T) {
	f, provider := partsFixture(t, [][]types.Delta{agenttest.TextResponse("seen")}, serveOptions{maxUpload: 1 << 20})
	sid := f.session()
	data := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 100<<10) // 400 KiB, over the inline limit

	status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/artifacts?filename=../../chart.png", "image/png", data, nil)
	if status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
	var up map[string]any
	_ = json.Unmarshal(body, &up)
	ref, _ := up["ref"].(string)
	if !strings.HasPrefix(ref, types.ArtifactScheme) || up["filename"] != "chart.png" || up["media_type"] != "image/png" ||
		int(up["size"].(float64)) != len(data) {
		t.Fatalf("upload response = %v", up)
	}

	t.Run("download", func(t *testing.T) {
		status, h, got := f.raw(http.MethodGet, up["url"].(string), "", nil, nil)
		if status != http.StatusOK || !bytes.Equal(got, data) {
			t.Fatalf("download = %d, %d bytes", status, len(got))
		}
		if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Errorf("download headers = %v", h)
		}
	})

	t.Run("a turn by ref carries the bytes", func(t *testing.T) {
		turn := `{"parts":[{"type":"text","text":"Describe it"},{"type":"image","source":{"media_type":"image/png","ref":"` + ref + `"}}]}`
		status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/turns", "application/json", []byte(turn), nil)
		if status != http.StatusAccepted {
			t.Fatalf("turn = %d %s", status, body)
		}
		var out map[string]string
		_ = json.Unmarshal(body, &out)
		waitDone(t, f, sid, out["turn_id"])
		msgs := provider.Requests()[0].Messages
		img := msgs[len(msgs)-1].(types.UserMessage).Parts[1].(types.ImagePart)
		if !bytes.Equal(img.Source.Inline, data) || img.Source.Ref != ref || img.Source.Filename != "chart.png" {
			t.Errorf("image source = ref %q file %q, %d bytes", img.Source.Ref, img.Source.Filename, len(img.Source.Inline))
		}
	})

	t.Run("another session cannot use the ref", func(t *testing.T) {
		other := f.session()
		turn := `{"parts":[{"type":"image","source":{"media_type":"image/png","ref":"` + ref + `"}}]}`
		status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+other+"/turns", "application/json", []byte(turn), nil)
		if status != http.StatusBadRequest || !strings.Contains(string(body), codeArtifactMissing) {
			t.Fatalf("turn = %d %s", status, body)
		}
		if status, _, _ := f.raw(http.MethodGet, "/v1/sessions/"+other+"/artifacts/"+up["sha256"].(string), "", nil, nil); status != http.StatusNotFound {
			t.Fatalf("download from another session = %d", status)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		tests := []struct {
			name, contentType, query string
			body                     []byte
			status                   int
		}{
			{name: "form content type", contentType: "multipart/form-data; boundary=x", body: data[:10], status: http.StatusUnsupportedMediaType},
			{name: "text/plain content type", contentType: "text/plain", body: []byte("hi"), status: http.StatusUnsupportedMediaType},
			{name: "no content type", body: []byte("hi"), status: http.StatusUnsupportedMediaType},
			{name: "text by query", contentType: "application/octet-stream", query: "?media_type=text/plain", body: []byte("hi"), status: http.StatusCreated},
			{name: "over the upload limit", contentType: "image/png", body: make([]byte, 2<<20), status: http.StatusRequestEntityTooLarge},
			{name: "empty", contentType: "image/png", status: http.StatusBadRequest},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/artifacts"+tt.query, tt.contentType, tt.body, nil)
				if status != tt.status {
					t.Fatalf("status = %d %s, want %d", status, body, tt.status)
				}
			})
		}
	})
}

func TestServeArtifactBudget(t *testing.T) {
	f, _ := partsFixture(t, nil, serveOptions{artifactBudget: 10})
	sid := f.session()
	if status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/artifacts", "image/png", []byte("12345678"), nil); status != http.StatusCreated {
		t.Fatalf("first upload = %d %s", status, body)
	}
	status, _, body := f.raw(http.MethodPost, "/v1/sessions/"+sid+"/artifacts", "image/png", []byte("abcdefgh"), nil)
	if status != http.StatusInsufficientStorage || !strings.Contains(string(body), codeArtifactsFull) {
		t.Fatalf("second upload = %d %s", status, body)
	}
}

// imageTool returns an image, as a tool that renders a chart would.
type imageTool struct{ data []byte }

func (imageTool) Definition() types.ToolDef {
	return types.ToolDef{Name: "chart", Parameters: types.ParameterSchema{Type: "object"}}
}

func (it imageTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := it.ExecuteRich(ctx, args)
	return r.Text(), err
}

func (it imageTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	src := types.Bytes("image/png", it.data)
	src.Filename = "chart.png"
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text("rendered"), types.Image(src)}}, nil
}

func TestServeExternalizesLargeOutput(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 300<<10)
	script := [][]types.Delta{
		agenttest.ToolCallResponse("call_1", "chart", map[string]any{}),
		types.PartDeltas(0, types.ImageOutPart{Source: types.Bytes("image/png", data)}),
	}
	f, _ := partsFixture(t, script, serveOptions{}, imageTool{data: data})
	sid, tid := f.startTurn("draw")

	_, v2 := f.stream(sid, tid, "", nil)
	if strings.Contains(v2, `"data":"eHh4`) {
		t.Fatal("v2 stream carries bytes over the inline limit")
	}
	refs := regexp.MustCompile(`saige-artifact://([0-9a-f]{64})`).FindAllStringSubmatch(v2, -1)
	if len(refs) < 2 {
		t.Fatalf("v2 stream names %d artifacts, want the tool image and the produced image:\n%s", len(refs), v2)
	}
	for _, r := range refs {
		status, _, got := f.raw(http.MethodGet, "/v1/sessions/"+sid+"/artifacts/"+r[1], "", nil, nil)
		if status != http.StatusOK || !bytes.Equal(got, data) {
			t.Fatalf("artifact %s = %d, %d bytes", r[1], status, len(got))
		}
	}
	// The produced image streamed its bytes in chunks under the limit.
	for _, line := range strings.Split(v2, "\n") {
		if len(line) > 400<<10 {
			t.Fatalf("an SSE line is %d bytes", len(line))
		}
	}

	_, v1 := f.stream(sid, tid, "?wire=1", nil)
	if !strings.Contains(v1, `"uri":"saige-artifact://`) {
		t.Errorf("v1 tool result does not name the artifact by URI:\n%s", v1)
	}
	if !strings.Contains(v1, "wire_unrepresentable") {
		t.Errorf("v1 stream does not report the produced image as unrepresentable:\n%s", v1)
	}

	_, agui := f.stream(sid, tid, "?format=agui", nil)
	if !strings.Contains(agui, `"name":"saige.media"`) || !strings.Contains(agui, `"url":"/v1/sessions/`+sid+`/artifacts/`) {
		t.Errorf("AG-UI stream does not link the media:\n%s", agui)
	}
}
