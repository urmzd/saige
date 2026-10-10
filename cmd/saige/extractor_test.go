package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/saige/agent/agenttest"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag"
	"github.com/urmzd/saige/rag/embedderregistry"
	"github.com/urmzd/saige/rag/knowledge"
)

func TestParseExtraction(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		entities  int
		relations int
		wantErr   bool
	}{
		{
			name:      "plain JSON",
			raw:       `{"entities":[{"name":"Alice","type":"Person","summary":"x"},{"name":"Acme","type":"Org","summary":"y"}],"relations":[{"source":"Alice","target":"Acme","type":"works_at","fact":"Alice works at Acme"}]}`,
			entities:  2,
			relations: 1,
		},
		{
			name:     "code fence and prose",
			raw:      "Here you go:\n```json\n{\"entities\":[{\"name\":\"Alice\",\"type\":\"Person\",\"summary\":\"x\"}],\"relations\":[]}\n```",
			entities: 1,
		},
		{
			name:      "drops unnamed entities and dangling relations",
			raw:       `{"entities":[{"name":" ","type":"Person"},{"name":"Bob"}],"relations":[{"source":"Bob","target":""}]}`,
			entities:  1,
			relations: 0,
		},
		{name: "no JSON", raw: "I cannot help with that.", wantErr: true},
		{name: "invalid JSON", raw: `{"entities": [}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ents, rels, err := parseExtraction(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExtraction: %v", err)
			}
			if len(ents) != tt.entities || len(rels) != tt.relations {
				t.Fatalf("got %d entities, %d relations; want %d, %d", len(ents), len(rels), tt.entities, tt.relations)
			}
		})
	}
}

func TestProviderExtractor(t *testing.T) {
	answer := `{"entities":[{"name":"Alice","type":"Person","summary":"presenter"}],"relations":[]}`
	tests := []struct {
		name     string
		provider func() (agenttypes.Provider, error)
		entities int
		wantErr  string
	}{
		{
			name: "uses the chat provider",
			provider: func() (agenttypes.Provider, error) {
				return &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{agenttest.TextResponse(answer)}}, nil
			},
			entities: 1,
		},
		{
			name: "provider construction error",
			provider: func() (agenttypes.Provider, error) {
				return nil, errors.New("ANTHROPIC_API_KEY is required")
			},
			wantErr: "ANTHROPIC_API_KEY",
		},
		{
			name: "stream error",
			provider: func() (agenttypes.Provider, error) {
				return &agenttest.ScriptedProvider{Responses: [][]agenttypes.Delta{{agenttypes.ErrorDelta{Error: errors.New("boom")}}}}, nil
			},
			wantErr: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ents, _, err := newProviderExtractor(tt.provider).Extract(context.Background(), "Alice presented the roadmap.")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(ents) != tt.entities {
				t.Fatalf("entities = %d, want %d", len(ents), tt.entities)
			}
		})
	}
}

// TestKnowledgeOptionsWireExtractor guards against building a graph that can
// search but never ingest: the extractor is always set, and the embedder is
// set whenever the command embeds. Commands that never embed (kg graph, kg
// node) must build with a provider that has no embedding API.
func TestKnowledgeOptionsWireExtractor(t *testing.T) {
	clearProviderEnv(t)
	tests := []struct {
		name          string
		provider      string
		embedProvider string
		withEmbedder  bool
		wantErr       bool
	}{
		{name: "ollama", provider: providerOllama, withEmbedder: true},
		{name: "anthropic chat with ollama embeddings", provider: providerAnthropic, embedProvider: providerOllama, withEmbedder: true},
		{name: "anthropic has no embeddings", provider: providerAnthropic, withEmbedder: true, wantErr: true},
		{name: "anthropic without embedder for graph and node", provider: providerAnthropic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newTestFlags(tt.provider, "", tt.embedProvider, "")
			opts, err := knowledgeOptions(context.Background(), cf, tt.withEmbedder)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("knowledgeOptions: %v", err)
			}
			var cfg knowledge.Config
			for _, o := range opts {
				o(&cfg)
			}
			if cfg.Extractor == nil {
				t.Fatal("extractor not configured: kg ingest would fail with ErrNoExtractor")
			}
			if (cfg.Embedder != nil) != tt.withEmbedder {
				t.Fatalf("embedder configured = %v, want %v", cfg.Embedder != nil, tt.withEmbedder)
			}
		})
	}
}

// TestNewRAGPipelineEmbedderOnlyWhenNeeded checks that rag lookup and delete
// build with a provider that has no embedding API, while search and ingest
// still require one.
func TestNewRAGPipelineEmbedderOnlyWhenNeeded(t *testing.T) {
	clearProviderEnv(t)
	tests := []struct {
		name          string
		provider      string
		embedProvider string
		withEmbedder  bool
		wantErr       bool
	}{
		{name: "anthropic lookup and delete", provider: providerAnthropic},
		{name: "anthropic search needs embeddings", provider: providerAnthropic, withEmbedder: true, wantErr: true},
		{name: "anthropic chat with ollama embeddings", provider: providerAnthropic, embedProvider: providerOllama, withEmbedder: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newTestFlags(tt.provider, "", tt.embedProvider, "")
			// The pool connects lazily, and building a pipeline sends nothing.
			pool, err := pgxpool.New(context.Background(), "postgres://localhost:1/none")
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			p, err := newRAGPipeline(context.Background(), pool, cf, tt.withEmbedder)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("newRAGPipeline: %v", err)
			}
			if p == nil {
				t.Fatal("nil pipeline")
			}
		})
	}
}

// TestRAGPipelineOptionsUseEmbeddings guards against a pipeline whose only
// retriever is an in-memory index that a fresh process starts with empty.
func TestRAGPipelineOptionsUseEmbeddings(t *testing.T) {
	var cfg rag.Config
	for _, o := range ragPipelineOptions(nil, embedderregistry.Text(embedFunc(nil))) {
		o(&cfg)
	}
	if cfg.Embedders == nil {
		t.Fatal("pipeline has no embedders, so search would have no vector retriever")
	}
}
