package tool_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/urmzd/saige/rag/tool"
	"github.com/urmzd/saige/rag/types"
)

type mockPipeline struct {
	searchResult      *types.SearchPipelineResult
	searchErr         error
	lookupResult      *types.SearchHit
	lookupErr         error
	deleteErr         error
	reconstructResult *types.Document
	reconstructErr    error
}

func (m *mockPipeline) Ingest(_ context.Context, _ *types.RawDocument) (*types.IngestResult, error) {
	return &types.IngestResult{DocumentUUID: "new-doc"}, nil
}
func (m *mockPipeline) Search(_ context.Context, _ string, _ ...types.SearchOption) (*types.SearchPipelineResult, error) {
	return m.searchResult, m.searchErr
}
func (m *mockPipeline) Lookup(_ context.Context, _ string) (*types.SearchHit, error) {
	return m.lookupResult, m.lookupErr
}
func (m *mockPipeline) Update(_ context.Context, _ string, _ *types.RawDocument) (*types.IngestResult, error) {
	return &types.IngestResult{DocumentUUID: "updated-doc"}, nil
}
func (m *mockPipeline) Delete(_ context.Context, _ string) error {
	return m.deleteErr
}
func (m *mockPipeline) Reconstruct(_ context.Context, _ string) (*types.Document, error) {
	return m.reconstructResult, m.reconstructErr
}
func (m *mockPipeline) Close(_ context.Context) error { return nil }

func TestSearchToolExecute(t *testing.T) {
	mp := &mockPipeline{
		searchResult: &types.SearchPipelineResult{
			Hits: []types.SearchHit{
				{
					Variant:    types.ContentVariant{UUID: "v1", ContentType: types.ContentText},
					Score:      0.95,
					Provenance: types.Provenance{DocumentUUID: "d1", SourceURI: "http://example.com"},
				},
			},
		},
	}

	tools := tool.NewTools(mp)
	searchTool := tools[0] // SearchTool is first

	result, err := searchTool.Execute(context.Background(), map[string]any{
		"query": "test query",
		"limit": float64(5),
	})
	if err != nil {
		t.Fatal(err)
	}

	var hits []map[string]any
	if err := json.Unmarshal([]byte(result), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0]["variant_uuid"] != "v1" {
		t.Errorf("expected variant_uuid v1, got %v", hits[0]["variant_uuid"])
	}
}

func TestSearchToolMissingQuery(t *testing.T) {
	tools := tool.NewTools(&mockPipeline{})
	searchTool := tools[0]

	_, err := searchTool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestLookupToolExecute(t *testing.T) {
	mp := &mockPipeline{
		lookupResult: &types.SearchHit{
			Variant: types.ContentVariant{UUID: "v1", Text: "content"},
			Score:   1.0,
		},
	}

	tools := tool.NewTools(mp)
	lookupTool := tools[1]

	result, err := lookupTool.Execute(context.Background(), map[string]any{
		"variant_uuid": "v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestLookupToolMissingUUID(t *testing.T) {
	tools := tool.NewTools(&mockPipeline{})
	lookupTool := tools[1]

	_, err := lookupTool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing variant_uuid")
	}
}

func TestDeleteToolExecute(t *testing.T) {
	tools := tool.NewTools(&mockPipeline{})
	deleteTool := tools[3]

	result, err := deleteTool.Execute(context.Background(), map[string]any{
		"document_uuid": "d1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result != `{"status":"deleted"}` {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestDeleteToolError(t *testing.T) {
	mp := &mockPipeline{deleteErr: fmt.Errorf("delete failed")}
	tools := tool.NewTools(mp)
	deleteTool := tools[3]

	_, err := deleteTool.Execute(context.Background(), map[string]any{
		"document_uuid": "d1",
	})
	if err == nil {
		t.Fatal("expected error from delete failure")
	}
}

func TestDeleteToolMissingUUID(t *testing.T) {
	tools := tool.NewTools(&mockPipeline{})
	deleteTool := tools[3]

	_, err := deleteTool.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing document_uuid")
	}
}

func TestReconstructToolExecute(t *testing.T) {
	mp := &mockPipeline{
		reconstructResult: &types.Document{UUID: "d1", Title: "Test"},
	}
	tools := tool.NewTools(mp)
	reconstructTool := tools[4]

	result, err := reconstructTool.Execute(context.Background(), map[string]any{
		"document_uuid": "d1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestToolDefinitions(t *testing.T) {
	tools := tool.NewTools(&mockPipeline{})

	expectedNames := []string{"rag_search", "rag_lookup", "rag_update", "rag_delete", "rag_reconstruct"}
	if len(tools) != len(expectedNames) {
		t.Fatalf("expected %d tools, got %d", len(expectedNames), len(tools))
	}

	for i, tool := range tools {
		def := tool.Definition()
		if def.Name != expectedNames[i] {
			t.Errorf("tool %d: expected name %q, got %q", i, expectedNames[i], def.Name)
		}
		if def.Description == "" {
			t.Errorf("tool %d (%s): missing description", i, def.Name)
		}
	}
}
