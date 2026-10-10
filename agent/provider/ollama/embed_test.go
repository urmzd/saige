package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func TestOllamaEmbedder_Embed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}

		var req EmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		// Return a deterministic embedding based on input length.
		vec := make([]float32, 3)
		vec[0] = float32(len(req.Input))
		resp := EmbedResponse{Embeddings: [][]float32{vec}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := must.Get(NewClient(Config{Host: server.URL, Model: "test-model", EmbeddingModel: "test-embed"}))
	embedder := NewEmbedder(client)

	results, err := embedder.Embed(context.Background(), []string{"hello", "world!"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0][0] != 5 { // len("hello") = 5
		t.Errorf("results[0][0] = %f, want 5", results[0][0])
	}
	if results[1][0] != 6 { // len("world!") = 6
		t.Errorf("results[1][0] = %f, want 6", results[1][0])
	}
}

func TestOllamaEmbedErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{"overloaded", http.StatusServiceUnavailable, true},
		{"rate limited", http.StatusTooManyRequests, true},
		{"bad request", http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":"busy"}`, tc.status)
			}))
			defer server.Close()
			_, err := must.Get(NewClient(Config{Host: server.URL, Model: "m", EmbeddingModel: "e"})).Embed(context.Background(), "x")
			if err == nil || types.IsTransient(err) != tc.wantTransient {
				t.Fatalf("err = %v, transient want %v", err, tc.wantTransient)
			}
		})
	}
}
