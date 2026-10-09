package bm25retriever_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/rag/bm25retriever"
	"github.com/urmzd/saige/rag/memstore"
	"github.com/urmzd/saige/rag/types"
)

// queryFixture indexes one single-variant document per text, with variant
// UUIDs v0, v1, ... in order.
func queryFixture(t *testing.T, texts ...string) *bm25retriever.Retriever {
	t.Helper()
	ctx := context.Background()
	store := memstore.New()
	r := bm25retriever.New(store, nil)
	for i, text := range texts {
		id := string(rune('0' + i))
		doc := makeDoc("d"+id, "", []types.Section{makeTextSection("d"+id, "s"+id, "v"+id, text)})
		if err := store.CreateDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := r.Index(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func retrieveIDs(t *testing.T, r *bm25retriever.Retriever, query string, q *types.KeywordQuery) []string {
	t.Helper()
	hits, err := r.Retrieve(context.Background(), query, &types.SearchOptions{Keyword: q})
	if err != nil {
		t.Fatalf("Retrieve(%q, %+v): %v", query, q, err)
	}
	ids := []string{}
	for _, h := range hits {
		ids = append(ids, h.Variant.UUID)
	}
	slices.Sort(ids)
	return ids
}

func TestKeywordQueryInMemory(t *testing.T) {
	r := queryFixture(t,
		"sleek running shoes",          // v0
		"shoes running backwards",      // v1
		"running sleek shoes",          // v2
		"the okapi grazes in a forest", // v3
		"postgres full text search",    // v4
	)
	tests := []struct {
		name  string
		query string
		q     *types.KeywordQuery
		want  []string
	}{
		{"plain any term", "running okapi", nil, []string{"v0", "v1", "v2", "v3"}},
		{"all terms", "running sleek", &types.KeywordQuery{Mode: types.KeywordAll}, []string{"v0", "v2"}},
		{"phrase", "running shoes", &types.KeywordQuery{Mode: types.KeywordPhrase}, []string{"v0"}},
		{"phrase with a word between", "running shoes", &types.KeywordQuery{Mode: types.KeywordPhrase, Slop: 1}, []string{"v0", "v2"}},
		{"phrase swapped needs slop 2", "running shoes", &types.KeywordQuery{Mode: types.KeywordPhrase, Slop: 2}, []string{"v0", "v1", "v2"}},
		{"one-term phrase", "okapi", &types.KeywordQuery{Mode: types.KeywordPhrase}, []string{"v3"}},
		{"phrase ignores punctuation", "Running, SHOES!", &types.KeywordQuery{Mode: types.KeywordPhrase}, []string{"v0"}},
		{"prefix", "postg oka", &types.KeywordQuery{Prefix: true}, []string{"v3", "v4"}},
		{"prefix all", "post sea", &types.KeywordQuery{Prefix: true, Mode: types.KeywordAll}, []string{"v4"}},
		{"no prefix without flag", "postg", nil, []string{}},
		{"fuzzy", "okapy", &types.KeywordQuery{Fuzziness: 1}, []string{"v3"}},
		{"fuzzy distance bound", "okpy", &types.KeywordQuery{Fuzziness: 1}, []string{}},
		{"transposition costs two by default", "oakpi", &types.KeywordQuery{Fuzziness: 1}, []string{}},
		{"transposition costs one", "oakpi", &types.KeywordQuery{Fuzziness: 1, Transpositions: true}, []string{"v3"}},
		{"fuzzy prefix", "pstg", &types.KeywordQuery{Fuzziness: 1, Prefix: true}, []string{"v4"}},
		{"must", "shoes", &types.KeywordQuery{Must: []types.KeywordClause{{Text: "backwards"}}}, []string{"v1"}},
		{"must not", "shoes", &types.KeywordQuery{MustNot: []types.KeywordClause{{Text: "sleek"}}}, []string{"v1"}},
		{"must not phrase", "shoes", &types.KeywordQuery{MustNot: []types.KeywordClause{{Text: "sleek shoes", Mode: types.KeywordPhrase}}}, []string{"v0", "v1"}},
		{"should alone", "", &types.KeywordQuery{Text: " ", Should: []types.KeywordClause{{Text: "okapi"}, {Text: "postgres"}}}, []string{"v3", "v4"}},
		{"should only scores", "okapi", &types.KeywordQuery{Should: []types.KeywordClause{{Text: "postgres"}}}, []string{"v3"}},
		{"only must not", "", &types.KeywordQuery{Text: " ", MustNot: []types.KeywordClause{{Text: "okapi"}}}, []string{}},
		{"body boost zero", "okapi", &types.KeywordQuery{Fields: &types.FieldBoosts{Title: 1}}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retrieveIDs(t, r, tt.query, tt.q); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKeywordQueryShouldRaisesScore(t *testing.T) {
	r := queryFixture(t, "okapi forest", "okapi zebra")
	ctx := context.Background()
	q := &types.KeywordQuery{Should: []types.KeywordClause{{Text: "zebra"}}}
	hits, err := r.Retrieve(ctx, "okapi", &types.SearchOptions{Keyword: q})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Variant.UUID != "v1" {
		t.Errorf("hits = %+v, want the should match first", hits)
	}
}

func TestKeywordQueryInvalid(t *testing.T) {
	r := queryFixture(t, "okapi")
	_, err := r.Retrieve(context.Background(), "okapi", &types.SearchOptions{Keyword: &types.KeywordQuery{Fuzziness: 5}})
	if !errors.Is(err, types.ErrInvalidKeywordQuery) {
		t.Errorf("err = %v, want ErrInvalidKeywordQuery", err)
	}
}
