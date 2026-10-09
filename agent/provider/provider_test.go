package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func ptr[T any](v T) *T { return &v }

func TestInfer(t *testing.T) {
	for _, tt := range []struct {
		model, provider, bare string
		err                   bool
	}{
		{"claude-sonnet-4-5", Anthropic, "claude-sonnet-4-5", false},
		{"gpt-4o-mini", OpenAI, "gpt-4o-mini", false},
		{"o3", OpenAI, "o3", false},
		{"gemini-2.5-flash", Google, "gemini-2.5-flash", false},
		{"claude-haiku-5-5", Anthropic, "claude-haiku-5-5", false},
		{"gpt-6-luna", OpenAI, "gpt-6-luna", false},
		{"gpt-6.1-sol", OpenAI, "gpt-6.1-sol", false},
		{"gemini-3.1-flash-lite", Google, "gemini-3.1-flash-lite", false},
		{"qwen3:4b", Ollama, "qwen3:4b", false},
		{"my-local-model:latest", Ollama, "my-local-model:latest", false},
		{"openai/my-finetune", OpenAI, "my-finetune", false},
		{"ollama/llama3.1:8b", Ollama, "llama3.1:8b", false},
		{"hf.co/someone/qwen3:latest", Ollama, "hf.co/someone/qwen3:latest", false},
		{"mystery", "", "mystery", true},
	} {
		p, bare, err := Infer(tt.model)
		if p != tt.provider || bare != tt.bare || (err != nil) != tt.err {
			t.Errorf("Infer(%q) = %q, %q, %v; want %q, %q, err=%v", tt.model, p, bare, err, tt.provider, tt.bare, tt.err)
		}
		if tt.err && !errors.Is(err, ErrUnknownProvider) {
			t.Errorf("Infer(%q) error %v must match ErrUnknownProvider", tt.model, err)
		}
	}
}

func TestBuildSelectsAdapterAndCredentials(t *testing.T) {
	keys := map[string]string{"ANTHROPIC_API_KEY": "a", "OPENAI_API_KEY": "o", "GEMINI_API_KEY": "g"}
	for _, tt := range []struct {
		name     string
		cfg      Config
		provider string
		model    string
		check    func(error) bool // nil means success
	}{
		{name: "anthropic by name", cfg: Config{Model: "claude-haiku-5-5", Getenv: env(keys)}, provider: Anthropic, model: "claude-haiku-5-5"},
		{name: "openai by name", cfg: Config{Model: "gpt-6-luna", Getenv: env(keys)}, provider: OpenAI, model: "gpt-6-luna"},
		{name: "google reads the fallback key", cfg: Config{Model: "gemini-3.1-flash-lite", Getenv: env(keys)}, provider: Google, model: "gemini-3.1-flash-lite"},
		{name: "ollama needs no key", cfg: Config{Model: "qwen3:4b", Getenv: env(nil)}, provider: Ollama, model: "qwen3:4b"},
		{name: "explicit provider strips its prefix", cfg: Config{Provider: "OpenAI", Model: "openai/gpt-6-luna", Getenv: env(keys)}, provider: OpenAI, model: "gpt-6-luna"},
		{name: "explicit provider for an unlisted model", cfg: Config{Provider: Ollama, Model: "custom", Getenv: env(nil)}, provider: Ollama, model: "custom"},
		{name: "compatible server without key", cfg: Config{Provider: OpenAI, Model: "local", BaseURL: "http://127.0.0.1:1", Getenv: env(nil)}, provider: OpenAI, model: "local"},
		{name: "missing key", cfg: Config{Model: "claude-haiku-5-5", Getenv: env(nil)}, check: types.IsAuth},
		{name: "unknown provider", cfg: Config{Provider: "acme", Model: "x"}, check: func(err error) bool { return errors.Is(err, ErrUnknownProvider) }},
		{name: "uninferable model", cfg: Config{Model: "mystery", Getenv: env(keys)}, check: func(err error) bool { return errors.Is(err, ErrUnknownProvider) }},
		{name: "empty model", cfg: Config{Provider: OpenAI, Getenv: env(keys)}, check: func(err error) bool { return err != nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Build(context.Background(), tt.cfg)
			if tt.check != nil {
				if err == nil || !tt.check(err) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := p.(types.NamedProvider).Name(); got != tt.provider {
				t.Errorf("provider = %s, want %s", got, tt.provider)
			}
			if got := p.(types.ModelProvider).Model(); got != tt.model {
				t.Errorf("model = %s, want %s", got, tt.model)
			}
		})
	}
}

func TestBuildRejectsWhatItCannotSend(t *testing.T) {
	keys := env(map[string]string{"ANTHROPIC_API_KEY": "a", "OPENAI_API_KEY": "o", "GOOGLE_API_KEY": "g"})
	for _, tt := range []struct {
		name string
		cfg  Config
	}{
		{"seed on anthropic", Config{Model: "claude-sonnet-4-5", Options: types.RequestOptions{Seed: ptr[int64](1)}}},
		{"fractional top_k", Config{Model: "claude-sonnet-4-5", Options: types.RequestOptions{TopK: ptr(2.5)}}},
		{"top_k on openai", Config{Model: "gpt-4o", Options: types.RequestOptions{TopK: ptr(4.0)}}},
		{"temperature on a reasoning model", Config{Model: "o3", Options: types.RequestOptions{Temperature: ptr(0.2)}}},
		{"base URL on google", Config{Model: "gemini-2.5-flash", BaseURL: "http://x"}},
		{"two reasoning controls on google", Config{Model: "gemini-2.5-flash", Options: types.RequestOptions{ReasoningBudget: ptr[int64](1024), ReasoningEffort: ptr("low")}}},
		{"parallel tools on google", Config{Model: "gemini-2.5-flash", Options: types.RequestOptions{ParallelTools: ptr(false)}}},
		{"reasoning effort on ollama", Config{Model: "qwen3:4b", Options: types.RequestOptions{ReasoningEffort: ptr("low")}}},
		{"required tool choice on ollama", Config{Model: "qwen3:4b", Options: types.RequestOptions{ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceRequired}}}},
		{"api key on ollama", Config{Model: "qwen3:4b", APIKey: "k"}},
		{"server tools on openai", Config{Model: "gpt-4o", ServerTools: []types.ServerTool{types.WebSearchTool(1)}}},
		{"undeclared server tool", Config{Model: "claude-3-5-sonnet", ServerTools: []types.ServerTool{types.WebSearchTool(1)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.Getenv = keys
			if _, err := Build(context.Background(), tt.cfg); !errors.Is(err, types.ErrInvalidModelConfig) {
				t.Fatalf("err = %v, want ErrInvalidModelConfig", err)
			}
		})
	}
}

// recorder captures the first request body and answers with a fixed body.
func recorder(t *testing.T, contentType, reply string) (*httptest.Server, *map[string]any) {
	t.Helper()
	body := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, &body
}

func TestBuildAppliesOptions(t *testing.T) {
	const anthropicReply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const openaiReply = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":0,\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	const ollamaReply = `{"model":"qwen3:4b","done":true}` + "\n"
	tools := []types.ToolDef{{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}}}
	opts := types.RequestOptions{Temperature: ptr(0.3), MaxOutputTokens: ptr[int64](256),
		ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}}

	for _, tt := range []struct {
		name, model, contentType, reply string
		want                            map[string]string // path in the body -> fmt.Sprint of the value
	}{
		{"anthropic", "claude-sonnet-4-5", "text/event-stream", anthropicReply,
			map[string]string{"temperature": "0.3", "max_tokens": "256", "tool_choice": "map[name:lookup type:tool]"}},
		{"openai", "gpt-4o", "text/event-stream", openaiReply,
			map[string]string{"temperature": "0.3", "max_completion_tokens": "256", "tool_choice": "map[function:map[name:lookup] type:function]"}},
		{"ollama", "qwen3:4b", "application/x-ndjson", ollamaReply,
			map[string]string{"options": "map[num_predict:256 temperature:0.3]"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, body := recorder(t, tt.contentType, tt.reply)
			key := "k"
			if tt.name == "ollama" {
				key = "" // the ollama adapter sends no key, so Build rejects one
			}
			p, err := Build(context.Background(), Config{Model: tt.model, APIKey: key, BaseURL: server.URL, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			ch, err := p.ChatStream(context.Background(), []types.Message{types.NewUserMessage("hi")}, tools)
			if err != nil {
				t.Fatal(err)
			}
			for d := range ch {
				if e, ok := d.(types.ErrorDelta); ok {
					t.Fatal(e.Error)
				}
			}
			for k, want := range tt.want {
				if got := fmt.Sprint((*body)[k]); got != want {
					t.Errorf("%s = %s, want %s", k, got, want)
				}
			}
		})
	}
}

func TestOllamaHostResolution(t *testing.T) {
	for _, tt := range []struct {
		name, baseURL, env, want string
	}{
		{"default", "", "", DefaultOllamaHost},
		{"environment without scheme", "", "10.0.0.2:11434", "http://10.0.0.2:11434"},
		{"base URL wins", "https://gpu.internal", "10.0.0.2:11434", "https://gpu.internal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Build(context.Background(), Config{Model: "qwen3:4b", BaseURL: tt.baseURL,
				Getenv: env(map[string]string{"OLLAMA_HOST": tt.env})})
			if err != nil {
				t.Fatal(err)
			}
			if host := p.(*ollama.Adapter).Client.Host; !strings.EqualFold(host, tt.want) {
				t.Errorf("host = %s, want %s", host, tt.want)
			}
		})
	}
}
