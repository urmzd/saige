package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// TestAgentToolChoiceThroughOllama checks that a tool choice set on the agent
// reaches the Ollama adapter's emulation instead of being refused by the
// capability check, and that required is still rejected before any request.
func TestAgentToolChoiceThroughOllama(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		choice    types.ToolChoice
		wantTools []string // tools sent on the single request
		wantErr   bool
	}{
		{"named on a reasoning model", "qwen3.5:4b", types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "write"}, []string{"write"}, false},
		{"named on a tool model", "llama3.1", types.ToolChoice{Mode: types.ToolChoiceNamed, Name: "lookup"}, []string{"lookup"}, false},
		{"required cannot be emulated", "qwen3.5:4b", types.ToolChoice{Mode: types.ToolChoiceRequired}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu   sync.Mutex
				sent [][]string
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ollama.ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				var names []string
				for _, tool := range req.Tools {
					names = append(names, tool.Function.Name)
				}
				mu.Lock()
				sent = append(sent, names)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(ollama.ChatChunk{
					Message: ollama.ChatMessage{Role: "assistant", Content: "done"},
					Done:    true, DoneReason: "stop",
				})
			}))
			t.Cleanup(server.Close)

			choice := tt.choice
			a := agent.NewAgent(agent.AgentConfig{
				Provider:   ollama.NewAdapter(ollama.NewClient(server.URL, tt.model, "")),
				ToolChoice: &choice,
				Tools: types.NewToolRegistry(
					&agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "r"},
					&agenttest.MockTool{Def: types.ToolDef{Name: "write"}, Result: "w"},
				),
			})
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			agenttest.CollectDeltas(stream.Deltas())
			err := stream.Wait()

			mu.Lock()
			defer mu.Unlock()
			if tt.wantErr {
				if !errors.Is(err, types.ErrInvalidModelConfig) {
					t.Fatalf("err = %v, want ErrInvalidModelConfig", err)
				}
				if len(sent) != 0 {
					t.Fatalf("requests = %d, want none", len(sent))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(sent) != 1 || !slices.Equal(sent[0], tt.wantTools) {
				t.Fatalf("tools sent = %v, want one request with %v", sent, tt.wantTools)
			}
		})
	}
}
