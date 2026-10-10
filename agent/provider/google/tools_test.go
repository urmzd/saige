package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// captureTransport records request bodies and answers like sseTransport.
type captureTransport struct {
	events []string
	bodies *[]map[string]any
}

func (t captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*t.bodies = append(*t.bodies, body)
	}
	return sseTransport{events: t.events}.RoundTrip(req)
}

const doneEvent = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`

var testTools = []types.ToolDef{
	{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}},
	{Name: "write", Parameters: types.ParameterSchema{Type: "object"}},
}

func TestToolChoiceWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choice  *types.ToolChoice
		tools   []types.ToolDef
		want    string // fmt.Sprint of functionCallingConfig; "" means no toolConfig
		wantErr bool
	}{
		{name: "unset", tools: testTools},
		{name: "auto", choice: &types.ToolChoice{Mode: types.ToolChoiceAuto}, tools: testTools, want: "map[mode:AUTO]"},
		{name: "none", choice: &types.ToolChoice{Mode: types.ToolChoiceNone}, tools: testTools, want: "map[mode:NONE]"},
		{name: "required", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired}, tools: testTools, want: "map[mode:ANY]"},
		{name: "named", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"}, tools: testTools,
			want: "map[allowedFunctionNames:[write] mode:ANY]"},
		{name: "no tools offered", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired}},
		{name: "named tool not offered", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}, tools: testTools, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []map[string]any
			opts := []Option{WithHTTPClient(&http.Client{Transport: captureTransport{events: []string{doneEvent}, bodies: &bodies}})}
			if tc.choice != nil {
				opts = append(opts, WithToolChoice(*tc.choice))
			}
			a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: tc.tools})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) || len(bodies) != 0 {
					t.Fatalf("err = %v, requests = %d; want a local configuration error", err, len(bodies))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			cfg, ok := bodies[0]["toolConfig"].(map[string]any)
			if tc.want == "" {
				if ok {
					t.Fatalf("toolConfig = %v, want absent", cfg)
				}
				return
			}
			if got := fmt.Sprint(cfg["functionCallingConfig"]); got != tc.want {
				t.Fatalf("functionCallingConfig = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPenaltiesAreSentAndValidated(t *testing.T) {
	var bodies []map[string]any
	half := float32(0.5)
	a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithHTTPClient(&http.Client{Transport: captureTransport{events: []string{doneEvent}, bodies: &bodies}}), WithGenerationConfig(GenerationConfig{FrequencyPenalty: &half, PresencePenalty: &half}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	gen, _ := bodies[0]["generationConfig"].(map[string]any)
	if gen["frequencyPenalty"] != 0.5 || gen["presencePenalty"] != 0.5 {
		t.Fatalf("generationConfig = %v", gen)
	}

	tooHigh := float32(3)
	if _, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithGenerationConfig(GenerationConfig{FrequencyPenalty: &tooHigh})); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("err = %v, want an out-of-range rejection", err)
	}
}

func TestServerToolDeltas(t *testing.T) {
	events := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"executableCode":{"language":"PYTHON","code":"print(1+1)"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"codeExecutionResult":{"outcome":"OUTCOME_OK","output":"2\n"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"executableCode":{"language":"PYTHON","code":"1/0"}},{"codeExecutionResult":{"outcome":"OUTCOME_FAILED","output":"ZeroDivisionError"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"groundingMetadata":{"webSearchQueries":["go generics"],"groundingChunks":[{"web":{"uri":"https://go.dev/doc","title":"Docs"}}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"."}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["go generics"],"groundingChunks":[{"web":{"uri":"https://go.dev/doc","title":"Docs"}}]}}]}`,
	}
	a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithServerTools(types.ServerTool{Kind: types.ServerToolCodeExecution}, types.ServerTool{Kind: types.ServerToolWebSearch}), WithHTTPClient(&http.Client{Transport: sseTransport{events: events}}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []types.ServerToolCallPart
	var results []types.ServerToolResultPart
	var citations int
	for d := range ch {
		switch v := d.(type) {
		case types.PartEnd:
			switch p := v.Part.(type) {
			case types.ServerToolCallPart:
				calls = append(calls, p)
			case types.ServerToolResultPart:
				results = append(results, p)
			case types.CitationPart:
				citations++
			}
		case types.ErrorDelta:
			t.Fatal(v.Error)
		}
	}
	if len(calls) != 3 || len(results) != 3 {
		t.Fatalf("calls = %+v, results = %+v", calls, results)
	}
	for i, tc := range []struct {
		kind    types.ServerToolKind
		input   string
		text    string
		isError bool
	}{
		{types.ServerToolCodeExecution, "print(1+1)", "2\n", false},
		{types.ServerToolCodeExecution, "1/0", "ZeroDivisionError", true},
		{types.ServerToolWebSearch, "", "Docs https://go.dev/doc", false},
	} {
		c, r := calls[i], results[i]
		if c.ID == "" || c.ID != r.CallID || c.ToolKind != tc.kind || r.ToolKind != tc.kind {
			t.Errorf("pair %d: call %+v, result %+v", i, c, r)
		}
		if tc.input != "" && (c.Input["code"] != tc.input || c.Input["language"] != "PYTHON") {
			t.Errorf("pair %d: input = %v", i, c.Input)
		}
		if r.Text != tc.text || r.IsError != tc.isError || len(r.Result) == 0 {
			t.Errorf("pair %d: result = %+v, want %q error=%v", i, r, tc.text, tc.isError)
		}
	}
	if q, _ := calls[2].Input["queries"].([]any); len(q) != 1 || q[0] != "go generics" {
		t.Errorf("search input = %v", calls[2].Input)
	}
	if citations == 0 {
		t.Error("grounding sources must still be reported as citations")
	}
}

func TestListModels(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"models":[` +
			`{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash","inputTokenLimit":1048576,"outputTokenLimit":65536,"supportedGenerationMethods":["generateContent"]},` +
			`{"name":"models/text-embedding-004","displayName":"Embedding","inputTokenLimit":2048,"outputTokenLimit":1,"supportedGenerationMethods":["embedContent"]}]}`
		if !strings.HasSuffix(req.URL.Path, "/models") {
			return &http.Response{StatusCode: http.StatusNotFound, Request: req, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Request: req,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(body))}, nil
	})
	a, err := New(context.Background(), Config{APIKey: "k", Model: "gemini-2.5-flash"}, WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	models, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gemini-2.5-flash" || models[0].ContextWindow != 1048576 ||
		models[0].MaxOutputTokens != 65536 || models[0].Embedding || !models[1].Embedding {
		t.Fatalf("models = %+v", models)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
