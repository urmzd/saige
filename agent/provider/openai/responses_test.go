package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// event renders one Responses API SSE event from its JSON body.
func event(body string) string { return "data: " + body + "\n\n" }

func textDelta(s string) string {
	return event(fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":%q,"sequence_number":1}`, s))
}

func callAdded(itemID, callID, name string) string {
	return event(fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"type":"function_call","id":%q,"call_id":%q,"name":%q,"arguments":"","status":"in_progress"}}`, itemID, callID, name))
}

func argsDelta(itemID, s string) string {
	return event(fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":%q,"output_index":0,"delta":%q,"sequence_number":1}`, itemID, s))
}

func argsDone(itemID, s string) string {
	return event(fmt.Sprintf(`{"type":"response.function_call_arguments.done","item_id":%q,"output_index":0,"arguments":%q,"sequence_number":1}`, itemID, s))
}

const responseUsage = `"usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":12}`

var completed = event(`{"type":"response.completed","sequence_number":9,"response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"completed","output":[],` + responseUsage + `}}`)

var incompleteLength = event(`{"type":"response.incomplete","sequence_number":9,"response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],` + responseUsage + `}}`)

var incompleteFiltered = event(`{"type":"response.incomplete","sequence_number":9,"response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[],` + responseUsage + `}}`)

func runResponses(t *testing.T, a *ResponsesAdapter, schema *types.ParameterSchema) streamResult {
	t.Helper()
	ch, err := a.ChatStreamWithSchema(context.Background(), []types.Message{types.NewUserMessage("go")}, nil, schema)
	if err != nil {
		t.Fatal(err)
	}
	var r streamResult
	for d := range ch {
		r.deltas = append(r.deltas, d)
		switch v := d.(type) {
		case types.ToolCallEndDelta:
			r.ends = append(r.ends, v)
		case types.UsageDelta:
			r.usage = true
		case types.ErrorDelta:
			r.errs = append(r.errs, v.Error)
		}
	}
	return r
}

func TestResponsesStream(t *testing.T) {
	tests := []struct {
		name          string
		body          []string
		structured    bool
		wantText      string
		wantEnds      map[string]string // call ID -> value of the "path" argument
		wantArgsErr   []string
		wantUsage     bool
		wantErr       bool
		wantTruncated bool
		wantKind      *types.ErrorKind
	}{
		{
			name:      "text response",
			body:      []string{textDelta("hel"), textDelta("lo"), completed},
			wantText:  "hello",
			wantUsage: true,
		},
		{
			name: "two function calls pair with their own arguments",
			body: []string{
				callAdded("fc_1", "call_a", "read"), argsDelta("fc_1", `{"path":`), argsDelta("fc_1", `"a.txt"}`), argsDone("fc_1", `{"path":"a.txt"}`),
				callAdded("fc_2", "call_b", "read"), argsDelta("fc_2", `{"path":"b.txt"}`), argsDone("fc_2", `{"path":"b.txt"}`),
				completed,
			},
			wantEnds:  map[string]string{"call_a": "a.txt", "call_b": "b.txt"},
			wantUsage: true,
		},
		{
			name:        "malformed finished arguments end the call with an error",
			body:        []string{callAdded("fc_1", "call_a", "write"), argsDelta("fc_1", `{"path":}`), argsDone("fc_1", `{"path":}`), completed},
			wantArgsErr: []string{"call_a"},
			wantUsage:   true,
		},
		{
			name:          "output limit inside a call is a truncation",
			body:          []string{callAdded("fc_1", "call_a", "write"), argsDelta("fc_1", `{"path":"a.t`), incompleteLength},
			wantUsage:     true,
			wantErr:       true,
			wantTruncated: true,
		},
		{
			name:          "output limit on structured output is a truncation",
			body:          []string{textDelta(`{"a":`), incompleteLength},
			structured:    true,
			wantText:      `{"a":`,
			wantUsage:     true,
			wantErr:       true,
			wantTruncated: true,
		},
		{
			name:      "output limit on plain text is committed",
			body:      []string{textDelta("partial"), incompleteLength},
			wantText:  "partial",
			wantUsage: true,
		},
		{
			name:     "stream without a terminal event is incomplete",
			body:     []string{textDelta("partial")},
			wantText: "partial",
			wantErr:  true,
		},
		{
			name:      "content filter stop is a refusal, not an answer",
			body:      []string{textDelta("partial"), incompleteFiltered},
			wantText:  "partial",
			wantUsage: true,
			wantErr:   true,
			wantKind:  ptr(types.ErrorKindContentFilter),
		},
		{
			name:     "failed response is classified by its code",
			body:     []string{event(`{"type":"response.failed","sequence_number":2,"response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`)},
			wantErr:  true,
			wantKind: ptr(types.ErrorKindRateLimit),
		},
		{
			name:     "error event is classified by its code",
			body:     []string{event(`{"type":"error","code":"server_error","message":"boom","param":null,"sequence_number":1}`)},
			wantErr:  true,
			wantKind: ptr(types.ErrorKindUnavailable),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewResponsesAdapter("test", testModel, WithBaseURL(sseServer(t, false, tt.body...).URL))
			var schema *types.ParameterSchema
			if tt.structured {
				schema = &types.ParameterSchema{Type: types.SchemaObject, Properties: map[string]types.PropertyDef{"a": {Type: types.SchemaString}}, Required: []string{"a"}}
			}
			r := runResponses(t, a, schema)

			var text strings.Builder
			for _, d := range r.deltas {
				if v, ok := d.(types.TextContentDelta); ok {
					text.WriteString(v.Content)
				}
			}
			if text.String() != tt.wantText {
				t.Errorf("text = %q, want %q", text.String(), tt.wantText)
			}
			if r.usage != tt.wantUsage {
				t.Errorf("usage = %v, want %v", r.usage, tt.wantUsage)
			}

			var argsErrs []string
			ends := map[string]string{}
			for _, end := range r.ends {
				if end.ArgumentsError != "" {
					argsErrs = append(argsErrs, end.ID)
					continue
				}
				path, _ := end.Arguments["path"].(string)
				ends[end.ID] = path
			}
			if strings.Join(argsErrs, ",") != strings.Join(tt.wantArgsErr, ",") {
				t.Errorf("argument errors = %v, want %v", argsErrs, tt.wantArgsErr)
			}
			if len(ends) != len(tt.wantEnds) {
				t.Errorf("ends = %v, want %v", ends, tt.wantEnds)
			}
			for id, want := range tt.wantEnds {
				if ends[id] != want {
					t.Errorf("call %s path = %q, want %q", id, ends[id], want)
				}
			}

			if (len(r.errs) > 0) != tt.wantErr {
				t.Fatalf("errors = %v, wantErr %v", r.errs, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			if got := errors.Is(r.errs[0], types.ErrResponseTruncated); got != tt.wantTruncated {
				t.Errorf("truncated = %v (%v), want %v", got, r.errs[0], tt.wantTruncated)
			}
			if tt.wantKind != nil {
				var pe *types.ProviderError
				if !errors.As(r.errs[0], &pe) || pe.Kind != *tt.wantKind {
					t.Errorf("error = %v, want kind %v", r.errs[0], *tt.wantKind)
				}
			}
		})
	}
}

func TestResponsesRequest(t *testing.T) {
	var body atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		body.Store(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textDelta("ok") + completed))
	}))
	t.Cleanup(server.Close)

	a := NewResponsesAdapter("test", testModel, WithBaseURL(server.URL), WithMaxTokens(64),
		WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired}))
	msgs := []types.Message{
		types.NewSystemMessage("be brief"),
		types.NewUserMessage("read a.txt"),
		types.AssistantMessage{Content: []types.AssistantContent{
			types.TextContent{Text: "reading"},
			types.ToolUseContent{ID: "call_a", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
		}},
		types.UserMessage{Content: []types.UserContent{types.ToolResultContent{ToolCallID: "call_a", Text: "contents"}}},
	}
	tools := []types.ToolDef{{Name: "read", Description: "Read a file", Parameters: types.ParameterSchema{
		Type: types.SchemaObject, Required: []string{"path"},
		Properties: map[string]types.PropertyDef{"path": {Type: types.SchemaString}},
	}}}
	ch, err := a.ChatStream(context.Background(), msgs, tools)
	if err != nil {
		t.Fatal(err)
	}
	for d := range ch {
		if e, ok := d.(types.ErrorDelta); ok {
			t.Fatal(e.Error)
		}
	}

	var got struct {
		Model           string `json:"model"`
		Store           *bool  `json:"store"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		ToolChoice      string `json:"tool_choice"`
		Tools           []struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			Strict *bool  `json:"strict"`
		} `json:"tools"`
		Input []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			CallID  string          `json:"call_id"`
			Name    string          `json:"name"`
			Output  string          `json:"output"`
		} `json:"input"`
	}
	raw, _ := body.Load().([]byte)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode request %s: %v", raw, err)
	}
	if got.Model != testModel || got.Store == nil || *got.Store || got.MaxOutputTokens != 64 || got.ToolChoice != "required" {
		t.Errorf("request = %s", raw)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Strict == nil || *got.Tools[0].Strict {
		t.Errorf("tools = %+v", got.Tools)
	}
	var kinds []string
	for _, item := range got.Input {
		// A message item may omit its type, which defaults to message.
		if item.Type == "" && item.Role != "" {
			item.Type = "message"
		}
		kinds = append(kinds, item.Type+":"+item.Role)
	}
	want := []string{"message:system", "message:user", "message:assistant", "function_call:", "function_call_output:"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("input items = %v, want %v", kinds, want)
	}
	if call := got.Input[3]; call.CallID != "call_a" || call.Name != "read" {
		t.Errorf("function call = %+v", call)
	}
	if out := got.Input[4]; out.CallID != "call_a" || out.Output != "contents" {
		t.Errorf("function call output = %+v", out)
	}
}

func TestResponsesRejectsUnsupportedControls(t *testing.T) {
	tests := []struct {
		name string
		opt  Option
		want string
	}{
		{"seed", WithSeed(1), "seed"},
		{"stop sequences", WithStopSequences("END"), "stop_sequences"},
		{"frequency penalty", WithFrequencyPenalty(0.5), "frequency_penalty"},
		{"presence penalty", WithPresencePenalty(0.5), "presence_penalty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			t.Cleanup(server.Close)
			a := NewResponsesAdapter("test", testModel, WithBaseURL(server.URL), tt.opt)
			_, err := a.ChatStream(context.Background(), []types.Message{types.NewUserMessage("go")}, nil)
			if !errors.Is(err, types.ErrInvalidModelConfig) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want an invalid config error naming %s", err, tt.want)
			}
			if calls.Load() != 0 {
				t.Fatal("request sent despite an unsupported control")
			}
		})
	}
}

func TestResponsesPromptCacheRetention(t *testing.T) {
	tests := []struct {
		retention string
		want      string
	}{
		{"", ""},
		{"in_memory", "in-memory"},
		{"in-memory", "in-memory"},
		{"24h", "24h"},
	}
	for _, tt := range tests {
		t.Run(tt.retention, func(t *testing.T) {
			a := NewResponsesAdapter("test", testModel, WithPromptCache("k", tt.retention))
			params, err := a.buildParams([]types.Message{types.NewUserMessage("go")}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(params.PromptCacheRetention) != tt.want || params.PromptCacheKey.Value != "k" {
				t.Fatalf("retention = %q key = %q, want %q", params.PromptCacheRetention, params.PromptCacheKey.Value, tt.want)
			}
		})
	}
}
func ptr[T any](v T) *T { return &v }
