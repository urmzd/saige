package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestAcceptsTemperature(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4o-mini", true},
		{"gpt-4.1", true},
		{"o3", false},
		{"o1-mini", false},
		{"o4-mini", false},
		{"gpt-5", false},
		{"gpt-5.1", true}, // reasoning is off by default, so temperature is accepted
		{"llama-3.1-8b-instant", true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := AcceptsTemperature(tt.model); got != tt.want {
				t.Errorf("AcceptsTemperature(%q) = %v, want %v", tt.model, got, tt.want)
			}
			if got := NewClient("", "k", tt.model).Temperature != nil; got != tt.want {
				t.Errorf("NewClient(%q) sends temperature = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestChatResultAdd(t *testing.T) {
	a := ChatResult{Text: "first", InputTokens: 10, OutputTokens: 5, CachedInputTokens: 2, Retried: true}
	b := ChatResult{Text: "second", InputTokens: 7, OutputTokens: 3, CachedInputTokens: 1}
	got := a.Add(b)
	want := ChatResult{Text: "second", InputTokens: 17, OutputTokens: 8, CachedInputTokens: 3, Retried: true}
	if got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
}

// schemaProvider adds structured output to a scripted provider.
type schemaProvider struct {
	*agenttest.ScriptedProvider
	schema *types.ParameterSchema
}

func (p *schemaProvider) ChatStreamWithSchema(ctx context.Context, m []types.Message, tl []types.ToolDef, s *types.ParameterSchema) (<-chan types.Delta, error) {
	p.schema = s
	return p.ChatStream(ctx, m, tl)
}

// plainProvider implements only types.Provider.
type plainProvider struct{ inner *agenttest.ScriptedProvider }

func (p plainProvider) ChatStream(ctx context.Context, m []types.Message, tl []types.ToolDef) (<-chan types.Delta, error) {
	return p.inner.ChatStream(ctx, m, tl)
}

func TestProviderClientChat(t *testing.T) {
	usage := types.UsageDelta{PromptTokens: 12, CompletionTokens: 4, CachedPromptTokens: 3}
	reply := append(agenttest.TextResponse("hello"), usage)
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "required": []any{"a"}}
	temp := 0.0

	tests := []struct {
		name     string
		provider func(*agenttest.ScriptedProvider) types.Provider
		temp     *float64
		opts     []ChatOption
		wantErr  error
		wantOpts bool
	}{
		{name: "plain", provider: func(s *agenttest.ScriptedProvider) types.Provider { return s }},
		{name: "temperature through options", provider: func(s *agenttest.ScriptedProvider) types.Provider { return s }, temp: &temp, wantOpts: true},
		{name: "temperature needs options provider", provider: func(s *agenttest.ScriptedProvider) types.Provider { return plainProvider{s} }, temp: &temp, wantErr: types.ErrInvalidModelConfig},
		{name: "schema", provider: func(s *agenttest.ScriptedProvider) types.Provider { return &schemaProvider{ScriptedProvider: s} }, opts: []ChatOption{WithJSONSchema("x", schema)}},
		{name: "schema needs structured provider", provider: func(s *agenttest.ScriptedProvider) types.Provider { return s }, opts: []ChatOption{WithJSONSchema("x", schema)}, wantErr: types.ErrInvalidModelConfig},
		{name: "schema with temperature", provider: func(s *agenttest.ScriptedProvider) types.Provider { return &schemaProvider{ScriptedProvider: s} }, temp: &temp, opts: []ChatOption{WithJSONSchema("x", schema)}, wantErr: types.ErrInvalidModelConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripted := &agenttest.ScriptedProvider{Responses: [][]types.Delta{reply}}
			p := tt.provider(scripted)
			c := NewProviderClient(p)
			c.Temperature = tt.temp
			got, err := c.Chat(context.Background(), []Message{
				{Role: roleSystem, Content: "sys"},
				{Role: roleUser, Content: "hi"},
				{Role: roleAssistant, Content: "earlier"},
				{Role: roleUser, Content: "again"},
			}, tt.opts...)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != "hello" || got.InputTokens != 12 || got.OutputTokens != 4 || got.CachedInputTokens != 3 {
				t.Errorf("result = %+v", got)
			}
			reqs := scripted.Requests()
			if len(reqs) != 1 || len(reqs[0].Messages) != 4 {
				t.Fatalf("requests = %+v", reqs)
			}
			if roles := []types.Role{reqs[0].Messages[0].Role(), reqs[0].Messages[2].Role()}; roles[0] != types.RoleSystem || roles[1] != types.RoleAssistant {
				t.Errorf("roles = %v", roles)
			}
			if (reqs[0].Options != nil) != tt.wantOpts {
				t.Errorf("options = %+v, want sent %v", reqs[0].Options, tt.wantOpts)
			}
			if sp, ok := p.(*schemaProvider); ok && len(tt.opts) > 0 {
				if sp.schema == nil || sp.schema.Type != "object" || len(sp.schema.Required) != 1 {
					t.Errorf("schema = %+v", sp.schema)
				}
			}
		})
	}
}

func TestProviderClientReturnsStreamError(t *testing.T) {
	boom := &types.ProviderError{Provider: "p", Kind: types.ErrorKindAuth, Err: errors.New("denied")}
	scripted := &agenttest.ScriptedProvider{Responses: [][]types.Delta{{types.TextContentDelta{Content: "x"}, types.ErrorDelta{Error: boom}}}}
	_, err := NewProviderClient(scripted).Chat(context.Background(), []Message{{Role: roleUser, Content: "hi"}})
	if !types.IsAuth(err) {
		t.Fatalf("err = %v, want an auth error", err)
	}
	if _, err := NewProviderClient(scripted).Chat(context.Background(), []Message{{Role: "tool", Content: "x"}}); err == nil {
		t.Error("an unsupported role should fail")
	}
}

func TestClientClassifiesHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantCalls int64
		is        func(error) bool
	}{
		{"context length is not retried", http.StatusBadRequest, "This model's maximum context length is 128000 tokens", 1, types.IsContextLength},
		{"auth is not retried", http.StatusUnauthorized, "invalid api key", 1, types.IsAuth},
		{"unavailable is retried", http.StatusServiceUnavailable, "overloaded", 2, func(err error) bool { return err == nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					http.Error(w, tt.body, tt.status)
					return
				}
				writeChatResponse(t, w, "ok", 1, 1)
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "k", "mock").Chat(context.Background(), []Message{{Role: roleUser, Content: "hi"}})
			if !tt.is(err) {
				t.Errorf("err = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), OpenAICompatible) {
				t.Errorf("error should name the transport: %v", err)
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

func TestClientProviderName(t *testing.T) {
	if got := NewClient("", "k", "").ProviderName(); got != OpenAICompatible {
		t.Errorf("HTTP client name = %q", got)
	}
	if got := NewProviderClient(&agenttest.ScriptedProvider{}).ProviderName(); got != "unknown" {
		t.Errorf("unnamed provider = %q", got)
	}
}

func TestClientRetriesTruncatedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Promise more bytes than are sent, so the body read fails with an
		// unexpected EOF when the handler returns.
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":`))
	}))
	defer server.Close()
	c := NewClient(server.URL, "k", "mock")
	_, retry, err := c.doChat(context.Background(), []byte(`{}`))
	var perr *types.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T %v, want a *types.ProviderError", err, err)
	}
	if !retry || !perr.Kind.Transient() {
		t.Errorf("retry = %v, kind = %v; want a transient failure", retry, perr.Kind)
	}
}
