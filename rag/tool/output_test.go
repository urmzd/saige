package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/tool"
	"github.com/urmzd/saige/rag/types"
)

func findTool(t *testing.T, tools []agenttypes.Tool, name string) agenttypes.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Definition().Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

// recordingPipeline records the search config of the last Search call.
type recordingPipeline struct {
	mockPipeline
	cfg types.SearchConfig
}

func (r *recordingPipeline) Search(ctx context.Context, q string, opts ...types.SearchOption) (*types.SearchPipelineResult, error) {
	r.cfg = types.SearchConfig{}
	for _, o := range opts {
		o(&r.cfg)
	}
	return r.mockPipeline.Search(ctx, q, opts...)
}

func TestSearchToolPartialAndLimits(t *testing.T) {
	hits := &types.SearchPipelineResult{Hits: []types.SearchHit{{
		Variant: types.ContentVariant{UUID: "v1", ContentType: types.ContentText},
		Score:   0.03,
	}}}
	tests := []struct {
		name         string
		err          error
		result       *types.SearchPipelineResult
		limit        float64
		wantErr      bool
		wantDegraded bool
		wantLimit    int
	}{
		{name: "complete", result: hits, limit: 5, wantLimit: 5},
		{name: "partial keeps hits", result: hits, err: fmt.Errorf("%w: graph down", types.ErrPartialSearch), wantDegraded: true},
		{name: "fatal error", err: errors.New("store down"), wantErr: true},
		{name: "limit clamped", result: hits, limit: 1000, wantLimit: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp := &recordingPipeline{mockPipeline: mockPipeline{searchResult: tt.result, searchErr: tt.err}}
			args := map[string]any{"query": "q"}
			if tt.limit > 0 {
				args["limit"] = tt.limit
			}
			out, err := findTool(t, tool.NewTools(mp), "rag_search").Execute(context.Background(), args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if tt.wantLimit > 0 && mp.cfg.Limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", mp.cfg.Limit, tt.wantLimit)
			}
			if !tt.wantDegraded {
				var list []map[string]any
				if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 1 {
					t.Fatalf("output %s: want a one-hit array (%v)", out, err)
				}
				return
			}
			var obj struct {
				Hits     []map[string]any `json:"hits"`
				Degraded bool             `json:"degraded"`
				Warnings []string         `json:"warnings"`
			}
			if err := json.Unmarshal([]byte(out), &obj); err != nil {
				t.Fatal(err)
			}
			if !obj.Degraded || len(obj.Hits) != 1 || len(obj.Warnings) != 1 || !strings.Contains(obj.Warnings[0], "graph down") {
				t.Errorf("output = %s, want degraded hits with a warning", out)
			}
		})
	}
}

func bigVariant(uuid string) types.ContentVariant {
	emb := make([]float32, 1536)
	for i := range emb {
		emb[i] = 0.123456
	}
	return types.ContentVariant{
		UUID:        uuid,
		ContentType: types.ContentImage,
		MIMEType:    "image/png",
		Data:        make([]byte, 1<<20),
		Text:        strings.Repeat("é", 20<<10), // 40 KiB of two-byte runes
		Embedding:   emb,
	}
}

func TestLookupToolOmitsEmbeddingsAndBytes(t *testing.T) {
	mp := &mockPipeline{lookupResult: &types.SearchHit{
		Variant:    bigVariant("v1"),
		Score:      1,
		Provenance: types.Provenance{DocumentUUID: "d1", SectionUUID: "s1"},
	}}
	out, err := findTool(t, tool.NewTools(mp), "rag_lookup").Execute(context.Background(), map[string]any{"variant_uuid": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "embedding") || strings.Contains(out, `"data"`) {
		t.Error("lookup output must not carry embeddings or raw data")
	}
	if len(out) > 20<<10 {
		t.Errorf("lookup output is %d bytes, want it within the default cap", len(out))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["data_bytes"] != float64(1<<20) || got["text_truncated"] != true || got["mime_type"] != "image/png" {
		t.Errorf("lookup output fields = %v", got)
	}
	if text, _ := got["text"].(string); !strings.HasPrefix(text, "é") || strings.ContainsRune(text, '�') {
		t.Error("truncation split a rune")
	}
}

func TestLookupToolSection(t *testing.T) {
	mp := &mockPipeline{
		lookupResult: &types.SearchHit{
			Variant:    types.ContentVariant{UUID: "v1", Text: "child"},
			Provenance: types.Provenance{DocumentUUID: "d1", SectionUUID: "s1"},
		},
		reconstructResult: &types.Document{UUID: "d1", Sections: []types.Section{{
			UUID: "s1",
			Variants: []types.ContentVariant{
				{UUID: "v1", Text: "child"},
				{UUID: "v2", Text: "sibling"},
			},
		}}},
	}
	tests := []struct {
		section bool
		want    string
	}{
		{section: false, want: "child"},
		{section: true, want: "child\n\nsibling"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.section), func(t *testing.T) {
			out, err := findTool(t, tool.NewTools(mp), "rag_lookup").Execute(context.Background(),
				map[string]any{"variant_uuid": "v1", "section": tt.section})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got["text"] != tt.want {
				t.Errorf("text = %q, want %q", got["text"], tt.want)
			}
		})
	}
}

func TestReconstructToolOutputCaps(t *testing.T) {
	doc := &types.Document{UUID: "d1", Title: "T", Sections: []types.Section{
		{UUID: "s1", Variants: []types.ContentVariant{bigVariant("v1"), bigVariant("v2")}},
		{UUID: "s2", Variants: []types.ContentVariant{bigVariant("v3")}},
	}}
	tests := []struct {
		name          string
		opts          []tool.Option
		maxBytes      int
		wantTruncated bool
	}{
		{name: "defaults", maxBytes: 80 << 10, wantTruncated: true},
		{name: "custom caps", opts: []tool.Option{tool.WithOutputLimits(100, 150)}, maxBytes: 2 << 10, wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp := &mockPipeline{reconstructResult: doc}
			out, err := findTool(t, tool.NewTools(mp, tt.opts...), "rag_reconstruct").Execute(context.Background(), map[string]any{"document_uuid": "d1"})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "embedding") || strings.Contains(out, `"data"`) {
				t.Error("reconstruct output must not carry embeddings or raw data")
			}
			if len(out) > tt.maxBytes {
				t.Errorf("output is %d bytes, want at most %d", len(out), tt.maxBytes)
			}
			var got struct {
				UUID      string `json:"document_uuid"`
				Truncated bool   `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if got.UUID != "d1" || got.Truncated != tt.wantTruncated {
				t.Errorf("document = %+v", got)
			}
		})
	}
}

func TestReconstructToolChargesEveryEntry(t *testing.T) {
	bigMeta := func(prefix string, n int) map[string]string {
		m := make(map[string]string, n)
		for i := range n {
			m[fmt.Sprintf("%s%04d", prefix, i)] = strings.Repeat("x", 200)
		}
		return m
	}
	binaryVariants := func(n int, meta map[string]string) []types.ContentVariant {
		vs := make([]types.ContentVariant, n)
		for i := range vs {
			vs[i] = types.ContentVariant{
				UUID:        fmt.Sprintf("img-%05d", i),
				ContentType: types.ContentImage,
				MIMEType:    "image/png",
				Data:        make([]byte, 16),
				Metadata:    meta,
			}
		}
		return vs
	}
	tests := []struct {
		name  string
		doc   *types.Document
		opts  []tool.Option
		limit int
	}{
		{
			name:  "many binary variants",
			doc:   &types.Document{UUID: "d1", Sections: []types.Section{{UUID: "s1", Variants: binaryVariants(5000, nil)}}},
			limit: 64 << 10,
		},
		{
			name:  "large variant metadata",
			doc:   &types.Document{UUID: "d1", Sections: []types.Section{{UUID: "s1", Variants: binaryVariants(500, bigMeta("k", 50))}}},
			limit: 64 << 10,
		},
		{
			name:  "large document metadata",
			doc:   &types.Document{UUID: "d1", Metadata: bigMeta("doc", 2000), Sections: []types.Section{{UUID: "s1", Variants: binaryVariants(10, nil)}}},
			limit: 64 << 10,
		},
		{
			name:  "escaped text and small caps",
			doc:   &types.Document{UUID: "d1", Sections: []types.Section{{UUID: "s1", Variants: []types.ContentVariant{{UUID: "v1", Text: strings.Repeat("<&>\"", 4000)}}}}},
			opts:  []tool.Option{tool.WithOutputLimits(1000, 600)},
			limit: 600,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp := &mockPipeline{reconstructResult: tt.doc}
			out, err := findTool(t, tool.NewTools(mp, tt.opts...), "rag_reconstruct").Execute(context.Background(), map[string]any{"document_uuid": "d1"})
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > tt.limit {
				t.Errorf("output is %d bytes, want at most %d", len(out), tt.limit)
			}
			var got struct {
				Truncated bool `json:"truncated"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if !got.Truncated {
				t.Error("output must be marked truncated")
			}
		})
	}
}

func TestLookupToolChargesMetadata(t *testing.T) {
	meta := make(map[string]string, 1000)
	for i := range 1000 {
		meta[fmt.Sprintf("k%04d", i)] = strings.Repeat("y", 100)
	}
	mp := &mockPipeline{lookupResult: &types.SearchHit{
		Variant:    types.ContentVariant{UUID: "v1", ContentType: types.ContentText, Text: "hello", Metadata: meta},
		Provenance: types.Provenance{DocumentUUID: "d1", SectionUUID: "s1"},
	}}
	out, err := findTool(t, tool.NewTools(mp, tool.WithOutputLimits(0, 4<<10)), "rag_lookup").Execute(context.Background(), map[string]any{"variant_uuid": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4<<10 {
		t.Errorf("output is %d bytes, want at most %d", len(out), 4<<10)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["metadata_truncated"] != true || got["text"] != "hello" {
		t.Errorf("output fields: metadata_truncated=%v text=%v", got["metadata_truncated"], got["text"])
	}
}
