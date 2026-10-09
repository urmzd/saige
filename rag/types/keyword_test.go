package types

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestKeywordQueryValidate(t *testing.T) {
	boosts := func(b FieldBoosts) *FieldBoosts { return &b }
	tests := []struct {
		name  string
		q     KeywordQuery
		valid bool
	}{
		{"plain", KeywordQuery{Text: "okapi"}, true},
		{"all fuzzy prefix", KeywordQuery{Text: "oka", Mode: KeywordAll, Prefix: true, Fuzziness: 1, Transpositions: true}, true},
		{"phrase with slop", KeywordQuery{Text: "running shoes", Mode: KeywordPhrase, Slop: 2}, true},
		{"title only", KeywordQuery{Text: "x", Fields: boosts(FieldBoosts{Title: 2})}, true},
		{"unknown mode", KeywordQuery{Text: "x", Mode: "regex"}, false},
		{"fuzziness too high", KeywordQuery{Text: "x", Fuzziness: 3}, false},
		{"negative fuzziness", KeywordQuery{Text: "x", Fuzziness: -1}, false},
		{"transpositions without fuzziness", KeywordQuery{Text: "x", Transpositions: true}, false},
		{"fuzzy phrase", KeywordQuery{Text: "x y", Mode: KeywordPhrase, Fuzziness: 1}, false},
		{"prefix phrase", KeywordQuery{Text: "x y", Mode: KeywordPhrase, Prefix: true}, false},
		{"negative slop", KeywordQuery{Text: "x y", Mode: KeywordPhrase, Slop: -1}, false},
		{"slop without phrase", KeywordQuery{Text: "x", Slop: 1}, false},
		{"bad clause", KeywordQuery{Text: "x", MustNot: []KeywordClause{{Text: "y", Fuzziness: 9}}}, false},
		{"negative boost", KeywordQuery{Text: "x", Fields: boosts(FieldBoosts{Body: -1})}, false},
		{"NaN boost", KeywordQuery{Text: "x", Fields: boosts(FieldBoosts{Body: math.NaN()})}, false},
		{"boost too high", KeywordQuery{Text: "x", Fields: boosts(FieldBoosts{Body: 1, Title: 4096})}, false},
		{"all boosts zero", KeywordQuery{Text: "x", Fields: boosts(FieldBoosts{})}, false},
		{"negative highlight size", KeywordQuery{Text: "x", Highlight: &HighlightOptions{MaxChars: -1}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.q.Validate()
			if tt.valid && err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidKeywordQuery) {
				t.Fatalf("Validate = %v, want ErrInvalidKeywordQuery", err)
			}
		})
	}
}

func TestKeywordQueryFor(t *testing.T) {
	if got := KeywordQueryFor("okapi", nil); !reflect.DeepEqual(got, KeywordQuery{Text: "okapi"}) {
		t.Errorf("nil opts: %+v", got)
	}
	opts := &SearchOptions{Keyword: &KeywordQuery{Mode: KeywordAll}}
	if got := KeywordQueryFor("okapi zebra", opts); got.Text != "okapi zebra" || got.Mode != KeywordAll {
		t.Errorf("empty text takes the search query: %+v", got)
	}
	if opts.Keyword.Text != "" {
		t.Error("KeywordQueryFor modified the options")
	}
	opts.Keyword.Text = "own"
	if got := KeywordQueryFor("ignored", opts); got.Text != "own" {
		t.Errorf("own text kept: %+v", got)
	}
}

func TestKeywordQueryIsEmpty(t *testing.T) {
	for _, tt := range []struct {
		q    KeywordQuery
		want bool
	}{
		{KeywordQuery{}, true},
		{KeywordQuery{Text: "  "}, true},
		{KeywordQuery{MustNot: []KeywordClause{{Text: "x"}}}, true},
		{KeywordQuery{Text: "x"}, false},
		{KeywordQuery{Must: []KeywordClause{{Text: "x"}}}, false},
		{KeywordQuery{Should: []KeywordClause{{Text: "x"}}}, false},
	} {
		if got := tt.q.IsEmpty(); got != tt.want {
			t.Errorf("%+v IsEmpty = %v, want %v", tt.q, got, tt.want)
		}
	}
}

func TestHighlightShifted(t *testing.T) {
	var none *Highlight
	if none.Shifted(3) != nil {
		t.Error("nil highlight shifted to non-nil")
	}
	h := &Highlight{Snippet: "s", Spans: []TextSpan{{1, 3}}}
	got := h.Shifted(10)
	if got.Snippet != "s" || !reflect.DeepEqual(got.Spans, []TextSpan{{11, 13}}) {
		t.Errorf("Shifted = %+v", got)
	}
	if h.Spans[0].Start != 1 {
		t.Error("Shifted modified the original")
	}
}

type unwrapStore struct{ Store }

func (u unwrapStore) Unwrap() Store { return u.Store }

type keywordOnly struct{ Store }

func (keywordOnly) SearchByKeyword(_ context.Context, _ string, _ *SearchOptions) ([]SearchHit, error) {
	return nil, nil
}

func TestAsStore(t *testing.T) {
	inner := keywordOnly{}
	if _, ok := AsStore[KeywordSearcher](unwrapStore{unwrapStore{inner}}); !ok {
		t.Error("AsStore did not find the keyword searcher through two wrappers")
	}
	if _, ok := AsStore[KeywordSearcher](unwrapStore{}); ok {
		t.Error("AsStore found a keyword searcher in a chain without one")
	}
	if _, ok := AsStore[KeywordSearcher](nil); ok {
		t.Error("AsStore found a keyword searcher in a nil store")
	}
}
