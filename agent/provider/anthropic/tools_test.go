package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// captureServer records each request body and answers with a minimal
// complete stream.
func captureServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []sseEvent{{"message_start", evStart}, {"message_delta", evMessageDelta("end_turn", 1)}, {"message_stop", evStop}} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.kind, e.data)
		}
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

func drain(ch <-chan types.Delta) []types.Delta {
	var out []types.Delta
	for d := range ch {
		out = append(out, d)
	}
	return out
}

var testTools = []types.ToolDef{
	{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}},
	{Name: "write", Parameters: types.ParameterSchema{Type: "object"}},
}

func TestToolChoiceWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []Option
		tools   []types.ToolDef
		want    map[string]any // nil means tool_choice is absent
		wantErr bool
	}{
		{name: "unset sends nothing", tools: testTools},
		{name: "auto", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceAuto})}, tools: testTools,
			want: map[string]any{"type": "auto"}},
		{name: "none", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone})}, tools: testTools,
			want: map[string]any{"type": "none"}},
		{name: "required is any", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})}, tools: testTools,
			want: map[string]any{"type": "any"}},
		{name: "named", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"})}, tools: testTools,
			want: map[string]any{"type": "tool", "name": "write"}},
		{name: "required with parallel off", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired}), WithParallelToolCalls(false)},
			tools: testTools, want: map[string]any{"type": "any", "disable_parallel_tool_use": true}},
		{name: "parallel off alone stays auto", opts: []Option{WithParallelToolCalls(false)}, tools: testTools,
			want: map[string]any{"type": "auto", "disable_parallel_tool_use": true}},
		{name: "no tools offered sends no choice", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})}},
		{name: "server tools only carry none", opts: []Option{WithServerTools(types.WebSearchTool(1)), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone})},
			want: map[string]any{"type": "none"}},
		{name: "server tools only carry required", opts: []Option{WithServerTools(types.WebSearchTool(1)), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})},
			want: map[string]any{"type": "any"}},
		{name: "named with only server tools", opts: []Option{WithServerTools(types.WebSearchTool(1)), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"})},
			wantErr: true},
		{name: "named tool not offered", opts: []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"})},
			tools: testTools, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			a := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, append(tc.opts, WithBaseURL(server.URL))...))
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: tc.tools})
			if tc.wantErr {
				if err == nil || !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want an invalid configuration", err)
				}
				if len(*bodies) != 0 {
					t.Fatal("an invalid choice must fail before the request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			drain(ch)
			got, ok := (*bodies)[0]["tool_choice"].(map[string]any)
			if tc.want == nil {
				if ok {
					t.Fatalf("tool_choice = %v, want absent", got)
				}
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("tool_choice = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchemaToolChoice(t *testing.T) {
	schema := &types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"a": {Type: "string"}}}
	for _, tc := range []struct {
		name   string
		choice *types.ToolChoice
		ok     bool
	}{
		{"unset", nil, true},
		{"auto", &types.ToolChoice{Mode: types.ToolChoiceAuto}, true},
		{"none", &types.ToolChoice{Mode: types.ToolChoiceNone}, false},
		{"required", &types.ToolChoice{Mode: types.ToolChoiceRequired}, false},
		{"named", &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, bodies := captureServer(t)
			opts := []Option{WithBaseURL(server.URL)}
			if tc.choice != nil {
				opts = append(opts, WithToolChoice(*tc.choice))
			}
			a := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, opts...))
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: testTools, Schema: schema})
			if !tc.ok {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want an invalid configuration", err)
				}
				if len(*bodies) != 0 {
					t.Fatal("a conflicting choice must fail before the request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			drain(ch)
			got, _ := (*bodies)[0]["tool_choice"].(map[string]any)
			if got["type"] != "tool" || got["name"] != "structured_output" {
				t.Fatalf("tool_choice = %v, want the forced structured_output tool", got)
			}
		})
	}
}

func TestToolChoiceValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		opts  []Option
		ok    bool
	}{
		{"required with manual thinking", testModel, []Option{WithThinking(2048), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})}, false},
		{"named with manual thinking", testModel, []Option{WithThinking(2048), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "x"})}, false},
		{"none with thinking", testModel, []Option{WithThinking(2048), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone})}, true},
		{"required on an adaptive model", "claude-haiku-5-5", []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})}, true},
		{"named with adaptive effort", "claude-haiku-5-5", []Option{WithReasoningEffort("low"), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "x"})}, true},
		{"required on a model that rejects forcing", "claude-sonnet-5-5", []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired})}, false},
		{"named on a model that rejects forcing", "claude-opus-5-5", []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "x"})}, false},
		{"auto on a model that rejects forcing", "claude-fable-5-1", []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceAuto})}, true},
		{"none on a model that rejects forcing", "claude-mythos-5-1", []Option{WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceNone})}, true},
		{"unknown mode", testModel, []Option{WithToolChoice(types.ToolChoice{Mode: "sometimes"})}, false},
		{"web search", testModel, []Option{WithServerTools(types.WebSearchTool(3))}, true},
		{"code execution", testModel, []Option{WithServerTools(types.ServerTool{Kind: types.ServerToolCodeExecution})}, true},
		{"code execution on an undeclared model", "claude-3-5-sonnet", []Option{WithServerTools(types.ServerTool{Kind: types.ServerToolCodeExecution})}, false},
		{"remote MCP is not sent", testModel, []Option{WithServerTools(types.ServerTool{Kind: types.ServerToolRemoteMCP,
			MCPServer: &types.RemoteMCPServer{Name: "n", URL: "https://example.com"}})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := must.Get(New(Config{APIKey: "k", Model: types.ModelID(tc.model)}, tc.opts...)).Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestServerToolsWire(t *testing.T) {
	server, bodies := captureServer(t)
	ws := types.WebSearchTool(4)
	ws.AllowedDomains = []string{"go.dev"}
	ws.UserLocation = "Austin, Texas, US"
	a := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, WithBaseURL(server.URL), WithServerTools(ws, types.ServerTool{Kind: types.ServerToolCodeExecution})))
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	tools, _ := (*bodies)[0]["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want web search and code execution", tools)
	}
	search := tools[0].(map[string]any)
	if search["type"] != "web_search_20250305" || search["name"] != "web_search" || search["max_uses"] != float64(4) {
		t.Errorf("web search = %v", search)
	}
	loc, _ := search["user_location"].(map[string]any)
	if loc["city"] != "Austin" || loc["region"] != "Texas" || loc["country"] != "US" || loc["type"] != "approximate" {
		t.Errorf("user_location = %v", loc)
	}
	if code := tools[1].(map[string]any); code["type"] != "code_execution_20250825" {
		t.Errorf("code execution = %v", code)
	}
	if _, ok := (*bodies)[0]["tool_choice"]; ok {
		t.Error("server tools alone must not send a tool choice")
	}
}

func TestServerToolStream(t *testing.T) {
	events := []sseEvent{
		{"message_start", evStart},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`},
		{"content_block_delta", evArgs(`{"query":"go generics"}`)},
		{"content_block_stop", evBlockStop},
		{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://go.dev/doc","title":"Docs","encrypted_content":"x"}]}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"server_tool_use","id":"srvtoolu_2","name":"bash_code_execution","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":2}`},
		{"content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"bash_code_execution_tool_result","tool_use_id":"srvtoolu_2","content":{"type":"bash_code_execution_result","stdout":"","stderr":"boom","return_code":1,"content":[]}}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":3}`},
		{"content_block_start", `{"type":"content_block_start","index":4,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_3","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":4}`},
		{"message_delta", evMessageDelta("end_turn", 30)},
		{"message_stop", evStop},
	}
	a := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, false, events...).URL)))
	r := run(t, a, nil)
	if len(r.errs) > 0 {
		t.Fatal(r.errs)
	}
	var calls []types.ServerToolCallPart
	var results []types.ServerToolResultPart
	for _, d := range r.deltas {
		switch v := d.(type) {
		case types.PartEnd:
			switch p := v.Part.(type) {
			case types.ServerToolCallPart:
				calls = append(calls, p)
			case types.ServerToolResultPart:
				results = append(results, p)
			case types.ToolCallPart:
				t.Fatalf("server tool leaked as a local tool call: %#v", p)
			}
		case types.PartStart:
			if v.Kind == types.KindToolCall {
				t.Fatalf("server tool leaked as a local tool call: %#v", v)
			}
		}
	}
	if len(calls) != 2 || len(results) != 3 {
		t.Fatalf("calls = %+v, results = %+v", calls, results)
	}
	for _, tc := range []struct {
		got  types.ServerToolCallPart
		id   string
		kind types.ServerToolKind
		key  string
		val  string
	}{
		{calls[0], "srvtoolu_1", types.ServerToolWebSearch, "query", "go generics"},
		{calls[1], "srvtoolu_2", types.ServerToolCodeExecution, "command", "ls"},
	} {
		if tc.got.ID != tc.id || tc.got.ToolKind != tc.kind || tc.got.Input[tc.key] != tc.val {
			t.Errorf("call = %+v, want %s %s %s=%s", tc.got, tc.id, tc.kind, tc.key, tc.val)
		}
	}
	for _, tc := range []struct {
		got     types.ServerToolResultPart
		id      string
		kind    types.ServerToolKind
		text    string
		isError bool
	}{
		{results[0], "srvtoolu_1", types.ServerToolWebSearch, "Docs https://go.dev/doc", false},
		{results[1], "srvtoolu_2", types.ServerToolCodeExecution, "boom", true},
		{results[2], "srvtoolu_3", types.ServerToolWebSearch, "error: max_uses_exceeded", true},
	} {
		if tc.got.CallID != tc.id || tc.got.ToolKind != tc.kind || tc.got.Text != tc.text || tc.got.IsError != tc.isError || len(tc.got.Result) == 0 {
			t.Errorf("result = %+v, want %s %s %q error=%v", tc.got, tc.id, tc.kind, tc.text, tc.isError)
		}
	}
}

func TestServerToolContentIsNotReplayed(t *testing.T) {
	_, out := toParams([]types.Message{
		types.UserMsg(types.Text("q")),
		types.AssistantMessage{Parts: []types.AssistantPart{
			types.ServerToolCallPart{ID: "srvtoolu_1", ToolKind: types.ServerToolWebSearch},
			types.ServerToolResultPart{CallID: "srvtoolu_1", ToolKind: types.ServerToolWebSearch, Text: "r"},
			types.TextPart{Text: "answer"},
		}},
	})
	if len(out) != 2 || len(out[1].Content) != 1 || out[1].Content[0].OfText == nil {
		t.Fatalf("assistant blocks = %+v, want only the text", out)
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-5","type":"model","display_name":"Claude Sonnet 4.5","created_at":"2025-09-29T00:00:00Z","max_input_tokens":200000,"max_tokens":64000}],"has_more":false,"first_id":"claude-sonnet-4-5","last_id":"claude-sonnet-4-5"}`)
	}))
	t.Cleanup(server.Close)
	models, err := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, WithBaseURL(server.URL))).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "claude-sonnet-4-5" || models[0].ContextWindow != 200000 ||
		models[0].MaxOutputTokens != 64000 || models[0].DisplayName == "" || models[0].Created.IsZero() {
		t.Fatalf("models = %+v", models)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)
	if _, err := must.Get(New(Config{APIKey: "k", Model: types.ModelID(testModel)}, WithBaseURL(failing.URL))).ListModels(context.Background()); !types.IsAuth(err) {
		t.Fatalf("err = %v, want an auth error", err)
	}
}
