package preset_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

// pulled is a ListLocal that reports the given models as pulled.
func pulled(ids ...string) func(context.Context, provider.Config) ([]catalog.RemoteModel, error) {
	return func(context.Context, provider.Config) ([]catalog.RemoteModel, error) {
		var out []catalog.RemoteModel
		for _, id := range ids {
			out = append(out, catalog.RemoteModel{ID: id, Embedding: strings.Contains(id, "embed")})
		}
		return out, nil
	}
}

func reachable(context.Context, provider.Config) error { return nil }

const localDoc = `{"version":1,"presets":{
	"local":{"chain":[{"id":"primary","provider":"ollama","model":"qwen3.5:4b","local_fallback":true}]},
	"opt":{"chain":[
		{"provider":"ollama","model":"qwen3.5:4b","local_fallback":true,"optional":true},
		{"provider":"openai","model":"gpt-6-luna"}]},
	"pinned":{"chain":[{"provider":"ollama","model":"qwen3.5:4b"}]},
	"child":{"extends":"local"}}}`

func TestLocalFallbackServesAPulledModel(t *testing.T) {
	cat := overlay(t, localDoc)
	tests := []struct {
		name   string
		pulled []string
		want   string
		warn   bool
	}{
		{"named model pulled", []string{"gemma3:latest", "qwen3.5:4b"}, "qwen3.5:4b", false},
		{"latest tag", []string{"qwen3.5:4b:latest"}, "qwen3.5:4b", false},
		{"prefers a tool-calling model", []string{"nomic-embed-text:latest", "llava:latest", "llama3.2:latest"}, "llama3.2:latest", true},
		{"any chat model", []string{"nomic-embed-text:latest", "some-model:7b"}, "some-model:7b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []types.PresetName{"local", "child"} {
				rec := newRecorder()
				b, err := preset.Build(context.Background(), cat, name, nil,
					preset.Options{Getenv: everyone, Factory: rec.factory, Probe: reachable, ListLocal: pulled(tt.pulled...)})
				if err != nil {
					t.Fatal(err)
				}
				rp, _ := b.Resolved(name)
				if len(rp.Chain) != 1 || string(rp.Chain[0].Model) != tt.want || rp.Chain[0].ProfileID != types.ProfileID(name)+"/primary" {
					t.Fatalf("%s: chain %+v", name, rp.Chain)
				}
				if _, ok := rec.configs[tt.want]; !ok {
					t.Fatalf("%s: built %v, want %s", name, rec.configs, tt.want)
				}
				warned := false
				for _, w := range b.Warnings() {
					warned = warned || w.Code == preset.WarnLocalModel
				}
				if warned != tt.warn {
					t.Fatalf("%s: warnings %v", name, b.Warnings())
				}
			}
		})
	}
}

func TestLocalFallbackWithNothingPulled(t *testing.T) {
	cat := overlay(t, localDoc)
	opts := preset.Options{Getenv: everyone, Factory: newRecorder().factory, Probe: reachable, ListLocal: pulled("nomic-embed-text:latest")}

	_, err := preset.Build(context.Background(), cat, "local", nil, opts)
	if !errors.Is(err, preset.ErrNoLocalModel) || !strings.Contains(err.Error(), "ollama pull qwen3.5:4b") {
		t.Fatalf("required entry: %v", err)
	}

	b, err := preset.Build(context.Background(), cat, "opt", nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rp, _ := b.Resolved("opt"); len(rp.Chain) != 1 || rp.Chain[0].Provider != "openai" {
		t.Fatalf("optional entry kept: %+v", rp.Chain)
	}

	rp, err := cat.Resolve("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := preset.Available(context.Background(), rp.Chain[0], opts); !errors.Is(err, preset.ErrNoLocalModel) {
		t.Fatalf("Available: %v", err)
	}
	opts.ListLocal = pulled("gemma3:latest")
	if err := preset.Available(context.Background(), rp.Chain[0], opts); err != nil {
		t.Fatalf("Available with another model pulled: %v", err)
	}
}

func TestLocalFallbackLeavesOtherEntriesAlone(t *testing.T) {
	cat := overlay(t, localDoc)
	listErr := func(context.Context, provider.Config) ([]catalog.RemoteModel, error) {
		return nil, errors.New("connection refused")
	}
	for _, tc := range []struct {
		name types.PresetName
		list func(context.Context, provider.Config) ([]catalog.RemoteModel, error)
	}{
		{"pinned", pulled("gemma3:latest")}, // no local_fallback: the named model is kept
		{"local", listErr},                  // the server cannot be listed: left as written
	} {
		rec := newRecorder()
		b, err := preset.Build(context.Background(), cat, tc.name, nil,
			preset.Options{Getenv: everyone, Factory: rec.factory, Probe: reachable, ListLocal: tc.list})
		if err != nil {
			t.Fatal(err)
		}
		if rp, _ := b.Resolved(tc.name); rp.Chain[0].Model != "qwen3.5:4b" {
			t.Fatalf("%s: %+v", tc.name, rp.Chain)
		}
	}
}

func TestLocalFallbackOnlyForOllama(t *testing.T) {
	layer, err := catalog.Load(strings.NewReader(`{"version":1,"presets":{"bad":{"chain":[
		{"provider":"openai","model":"gpt-6-luna","local_fallback":true}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Merging validates every preset, so the entry is refused there.
	if _, err := catalog.Merge(catalog.Default(), layer); !errors.Is(err, catalog.ErrInvalidCatalog) || !strings.Contains(err.Error(), "local_fallback") {
		t.Fatalf("got %v", err)
	}
}

func TestListOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "qwen3.5:4b"}, {"name": "nomic-embed-text:latest"}}})
	}))
	defer srv.Close()
	models, err := preset.ListOllama(context.Background(), provider.Config{Provider: provider.Ollama, BaseURL: srv.URL})
	if err != nil || len(models) != 2 || models[0].ID != "qwen3.5:4b" || models[0].Embedding || !models[1].Embedding {
		t.Fatalf("%+v %v", models, err)
	}
	if models, err := preset.ListOllama(context.Background(), provider.Config{Provider: "openai"}); models != nil || err != nil {
		t.Fatalf("non-ollama: %v %v", models, err)
	}
}
