package ollama

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

var testTools = []types.ToolDef{
	{Name: "lookup", Parameters: types.ParameterSchema{Type: "object"}},
	{Name: "write", Parameters: types.ParameterSchema{Type: "object"}},
}

func TestToolChoiceEmulation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		choice  *types.ToolChoice
		tools   []types.ToolDef
		want    []string // tool names sent
		wantErr bool
	}{
		{name: "unset sends every tool", model: "qwen3:4b", tools: testTools, want: []string{"lookup", "write"}},
		{name: "auto sends every tool", model: "qwen3:4b", choice: &types.ToolChoice{Mode: types.ToolChoiceAuto}, tools: testTools, want: []string{"lookup", "write"}},
		{name: "none sends no tools", model: "qwen3:4b", choice: &types.ToolChoice{Mode: types.ToolChoiceNone}, tools: testTools},
		{name: "none lets a model without tool support run", model: "gemma3", choice: &types.ToolChoice{Mode: types.ToolChoiceNone}, tools: testTools},
		{name: "named sends only that tool", model: "qwen3:4b", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"}, tools: testTools, want: []string{"write"}},
		{name: "named tool not offered", model: "qwen3:4b", choice: &types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "missing"}, tools: testTools, wantErr: true},
		{name: "required cannot be emulated", model: "qwen3:4b", choice: &types.ToolChoice{Mode: types.ToolChoiceRequired}, tools: testTools, wantErr: true},
		{name: "unknown mode", model: "qwen3:4b", choice: &types.ToolChoice{Mode: "maybe"}, tools: testTools, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, got := captureChat(t, ChatChunk{Done: true})
			var opts []AdapterOption
			if tc.choice != nil {
				opts = append(opts, WithToolChoice(*tc.choice))
			}
			a := NewAdapter(NewClient(server.URL, tc.model, ""), opts...)
			ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("go"))}, Tools: tc.tools})
			if tc.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want a local configuration error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			var names []string
			for _, tool := range got.Tools {
				names = append(names, tool.Function.Name)
			}
			if !slices.Equal(names, tc.want) {
				t.Fatalf("tools sent = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestWithModelKeepsToolChoice(t *testing.T) {
	a := NewAdapter(NewClient("http://unused", "qwen3:4b", ""), WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired}))
	if err := must.Get(a.WithTarget(types.ModelTarget("llama3.1"))).(*Adapter).Validate(); err == nil {
		t.Fatal("a switched adapter must keep, and still reject, the configured choice")
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"models":[
			{"name":"qwen3:4b","model":"qwen3:4b","modified_at":"2025-05-01T10:00:00Z","size":2600000000,"details":{"family":"qwen3","families":["qwen3"]}},
			{"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","size":274000000,"details":{"family":"nomic-bert"}},
			{"name":"custom-encoder","model":"custom-encoder","details":{"family":"bert"}}]}`)
	}))
	t.Cleanup(server.Close)
	models, err := NewAdapter(NewClient(server.URL+"/", "qwen3:4b", "")).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	if m := models[0]; m.ID != "qwen3:4b" || m.Embedding || m.SizeBytes != 2600000000 || m.Created.IsZero() {
		t.Errorf("chat model = %+v", m)
	}
	if !models[1].Embedding || !models[2].Embedding {
		t.Errorf("embedding detection = %+v", models[1:])
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	if _, err := NewClient(down.URL, "m", "").ListModels(context.Background()); !types.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
}
