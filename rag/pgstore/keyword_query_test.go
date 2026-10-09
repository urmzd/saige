package pgstore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/types"
)

func TestBuildKeywordSQLFeatures(t *testing.T) {
	boosts := &types.FieldBoosts{Body: 1, Title: 3, Heading: 2}
	tests := []struct {
		name     string
		q        types.KeywordQuery
		contains []string
		args     []any // arguments after the scope, before filters and limit
	}{
		{
			name:     "all terms",
			q:        types.KeywordQuery{Text: "a b", Mode: types.KeywordAll},
			contains: []string{"v.id @@@ paradedb.match('text', $2, conjunction_mode => true) AND"},
			args:     []any{"a b"},
		},
		{
			name: "phrase with slop",
			q:    types.KeywordQuery{Text: "a b", Mode: types.KeywordPhrase, Slop: 2},
			contains: []string{
				"CASE WHEN cardinality($2::text::pdb.unicode_words::text[]) > 1",
				"paradedb.phrase('text', $2::text::pdb.unicode_words::text[], slop => $3::int)",
				"ELSE paradedb.match('text', $2) END",
			},
			args: []any{"a b", 2},
		},
		{
			name:     "prefix any",
			q:        types.KeywordQuery{Text: "pos", Prefix: true},
			contains: []string{"paradedb.boolean(should => ARRAY(SELECT paradedb.fuzzy_term('text', tok, 0, false, true) FROM unnest($2::text::pdb.unicode_words::text[]) AS tok))"},
			args:     []any{"pos"},
		},
		{
			name:     "prefix all",
			q:        types.KeywordQuery{Text: "pos sea", Prefix: true, Mode: types.KeywordAll},
			contains: []string{"paradedb.boolean(must => ARRAY(SELECT paradedb.fuzzy_term("},
			args:     []any{"pos sea"},
		},
		{
			name:     "fuzzy",
			q:        types.KeywordQuery{Text: "okapy", Fuzziness: 1, Transpositions: true},
			contains: []string{"paradedb.match('text', $2, distance => $3::int, transposition_cost_one => true, prefix => false, conjunction_mode => false)"},
			args:     []any{"okapy", 1},
		},
		{
			name:     "fuzzy prefix",
			q:        types.KeywordQuery{Text: "oka", Fuzziness: 2, Prefix: true, Mode: types.KeywordAll},
			contains: []string{"distance => $3::int, transposition_cost_one => false, prefix => true, conjunction_mode => true"},
			args:     []any{"oka", 2},
		},
		{
			name: "boolean clauses",
			q: types.KeywordQuery{Text: "okapi",
				Must:    []types.KeywordClause{{Text: "forest"}},
				Should:  []types.KeywordClause{{Text: "grazes"}},
				MustNot: []types.KeywordClause{{Text: "zebra"}, {Text: " "}}},
			contains: []string{"v.id @@@ paradedb.boolean(must => ARRAY[paradedb.match('text', $2), paradedb.match('text', $3)], " +
				"should => ARRAY[paradedb.match('text', $4)], must_not => ARRAY[paradedb.match('text', $5)]) AND"},
			args: []any{"okapi", "forest", "grazes", "zebra"},
		},
		{
			name: "field boosts bind once per query",
			q:    types.KeywordQuery{Text: "okapi", Must: []types.KeywordClause{{Text: "zoo"}}, Fields: boosts},
			contains: []string{
				"paradedb.boolean(should => ARRAY[paradedb.match('text', $2), " +
					"paradedb.boost($3::real, paradedb.match('document_title', $2)), " +
					"paradedb.boost($4::real, paradedb.match('section_heading', $2))])",
				"paradedb.boost($3::real, paradedb.match('document_title', $5))",
			},
			args: []any{"okapi", float32(3), float32(2), "zoo"},
		},
		{
			name:     "title only",
			q:        types.KeywordQuery{Text: "okapi", Fields: &types.FieldBoosts{Title: 1}},
			contains: []string{"v.id @@@ paradedb.match('document_title', $2) AND"},
			args:     []any{"okapi"},
		},
		{
			name: "highlight",
			q:    types.KeywordQuery{Text: "okapi", Highlight: &types.HighlightOptions{StartTag: "[", MaxChars: 40}},
			contains: []string{
				"pdb.snippet(v.text, start_tag => $3::text, end_tag => $4::text, max_num_chars => $5::int)",
				"pdb.snippet_positions(v.text)",
			},
			args: []any{"okapi", "[", "</b>", 40},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.q.Validate(); err != nil {
				t.Fatalf("invalid test query: %v", err)
			}
			sql, args := buildKeywordSQL(tt.q, &types.SearchOptions{Scope: "s"}, 5)
			for _, c := range tt.contains {
				if !strings.Contains(sql, c) {
					t.Errorf("SQL missing %q:\n%s", c, sql)
				}
			}
			want := append(append([]any{"s"}, tt.args...), 5)
			if !reflect.DeepEqual(args, want) {
				t.Errorf("args = %#v, want %#v", args, want)
			}
		})
	}
}

// hostileInputs are strings that would break out of a quoted literal, start
// a comment, reference a placeholder, or use pg_search query syntax if they
// reached the SQL text.
var hostileInputs = []string{
	`'); DROP TABLE rag_variant; --`,
	`okapi' OR '1'='1`,
	`$1 $2 $$ $tag$`,
	`text:okapi AND title:(zebra OR "x") NOT y~2 z*`,
	"/* comment */ okapi \\' \x00",
	`::pdb.fuzzy(2)::pdb.boost(9)`,
}

// keywordQueryWith fills every text-bearing field of a query shape with s.
func keywordQueryWith(s string) types.KeywordQuery {
	return types.KeywordQuery{
		Text:    s,
		Mode:    types.KeywordPhrase,
		Slop:    1,
		Must:    []types.KeywordClause{{Text: s, Prefix: true}},
		Should:  []types.KeywordClause{{Text: s, Fuzziness: 2}},
		MustNot: []types.KeywordClause{{Text: s, Mode: types.KeywordAll}},
		Fields:  &types.FieldBoosts{Body: 1, Title: 2, Heading: 0.5},
		Highlight: &types.HighlightOptions{
			StartTag: s, EndTag: s,
		},
	}
}

// TestBuildKeywordSQLInjectionSafety checks that user input never shapes
// the SQL: a query full of hostile strings compiles to exactly the SQL of
// the same query with harmless text, and every input travels as an
// argument, unchanged.
func TestBuildKeywordSQLInjectionSafety(t *testing.T) {
	opts := &types.SearchOptions{Scope: "tenant"}
	reference, _ := buildKeywordSQL(keywordQueryWith("harmless"), opts, 10)
	for _, input := range hostileInputs {
		q := keywordQueryWith(input)
		if err := q.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", input, err)
		}
		sql, args := buildKeywordSQL(q, opts, 10)
		if sql != reference {
			t.Errorf("input %q changed the SQL:\n%s", input, sql)
		}
		if strings.Contains(sql, input) {
			t.Errorf("input %q appears in the SQL", input)
		}
		found := 0
		for _, a := range args {
			if a == input || a == strings.ReplaceAll(input, "\x00", "") {
				found++
			}
		}
		// Four clause texts plus the two highlight tags.
		if found != 6 {
			t.Errorf("input %q bound %d times, want 6: %#v", input, found, args)
		}
	}
}

// FuzzBuildKeywordSQL checks the same property for arbitrary input.
func FuzzBuildKeywordSQL(f *testing.F) {
	for _, s := range hostileInputs {
		f.Add(s)
	}
	reference, _ := buildKeywordSQL(keywordQueryWith("x"), nil, 10)
	f.Fuzz(func(t *testing.T, s string) {
		if blank(s) {
			t.Skip("blank clauses are dropped")
		}
		if sql, _ := buildKeywordSQL(keywordQueryWith(s), nil, 10); sql != reference {
			t.Fatalf("input %q changed the SQL", s)
		}
	})
}

func TestNewHighlight(t *testing.T) {
	snippet := "the <b>okapi</b>"
	h := newHighlight(&snippet, [][]int32{{4, 9}, {9, 4}, {20, 30}, {1}}, 10)
	if h == nil || h.Snippet != snippet || !reflect.DeepEqual(h.Spans, []types.TextSpan{{Start: 4, End: 9}}) {
		t.Errorf("highlight = %+v, want the snippet and the one valid span", h)
	}
	empty := ""
	if h := newHighlight(&empty, nil, 10); h != nil {
		t.Errorf("highlight = %+v, want nil without snippet or spans", h)
	}
	if h := newHighlight(nil, nil, 10); h != nil {
		t.Errorf("highlight = %+v, want nil for NULL", h)
	}
}

// --- Integration tests against ParadeDB pg_search ---

// keywordCorpus stores one document per entry, each with one section and
// one text variant whose UUID is the document UUID plus "-v".
func keywordCorpus(t *testing.T, s *Store, scope string, docs ...[3]string) {
	t.Helper()
	ctx := context.Background()
	for i, d := range docs {
		uuid, title, text := d[0], d[1], d[2]
		doc := singleSectionDoc(uuid, "fp-"+uuid, nil,
			types.ContentVariant{UUID: uuid + "-v", ContentType: types.ContentText, Text: text})
		doc.Scope = scope
		doc.Title = title
		doc.Sections[0].Heading = []string{"Overview", "Grazing habits", "Installation"}[i%3]
		if err := s.CreateDocument(ctx, doc); err != nil {
			t.Fatalf("create %s: %v", uuid, err)
		}
	}
}

func keywordQueryUUIDs(t *testing.T, s *Store, scope string, q types.KeywordQuery) []string {
	t.Helper()
	hits, err := s.SearchByKeyword(context.Background(), "", &types.SearchOptions{Scope: scope, Keyword: &q})
	if err != nil {
		t.Fatalf("SearchByKeyword(%+v): %v", q, err)
	}
	out := []string{}
	for _, h := range hits {
		out = append(out, h.Variant.UUID)
	}
	return out
}

func TestKeywordQueryFeatures(t *testing.T) {
	s := NewStore(testPool(t), nil)
	keywordCorpus(t, s, "kq",
		[3]string{"run1", "Shoe catalog", "sleek running shoes for the track"},    // Overview
		[3]string{"run2", "Misc", "shoes running backwards down the hill"},        // Grazing habits
		[3]string{"run3", "Footwear", "running sleek shoes"},                      // Installation
		[3]string{"oka1", "Okapi field guide", "the animal grazes in the forest"}, // Overview
		[3]string{"oka2", "Zoo", "a zebra and an okapi share a pen"},              // Grazing habits
		[3]string{"pg1", "Databases", "postgres full text search with bm25"},      // Installation
	)
	// A document in another scope must never leak into results.
	keywordCorpus(t, s, "other", [3]string{"leak", "Okapi", "okapi running shoes postgres"})

	sorted := func(ids []string) []string { slices.Sort(ids); return ids }
	tests := []struct {
		name    string
		q       types.KeywordQuery
		want    []string
		ordered bool
	}{
		{name: "plain any term", q: types.KeywordQuery{Text: "zebra postgres"}, want: []string{"oka2-v", "pg1-v"}},
		{name: "all terms", q: types.KeywordQuery{Text: "sleek shoes", Mode: types.KeywordAll}, want: []string{"run1-v", "run3-v"}},
		{name: "phrase", q: types.KeywordQuery{Text: "running shoes", Mode: types.KeywordPhrase}, want: []string{"run1-v"}},
		{name: "phrase slop 1", q: types.KeywordQuery{Text: "running shoes", Mode: types.KeywordPhrase, Slop: 1}, want: []string{"run1-v", "run3-v"}},
		{name: "phrase slop 2 allows a swap", q: types.KeywordQuery{Text: "running shoes", Mode: types.KeywordPhrase, Slop: 2}, want: []string{"run1-v", "run2-v", "run3-v"}},
		{name: "one-term phrase", q: types.KeywordQuery{Text: "zebra", Mode: types.KeywordPhrase}, want: []string{"oka2-v"}},
		{name: "phrase is tokenized", q: types.KeywordQuery{Text: "Running, SHOES!", Mode: types.KeywordPhrase}, want: []string{"run1-v"}},
		{name: "prefix", q: types.KeywordQuery{Text: "postg zeb", Prefix: true}, want: []string{"oka2-v", "pg1-v"}},
		{name: "prefix all", q: types.KeywordQuery{Text: "post sea", Prefix: true, Mode: types.KeywordAll}, want: []string{"pg1-v"}},
		{name: "no prefix without the flag", q: types.KeywordQuery{Text: "postg"}, want: []string{}},
		{name: "fuzzy", q: types.KeywordQuery{Text: "zebre", Fuzziness: 1}, want: []string{"oka2-v"}},
		{name: "fuzzy distance bound", q: types.KeywordQuery{Text: "zbre", Fuzziness: 1}, want: []string{}},
		{name: "transposition costs two", q: types.KeywordQuery{Text: "zebar", Fuzziness: 1}, want: []string{}},
		{name: "transposition costs one", q: types.KeywordQuery{Text: "zebar", Fuzziness: 1, Transpositions: true}, want: []string{"oka2-v"}},
		{name: "fuzzy prefix", q: types.KeywordQuery{Text: "pstg", Fuzziness: 1, Prefix: true}, want: []string{"pg1-v"}},
		{name: "must", q: types.KeywordQuery{Text: "shoes", Must: []types.KeywordClause{{Text: "hill"}}}, want: []string{"run2-v"}},
		{name: "must not", q: types.KeywordQuery{Text: "shoes", MustNot: []types.KeywordClause{{Text: "sleek"}}}, want: []string{"run2-v"}},
		{name: "must not phrase", q: types.KeywordQuery{Text: "shoes", MustNot: []types.KeywordClause{{Text: "sleek shoes", Mode: types.KeywordPhrase}}}, want: []string{"run1-v", "run2-v"}},
		{name: "should alone", q: types.KeywordQuery{Text: " ", Should: []types.KeywordClause{{Text: "zebra"}, {Text: "bm25"}}}, want: []string{"oka2-v", "pg1-v"}},
		{name: "should raises the score", q: types.KeywordQuery{Text: "shoes", Should: []types.KeywordClause{{Text: "hill"}}}, want: []string{"run2-v", "run3-v", "run1-v"}, ordered: true},
		{name: "body only misses a title match", q: types.KeywordQuery{Text: "guide"}, want: []string{}},
		{name: "title field", q: types.KeywordQuery{Text: "guide", Fields: &types.FieldBoosts{Body: 1, Title: 1}}, want: []string{"oka1-v"}},
		{name: "heading field", q: types.KeywordQuery{Text: "installation", Fields: &types.FieldBoosts{Heading: 1}}, want: []string{"pg1-v", "run3-v"}},
		{name: "title boost ranks the title match first", q: types.KeywordQuery{Text: "okapi", Fields: &types.FieldBoosts{Body: 1, Title: 10}}, want: []string{"oka1-v", "oka2-v"}, ordered: true},
		{name: "body boost ranks the body match first", q: types.KeywordQuery{Text: "okapi", Fields: &types.FieldBoosts{Body: 10, Title: 1}}, want: []string{"oka2-v", "oka1-v"}, ordered: true},
		{name: "query syntax is plain text", q: types.KeywordQuery{Text: `zebra:(AND "OR`, Fields: &types.FieldBoosts{Body: 1, Title: 1, Heading: 1}}, want: []string{"oka2-v"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keywordQueryUUIDs(t, s, "kq", tt.q)
			want := tt.want
			if !tt.ordered {
				got, want = sorted(got), sorted(slices.Clone(want))
			}
			if !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestKeywordQueryHighlight(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testPool(t), nil)
	keywordCorpus(t, s, "hl",
		[3]string{"h1", "Okapi notes", "Field notes: the shy okapi grazes at dusk, and the okapi hides by day."},
		[3]string{"h2", "Okapi only in the title", "a striped forest animal"},
	)
	q := types.KeywordQuery{
		Fields:    &types.FieldBoosts{Body: 1, Title: 1},
		Highlight: &types.HighlightOptions{StartTag: "[[", EndTag: "]]"},
	}
	hits, err := s.SearchByKeyword(ctx, "okapi", &types.SearchOptions{Scope: "hl", Keyword: &q})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]types.SearchHit{}
	for _, h := range hits {
		byID[h.Variant.UUID] = h
	}
	body, ok := byID["h1-v"]
	if !ok || body.Highlight == nil {
		t.Fatalf("body match has no highlight: %+v", hits)
	}
	if !strings.Contains(body.Highlight.Snippet, "[[okapi]]") {
		t.Errorf("snippet = %q, want the custom tags around okapi", body.Highlight.Snippet)
	}
	if len(body.Highlight.Spans) != 2 {
		t.Errorf("spans = %+v, want both okapi matches", body.Highlight.Spans)
	}
	for _, sp := range body.Highlight.Spans {
		if got := body.Variant.Text[sp.Start:sp.End]; got != "okapi" {
			t.Errorf("span %+v covers %q, want okapi", sp, got)
		}
	}
	if title, ok := byID["h2-v"]; !ok || title.Highlight != nil {
		t.Errorf("title-only match = %+v, want a hit without highlight", title)
	}

	plain, err := s.SearchByKeyword(ctx, "okapi", &types.SearchOptions{Scope: "hl"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || plain[0].Highlight != nil {
		t.Errorf("search without highlight = %+v, want one hit and no highlight", plain)
	}
}

// TestKeywordQueryHostileInput runs hostile strings through every clause
// against the real server: each search must succeed or fail cleanly, and
// the tables must survive.
func TestKeywordQueryHostileInput(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	s := NewStore(pool, nil)
	keywordCorpus(t, s, "inj", [3]string{"i1", "Title", "an okapi in the forest"})
	for _, input := range hostileInputs {
		q := keywordQueryWith(input + " okapi")
		if _, err := s.SearchByKeyword(ctx, "", &types.SearchOptions{Scope: "inj", Keyword: &q}); err != nil {
			t.Errorf("search with %q: %v", input, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rag_variant`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rag_variant count = %d, %v; want 1", n, err)
	}
	if got := keywordQueryUUIDs(t, s, "inj", types.KeywordQuery{Text: `okapi' OR '1'='1`}); !slices.Equal(got, []string{"i1-v"}) {
		t.Errorf("quoted input = %v, want only the okapi match", got)
	}
}

func TestKeywordQueryInvalid(t *testing.T) {
	s := NewStore(testPool(t), nil)
	q := types.KeywordQuery{Mode: types.KeywordPhrase, Fuzziness: 1}
	_, err := s.SearchByKeyword(context.Background(), "a b", &types.SearchOptions{Keyword: &q})
	if !errors.Is(err, types.ErrInvalidKeywordQuery) {
		t.Errorf("err = %v, want ErrInvalidKeywordQuery", err)
	}
}

// TestKeywordFieldsFollowStandaloneInserts checks that variants added
// through CreateSection and CreateVariant also index their heading and
// title.
func TestKeywordFieldsFollowStandaloneInserts(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testPool(t), nil)
	doc := singleSectionDoc("solo", "fp-solo", nil)
	doc.Title = "Capybara handbook"
	doc.Sections = nil
	if err := s.CreateDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSection(ctx, &types.Section{UUID: "solo-sec", DocumentUUID: "solo", Heading: "Swimming"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateVariant(ctx, &types.ContentVariant{UUID: "solo-v", SectionUUID: "solo-sec", ContentType: types.ContentText, Text: "they paddle"}); err != nil {
		t.Fatal(err)
	}
	fields := &types.FieldBoosts{Title: 1, Heading: 1}
	if got := keywordQueryUUIDs(t, s, "", types.KeywordQuery{Text: "capybara swimming", Mode: types.KeywordAll, Fields: fields}); !slices.Equal(got, []string{}) {
		t.Errorf("all terms across fields = %v; each field must hold every term", got)
	}
	if got := keywordQueryUUIDs(t, s, "", types.KeywordQuery{Text: "capybara swimming", Fields: fields}); !slices.Equal(got, []string{"solo-v"}) {
		t.Errorf("heading and title = %v, want solo-v", got)
	}
	if err := s.CreateVariant(ctx, &types.ContentVariant{UUID: "orphan", SectionUUID: "missing", ContentType: types.ContentText}); !errors.Is(err, types.ErrDocumentNotFound) {
		t.Errorf("variant of a missing section: err = %v, want ErrDocumentNotFound", err)
	}
}
