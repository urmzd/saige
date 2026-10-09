package fusion

import (
	"math"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/types"
)

func hit(id string) types.SearchHit {
	return types.SearchHit{Variant: types.ContentVariant{UUID: id, ContentType: types.ContentText, Text: id}}
}

func list(retriever string, ids ...string) types.RankedList {
	l := types.RankedList{Retriever: retriever}
	for _, id := range ids {
		l.Hits = append(l.Hits, hit(id))
	}
	return l
}

func scores(hits []types.SearchHit) map[string]float64 {
	out := map[string]float64{}
	for _, h := range hits {
		out[h.Variant.UUID] = h.Score
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestFusers(t *testing.T) {
	lists := []types.RankedList{list("vector", "a", "b"), list("bm25", "b", "c")}
	tests := []struct {
		name    string
		fuser   types.Fuser
		opts    types.FuseOptions
		want    map[string]float64
		ceiling float64
	}{
		{
			name: "rrf default k", fuser: RRF{},
			want:    map[string]float64{"a": 1.0 / 61, "b": 1.0/62 + 1.0/61, "c": 1.0 / 62},
			ceiling: 2.0 / 61,
		},
		{
			name: "per-search k wins over configured", fuser: RRF{K: 10}, opts: types.FuseOptions{K: 1},
			want:    map[string]float64{"a": 1.0 / 2, "b": 1.0/3 + 1.0/2, "c": 1.0 / 3},
			ceiling: 2.0 / 2,
		},
		{
			name: "weighted", fuser: Weighted{Weights: map[string]float64{"bm25": 2}},
			want:    map[string]float64{"a": 1.0 / 61, "b": 1.0/62 + 2.0/61, "c": 2.0 / 62},
			ceiling: 3.0 / 61,
		},
		{
			name: "zero weight drops a retriever", fuser: Weighted{Weights: map[string]float64{"bm25": 0}},
			want:    map[string]float64{"a": 1.0 / 61, "b": 1.0 / 62},
			ceiling: 1.0 / 61,
		},
		{
			name: "default weight for unnamed retrievers", fuser: Weighted{Default: 0.5},
			want:    map[string]float64{"a": 0.5 / 61, "b": 0.5/62 + 0.5/61, "c": 0.5 / 62},
			ceiling: 1.0 / 61,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scores(tt.fuser.Fuse(lists, tt.opts))
			if len(got) != len(tt.want) {
				t.Fatalf("scores = %v, want %v", got, tt.want)
			}
			for id, w := range tt.want {
				if !near(got[id], w) {
					t.Errorf("score[%s] = %v, want %v", id, got[id], w)
				}
			}
			c := tt.fuser.(types.FusionCeiling).MaxFusedScore(lists, tt.opts)
			if !near(c, tt.ceiling) {
				t.Errorf("ceiling = %v, want %v", c, tt.ceiling)
			}
		})
	}
}

func TestKeyAndMerge(t *testing.T) {
	expanded := func(id, section, text string) types.SearchHit {
		h := hit(id)
		h.Variant.Text = text
		h.Provenance.SectionUUID = section
		h.Provenance.ExpandedFromVariantUUID = id
		return h
	}
	tests := []struct {
		name      string
		a, b      types.SearchHit
		wantEqual bool
	}{
		{"distinct variants", hit("a"), hit("b"), false},
		{"same variant", hit("a"), hit("a"), true},
		{"expanded children of one section", expanded("a", "s", "x"), expanded("b", "s", "x"), true},
		{"expanded without section by text", expanded("a", "", " t "), expanded("b", "", "t"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Key(&tt.a) == Key(&tt.b); got != tt.wantEqual {
				t.Errorf("same key = %v, want %v", got, tt.wantEqual)
			}
		})
	}

	ts := time.Unix(10, 0)
	vec := hit("a")
	vec.Score = 0.2
	vec.Variant.Embedding = []float32{1}
	vec.Timestamp = ts
	lex := hit("a")
	lex.Score = 7
	m := MergeDuplicate(vec, lex)
	if m.Score != 7 || len(m.Variant.Embedding) != 1 || !m.Timestamp.Equal(ts) {
		t.Errorf("merge = %+v", m)
	}
}

func TestContentKey(t *testing.T) {
	text := func(s string) types.SearchHit {
		return types.SearchHit{Variant: types.ContentVariant{ContentType: types.ContentText, Text: s}}
	}
	img := func(b string) types.SearchHit {
		return types.SearchHit{Variant: types.ContentVariant{ContentType: types.ContentImage, Data: []byte(b)}}
	}
	tests := []struct {
		name      string
		a, b      types.SearchHit
		wantEqual bool
	}{
		{"trimmed text", text(" hello\n"), text("hello"), true},
		{"different text", text("hello"), text("world"), false},
		{"bytes for non-text", img("x"), img("x"), true},
		{"content type is part of the key", text("x"), types.SearchHit{Variant: types.ContentVariant{ContentType: types.ContentTable, Text: "x"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContentKey(&tt.a) == ContentKey(&tt.b); got != tt.wantEqual {
				t.Errorf("same key = %v, want %v", got, tt.wantEqual)
			}
		})
	}
}
