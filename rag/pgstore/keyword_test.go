package pgstore

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/types"
)

func TestBuildKeywordSQL(t *testing.T) {
	opts := &types.SearchOptions{
		Scope:           "tenant",
		ContentTypes:    []types.ContentType{types.ContentText},
		MinScore:        0.5,
		MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "core"}},
	}
	query, args := buildKeywordSQL("okapi", opts, 4)

	if !strings.HasPrefix(query, keywordBaseSQL) {
		t.Error("query should start with the keyword base SQL")
	}
	for _, clause := range []string{
		"v.text @@@ pdb.match($1)",
		"d.scope = $2",
		" AND v.content_type = ANY($3)",
		" AND " + keywordScoreSQL + " >= $4",
		" AND " + mergedMetadataSQL + " ->> $5 = $6",
	} {
		if !strings.Contains(query, clause) {
			t.Errorf("query missing %q:\n%s", clause, query)
		}
	}
	if !strings.HasSuffix(query, " ORDER BY "+keywordScoreSQL+" DESC, v.uuid LIMIT $7") {
		t.Errorf("unexpected suffix: %q", query)
	}
	want := []any{"okapi", "tenant", []string{"text"}, 0.5, "team", "core", 4}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

// keywordUUIDs runs a keyword search and returns the hit variant UUIDs in
// rank order.
func keywordUUIDs(t *testing.T, s *Store, query string, opts *types.SearchOptions) []string {
	t.Helper()
	hits, err := s.SearchByKeyword(context.Background(), query, opts)
	if err != nil {
		t.Fatalf("SearchByKeyword(%q): %v", query, err)
	}
	out := []string{}
	for _, h := range hits {
		out = append(out, h.Variant.UUID)
	}
	return out
}

// TestKeywordSearchFilters checks that keyword search applies the same
// scope, content-type, metadata, and limit rules as vector search, and
// returns provenance and document timestamps.
func TestKeywordSearchFilters(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testPool(t), nil)

	add := func(uuid, scope string, meta map[string]string, variants ...types.ContentVariant) {
		t.Helper()
		doc := singleSectionDoc(uuid, "fp-"+uuid, meta, variants...)
		doc.Scope = scope
		if err := s.CreateDocument(ctx, doc); err != nil {
			t.Fatalf("create %s: %v", uuid, err)
		}
	}
	add("k1", "a", map[string]string{"team": "core"},
		types.ContentVariant{UUID: "k1-v", ContentType: types.ContentText, Text: "okapi okapi okapi grazing in the forest"},
		types.ContentVariant{UUID: "k1-img", ContentType: types.ContentImage, MIMEType: "image/png", Data: []byte{1}, Text: "okapi photo"})
	add("k2", "a", map[string]string{"team": "edge"},
		types.ContentVariant{UUID: "k2-v", ContentType: types.ContentText, Text: "an okapi and a zebra"})
	add("k3", "b", map[string]string{"team": "core"},
		types.ContentVariant{UUID: "k3-v", ContentType: types.ContentText, Text: "okapi in another tenant"})
	add("k4", "a", nil,
		types.ContentVariant{UUID: "k4-v", ContentType: types.ContentText, Text: "nothing relevant here"})

	text := []types.ContentType{types.ContentText}
	tests := []struct {
		name  string
		query string
		opts  *types.SearchOptions
		want  []string
	}{
		{"scope a, text only, ranked", "okapi", &types.SearchOptions{Scope: "a", ContentTypes: text}, []string{"k1-v", "k2-v"}},
		{"scope b", "okapi", &types.SearchOptions{Scope: "b"}, []string{"k3-v"}},
		{"default scope is its own tenant", "okapi", nil, []string{}},
		{"all content types", "okapi", &types.SearchOptions{Scope: "a"}, []string{"k1-img", "k1-v", "k2-v"}},
		{"metadata eq", "okapi", &types.SearchOptions{Scope: "a", ContentTypes: text,
			MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterEq, Value: "edge"}}}, []string{"k2-v"}},
		{"metadata filter before limit", "okapi", &types.SearchOptions{Scope: "a", ContentTypes: text, Limit: 1,
			MetadataFilters: []types.MetadataFilter{{Key: "team", Op: types.FilterNeq, Value: "core"}}}, []string{"k2-v"}},
		{"limit", "okapi", &types.SearchOptions{Scope: "a", ContentTypes: text, Limit: 1}, []string{"k1-v"}},
		{"any term matches", "zebra forest", &types.SearchOptions{Scope: "a"}, []string{"k1-v", "k2-v"}},
		{"query syntax is plain text", `okapi: "(AND OR`, &types.SearchOptions{Scope: "b"}, []string{"k3-v"}},
		{"blank query", "   ", &types.SearchOptions{Scope: "a"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keywordUUIDs(t, s, tt.query, tt.opts)
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("got %v, want no hits", got)
				}
				return
			}
			if tt.name == "all content types" || tt.name == "any term matches" {
				slices.Sort(got)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	hits, err := s.SearchByKeyword(ctx, "okapi", &types.SearchOptions{Scope: "b"})
	if err != nil {
		t.Fatal(err)
	}
	h := hits[0]
	if h.Score <= 0 || h.Timestamp.IsZero() || h.Provenance.DocumentUUID != "k3" ||
		h.Provenance.SectionUUID != "k3-sec-0" || h.Variant.Text != "okapi in another tenant" {
		t.Errorf("hit = %+v, want a scored, dated hit with provenance", h)
	}
	high, err := s.SearchByKeyword(ctx, "okapi", &types.SearchOptions{Scope: "b", MinScore: h.Score + 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(high) != 0 {
		t.Errorf("MinScore above the best score returned %d hits", len(high))
	}
}

// TestKeywordIndexFollowsDocumentLifecycle checks that the BM25 index stays
// in step with the rows: inserts are searchable at once, ReplaceDocument
// swaps old terms for new ones, and DeleteDocument removes them.
func TestKeywordIndexFollowsDocumentLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testPool(t), nil)
	opts := &types.SearchOptions{Scope: "life"}

	doc := singleSectionDoc("life", "fp-life-1", nil,
		types.ContentVariant{UUID: "life-v1", ContentType: types.ContentText, Text: "the capybara swims"})
	doc.Scope = "life"
	if err := s.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if got := keywordUUIDs(t, s, "capybara", opts); !slices.Equal(got, []string{"life-v1"}) {
		t.Fatalf("after insert: %v", got)
	}

	next := singleSectionDoc("life", "fp-life-2", nil,
		types.ContentVariant{UUID: "life-v2", ContentType: types.ContentText, Text: "the axolotl regrows limbs"})
	next.Scope = "life"
	if err := s.ReplaceDocument(ctx, "life", next); err != nil {
		t.Fatal(err)
	}
	if got := keywordUUIDs(t, s, "capybara", opts); len(got) != 0 {
		t.Errorf("after replace, old text still matches: %v", got)
	}
	if got := keywordUUIDs(t, s, "axolotl", opts); !slices.Equal(got, []string{"life-v2"}) {
		t.Errorf("after replace: %v", got)
	}

	if err := s.DeleteDocument(ctx, "life"); err != nil {
		t.Fatal(err)
	}
	if got := keywordUUIDs(t, s, "axolotl", opts); len(got) != 0 {
		t.Errorf("after delete: %v", got)
	}
}

func TestKeywordRetriever(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testPool(t), nil)
	doc := singleSectionDoc("ret", "fp-ret", nil,
		types.ContentVariant{UUID: "ret-v", ContentType: types.ContentText, Text: "a pangolin rolls up"})
	if err := s.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	r := NewKeywordRetriever(s)
	if r.Name() != "bm25" {
		t.Errorf("Name() = %q, want bm25", r.Name())
	}
	hits, err := r.Retrieve(ctx, "pangolin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Variant.UUID != "ret-v" {
		t.Errorf("hits = %+v, want ret-v", hits)
	}
}
