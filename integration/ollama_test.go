package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// TestOllamaChatStream exercises the raw Provider contract: a streamed chat
// completion must produce text deltas and a final usage delta.
func TestOllamaChatStream(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 5*time.Minute)
	adapter := ollama.NewAdapter(client)

	rx, err := adapter.ChatStream(ctx, []types.Message{
		types.NewUserMessage("Reply with exactly one word: hello"),
	}, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var text strings.Builder
	var usage *types.UsageDelta
	for d := range rx {
		switch v := d.(type) {
		case types.TextContentDelta:
			text.WriteString(v.Content)
		case types.UsageDelta:
			usage = &v
		case types.ErrorDelta:
			t.Fatalf("stream error: %v", v.Error)
		}
	}

	if strings.TrimSpace(text.String()) == "" {
		t.Fatal("expected non-empty text response")
	}
	if usage == nil {
		t.Fatal("expected a UsageDelta on stream completion")
	}
	if usage.CompletionTokens <= 0 {
		t.Errorf("expected positive completion tokens, got %d", usage.CompletionTokens)
	}
	t.Logf("response: %q (prompt=%d completion=%d tokens)",
		strings.TrimSpace(text.String()), usage.PromptTokens, usage.CompletionTokens)
}

// TestOllamaStructuredOutput verifies schema-constrained generation returns
// parseable JSON matching the requested shape.
func TestOllamaStructuredOutput(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 5*time.Minute)
	adapter := ollama.NewAdapter(client)

	schema := &types.ParameterSchema{
		Type:     "object",
		Required: []string{"answer"},
		Properties: map[string]types.PropertyDef{
			"answer": {Type: "number", Description: "The numeric answer"},
		},
	}

	rx, err := adapter.ChatStreamWithSchema(ctx, []types.Message{
		types.NewUserMessage("What is 2 + 3? Respond in JSON."),
	}, nil, schema)
	if err != nil {
		t.Fatalf("ChatStreamWithSchema: %v", err)
	}

	var text strings.Builder
	for d := range rx {
		switch v := d.(type) {
		case types.TextContentDelta:
			text.WriteString(v.Content)
		case types.ErrorDelta:
			t.Fatalf("stream error: %v", v.Error)
		}
	}

	var out struct {
		Answer float64 `json:"answer"`
	}
	raw := strings.TrimSpace(text.String())
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("response is not valid JSON: %v\nraw: %s", err, raw)
	}
	if out.Answer != 5 {
		t.Errorf("expected answer 5, got %v (raw: %s)", out.Answer, raw)
	}
}

// TestOllamaEmbeddings verifies batch embedding: consistent dimensions and a
// sane similarity ordering (related texts closer than unrelated ones).
func TestOllamaEmbeddings(t *testing.T) {
	client := requireOllama(t)
	ctx := testContext(t, 5*time.Minute)
	embedder := ollama.NewEmbedder(client)

	texts := []string{
		"The cat sat on the mat.",
		"A kitten rested on the rug.",
		"Quarterly revenue grew by twelve percent.",
	}
	vecs, err := embedder.Embed(ctx, texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != len(texts) {
		t.Fatalf("expected %d vectors, got %d", len(texts), len(vecs))
	}
	dim := len(vecs[0])
	if dim == 0 {
		t.Fatal("expected non-empty embedding vectors")
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Fatalf("vector %d has dim %d, expected %d", i, len(v), dim)
		}
	}

	related := cosine(vecs[0], vecs[1])
	unrelated := cosine(vecs[0], vecs[2])
	t.Logf("dim=%d related=%.4f unrelated=%.4f", dim, related, unrelated)
	if related <= unrelated {
		t.Errorf("expected related texts (%.4f) to be more similar than unrelated (%.4f)", related, unrelated)
	}
}
