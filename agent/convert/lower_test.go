package convert_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/agent/convert"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

var update = flag.Bool("update", false, "rewrite golden files")

// captureBody serves one request with an error and returns the body it
// was sent, indented.
func captureBody(t *testing.T, stream func(baseURL string) (<-chan types.Delta, error)) []byte {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == nil {
			body, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"captured","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	ch, err := stream(srv.URL + "/v1")
	if err != nil {
		t.Fatalf("the request was refused before it was sent: %v", err)
	}
	for range ch {
	}
	if body == nil {
		t.Fatal("no request reached the server")
	}
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
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
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden:\n got: %s\nwant: %s", name, got, want)
	}
}

// toolImageRequest is a turn whose two tool calls returned an image and
// text.
func toolImageRequest() types.Request {
	img := types.Image(types.Bytes(types.MediaPNG, []byte("\x89PNG\r\n\x1a\nfixture")))
	return types.Request{
		Tools: []types.ToolDef{{Name: "snapshot", Description: "Takes a snapshot", Parameters: types.ParameterSchema{Type: "object"}}},
		Messages: []types.Message{
			types.UserMsg(types.Text("what color is the snapshot?")),
			types.AssistantMsg(types.ToolCallPart{ID: "call_1", Name: "snapshot", Arguments: map[string]any{}},
				types.ToolCallPart{ID: "call_2", Name: "snapshot", Arguments: map[string]any{}}),
			types.ToolResults(types.ToolOK("call_1", types.Text("snapshot taken"), img), types.ToolOK("call_2", types.Text("no change"))),
		},
	}
}

// On Chat Completions, whose tool messages carry text only, the image is
// sent in a user message after the tool messages.
func TestChatRequestLowersToolResultImage(t *testing.T) {
	req := toolImageRequest()
	body := captureBody(t, func(base string) (<-chan types.Delta, error) {
		p := must.Get(provider.Build(context.Background(), provider.Config{Provider: provider.OpenAI, Model: "gpt-6-luna", APIKey: "k", BaseURL: base}))
		if o, _ := convert.Target(p); o.Modalities.ToolResult[types.ModalityImage] != types.ToolResultFollowUpUser {
			t.Fatalf("Chat offering %s does not lower tool result images", o.ID)
		}
		return p.Stream(context.Background(), req)
	})
	golden(t, "requests/chat_tool_result_image.json", body)
	if _, ok := req.Messages[2].(types.SystemMessage).Parts[0].(types.ToolResultPart).Parts[1].(types.ImagePart); !ok {
		t.Fatal("the record lost its image")
	}
}

// The Responses API takes the image inside function_call_output.
func TestResponsesRequestKeepsToolResultImage(t *testing.T) {
	body := captureBody(t, func(base string) (<-chan types.Delta, error) {
		inner := must.Get(openai.NewResponses(openai.Config{APIKey: "k", Model: "gpt-6-luna"}, openai.WithBaseURL(base)))
		if got := inner.Offering().Modalities.ToolResult[types.ModalityImage]; got != types.ToolResultInline {
			t.Fatalf("Responses tool_result.image = %q, want inline", got)
		}
		p := must.Get(convert.New(inner, convert.Config{}))
		return p.Stream(context.Background(), toolImageRequest())
	})
	golden(t, "requests/responses_tool_result_image.json", body)
}
