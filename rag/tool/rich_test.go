package tool_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/rag/tool"
	"github.com/urmzd/saige/rag/types"
)

func richTool(t *testing.T, tools []agenttypes.Tool, name string) agenttypes.RichTool {
	t.Helper()
	rt, ok := findTool(t, tools, name).(agenttypes.RichTool)
	if !ok {
		t.Fatalf("%s is not a rich tool", name)
	}
	return rt
}

func chartHit() types.SearchHit {
	return types.SearchHit{
		Variant: types.ContentVariant{UUID: "v-img", ContentType: types.ContentImage, MIMEType: "image/png",
			Data: []byte("png bytes"), Text: "Revenue by quarter chart"},
		Score: 0.8,
		Provenance: types.Provenance{DocumentUUID: "d2", DocumentTitle: "Q3 report", SourceURI: "file://q3.pdf",
			SectionUUID: "s9", SectionIndex: 4, SectionHeading: "Page 5"},
	}
}

func TestSearchToolReturnsCitationsAndMedia(t *testing.T) {
	text := types.SearchHit{
		Variant:    types.ContentVariant{UUID: "v-txt", ContentType: types.ContentText, Text: "Refunds take 30 days."},
		Score:      0.9,
		Provenance: types.Provenance{DocumentUUID: "d1", SectionUUID: "s1", SectionIndex: 0},
		Highlight:  &types.Highlight{Spans: []types.TextSpan{{Start: 0, End: 7}}},
	}
	mp := &mockPipeline{searchResult: &types.SearchPipelineResult{Hits: []types.SearchHit{text, chartHit()}}}
	tools := tool.NewTools(mp, tool.ReadOnly())
	res, err := richTool(t, tools, "rag_search").ExecuteRich(context.Background(), map[string]any{"query": "refunds"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 2 {
		t.Fatalf("citations = %+v", res.Citations)
	}
	c0, c1 := res.Citations[0], res.Citations[1]
	if c0.Kind != agenttypes.CitationRetrieval || c0.Title != "document d1" || c0.Producer != "rag_search" ||
		c0.Meta["variant_uuid"] != "v-txt" || c0.Meta["section_uuid"] != "s1" || c0.Start != -1 {
		t.Fatalf("citation 0 = %+v", c0)
	}
	if spans, _ := c0.Meta["spans"].([][2]int); len(spans) != 1 || spans[0] != [2]int{0, 7} {
		t.Fatalf("spans = %#v", c0.Meta["spans"])
	}
	if c1.URI != "file://q3.pdf" || c1.Title != "Q3 report" || c1.Meta["section_index"] != 4 {
		t.Fatalf("citation 1 = %+v", c1)
	}
	if len(res.Parts) != 3 || res.Parts[2].Kind() != agenttypes.KindImage {
		t.Fatalf("parts = %#v", res.Parts)
	}
	if src, _ := agenttypes.SourceOf(res.Parts[2]); string(src.Inline) != "png bytes" || src.MediaType != agenttypes.MediaPNG {
		t.Fatalf("image source = %+v", src)
	}

	// Execute is the JSON text alone, as before.
	out, err := findTool(t, tools, "rag_search").Execute(context.Background(), map[string]any{"query": "refunds"})
	if err != nil {
		t.Fatal(err)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) != 2 {
		t.Fatalf("Execute = %s, %v", out, err)
	}
	if strings.Contains(out, "cG5n") {
		t.Fatal("Execute output carries media bytes")
	}
}

func TestMediaLimits(t *testing.T) {
	hits := []types.SearchHit{chartHit(), chartHit(), chartHit()}
	mp := &mockPipeline{searchResult: &types.SearchPipelineResult{Hits: hits}, lookupResult: ptr(chartHit())}
	count := func(opts ...tool.Option) (search, lookup int) {
		tools := tool.NewTools(mp, append(opts, tool.ReadOnly())...)
		for name, n := range map[string]*int{"rag_search": &search, "rag_lookup": &lookup} {
			res, err := richTool(t, tools, name).ExecuteRich(context.Background(), map[string]any{"query": "q", "variant_uuid": "v-img"})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range res.Parts {
				if agenttypes.IsMedia(p) {
					*n++
				}
			}
		}
		return search, lookup
	}
	if s, l := count(); s != 3 || l != 1 {
		t.Fatalf("default: %d, %d", s, l)
	}
	if s, _ := count(tool.WithMediaLimits(0, 2)); s != 2 {
		t.Fatalf("two parts: %d", s)
	}
	if s, _ := count(tool.WithMediaLimits(len("png bytes")+1, 0)); s != 1 {
		t.Fatalf("byte cap: %d", s)
	}
	if s, l := count(tool.WithMediaLimits(-1, 0)); s != 0 || l != 0 {
		t.Fatalf("no media: %d, %d", s, l)
	}
}

func TestLookupToolCitesTheVariant(t *testing.T) {
	long := strings.Repeat("policy text ", 200)
	hit := types.SearchHit{
		Variant:    types.ContentVariant{UUID: "v1", ContentType: types.ContentText, Text: long},
		Provenance: types.Provenance{DocumentUUID: "d1", DocumentTitle: "Handbook", SourceURI: "file://handbook.md", SectionUUID: "s1"},
	}
	tools := tool.NewTools(&mockPipeline{lookupResult: &hit}, tool.ReadOnly())
	res, err := richTool(t, tools, "rag_lookup").ExecuteRich(context.Background(), map[string]any{"variant_uuid": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 1 {
		t.Fatalf("citations = %+v", res.Citations)
	}
	c := res.Citations[0]
	if c.URI != "file://handbook.md" || c.Producer != "rag_lookup" || !strings.HasPrefix(long, c.Quote) ||
		len(c.Quote) == 0 || len(c.Quote) > 1024 || c.Meta["start"] != 0 || c.Meta["end"] != len(c.Quote) {
		t.Fatalf("citation = %+v", c)
	}
	if len(res.Parts) != 1 || res.HasMedia() {
		t.Fatalf("a text variant returned media: %#v", res.Parts)
	}
}

func ptr[T any](v T) *T { return &v }
