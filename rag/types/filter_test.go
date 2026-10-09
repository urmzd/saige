package types

import (
	"context"
	"testing"
	"time"
)

func TestFingerprint(t *testing.T) {
	data := []byte("same bytes")
	tests := []struct {
		name      string
		a, b      string
		dataA     []byte
		dataB     []byte
		wantEqual bool
	}{
		{"default scope is the plain content hash", "", "", data, data, true},
		{"same scope same data", "acme", "acme", data, data, true},
		{"different scopes never collide", "acme", "globex", data, data, false},
		{"scope differs from default", "acme", "", data, data, false},
		{"scope boundary is unambiguous", "ab", "a", []byte("c"), []byte("bc"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fingerprint(tt.a, tt.dataA) == Fingerprint(tt.b, tt.dataB)
			if got != tt.wantEqual {
				t.Errorf("equal = %v, want %v", got, tt.wantEqual)
			}
		})
	}
	// The default scope keeps the historical sha256(data) fingerprint, so
	// documents ingested before scopes existed still deduplicate.
	if got := Fingerprint("", []byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("default-scope fingerprint = %s", got)
	}
}

func TestEffectiveTime(t *testing.T) {
	t1 := time.Unix(100, 0)
	t2 := time.Unix(200, 0)
	t3 := time.Unix(300, 0)
	tests := []struct {
		name string
		doc  Document
		want time.Time
	}{
		{"source time wins", Document{SourceModifiedAt: t1, UpdatedAt: t2, CreatedAt: t3}, t1},
		{"updated without source time", Document{UpdatedAt: t2, CreatedAt: t3}, t2},
		{"created only", Document{CreatedAt: t3}, t3},
		{"unknown", Document{}, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.doc.EffectiveTime(); !got.Equal(tt.want) {
				t.Errorf("EffectiveTime = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchOptionsAdmits(t *testing.T) {
	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	rec := func(scope string, ts time.Time) *VariantRecord {
		return &VariantRecord{
			Variant:          ContentVariant{ContentType: ContentText, Metadata: map[string]string{"lang": "go"}},
			DocumentMetadata: map[string]string{"team": "core", "lang": "rust"},
			Scope:            scope,
			Timestamp:        ts,
		}
	}
	tests := []struct {
		name string
		opts *SearchOptions
		rec  *VariantRecord
		want bool
	}{
		{"nil options admit the default scope", nil, rec("", jan), true},
		{"nil options reject a named scope", nil, rec("a", jan), false},
		{"scope must match exactly", &SearchOptions{Scope: "a"}, rec("b", jan), false},
		{"default search rejects scoped records", &SearchOptions{}, rec("a", jan), false},
		{"since inclusive", &SearchOptions{Since: jan}, rec("", jan), true},
		{"until exclusive", &SearchOptions{Until: jan}, rec("", jan), false},
		{"bound rejects unknown time", &SearchOptions{Since: jan.AddDate(-1, 0, 0)}, rec("", time.Time{}), false},
		{"no bound admits unknown time", &SearchOptions{}, rec("", time.Time{}), true},
		{"content type filter", &SearchOptions{ContentTypes: []ContentType{ContentImage}}, rec("", jan), false},
		{"variant metadata wins", &SearchOptions{MetadataFilters: []MetadataFilter{{Key: "lang", Op: FilterEq, Value: "go"}}}, rec("", jan), true},
		{"document metadata visible", &SearchOptions{MetadataFilters: []MetadataFilter{{Key: "team", Op: FilterContains, Value: "co"}}}, rec("", jan), true},
		{"neq on absent key passes", &SearchOptions{MetadataFilters: []MetadataFilter{{Key: "x", Op: FilterNeq, Value: "y"}}}, rec("", jan), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.Admits(tt.rec); got != tt.want {
				t.Errorf("Admits = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSearchConfigOptions(t *testing.T) {
	since, until := time.Unix(1, 0), time.Unix(2, 0)
	cfg := &SearchConfig{}
	for _, o := range []SearchOption{WithScope("s"), WithTimeRange(since, until), WithContentDedup(), WithNeighborWindow(2), WithNeighborWindow(-1)} {
		o(cfg)
	}
	if cfg.Scope != "s" || !cfg.Since.Equal(since) || !cfg.Until.Equal(until) || !cfg.DedupContent || cfg.NeighborWindow != 2 {
		t.Errorf("config = %+v", cfg)
	}
}

func TestNoopObserver(t *testing.T) {
	ctx := context.Background()
	var o Observer = NoopObserver{}
	got, span := o.StartSpan(ctx, SpanSearch, Attr(AttrScope, "s"))
	if got != ctx {
		t.Error("noop StartSpan must return the same context")
	}
	span.SetAttributes(Attr(AttrHits, 1))
	span.RecordError(nil)
	span.End()
	o.RecordEmbedding(ctx, EmbeddingRecord{})
	o.RecordRetrieval(ctx, RetrievalRecord{})
}
