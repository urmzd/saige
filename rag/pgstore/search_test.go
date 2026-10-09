package pgstore

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/types"
)

func TestBuildSearchSQLNoFilters(t *testing.T) {
	query, args := buildSearchSQL("emb", nil, 10)

	if !strings.HasPrefix(query, searchBaseSQL) {
		t.Error("query should start with the base SQL")
	}
	if !strings.HasSuffix(query, " ORDER BY v.embedding <=> $1 LIMIT $3") {
		t.Errorf("unexpected suffix: %q", query)
	}
	// No over-fetch: the limit arg is exactly the requested limit. With no
	// options the search is limited to the default scope.
	want := []any{"emb", "", 10}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildSearchSQLMetadataFilterPushdown(t *testing.T) {
	tests := []struct {
		name       string
		op         types.FilterOp
		wantClause string
	}{
		{
			name:       "eq requires key present and equal",
			op:         types.FilterEq,
			wantClause: fmt.Sprintf(" AND %s ->> $3 = $4", mergedMetadataSQL),
		},
		{
			name:       "neq passes absent keys via IS DISTINCT FROM",
			op:         types.FilterNeq,
			wantClause: fmt.Sprintf(" AND (%s ->> $3) IS DISTINCT FROM $4", mergedMetadataSQL),
		},
		{
			name:       "contains uses position to avoid LIKE escaping",
			op:         types.FilterContains,
			wantClause: fmt.Sprintf(" AND position($4 IN %s ->> $3) > 0", mergedMetadataSQL),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &types.SearchOptions{
				MetadataFilters: []types.MetadataFilter{{Key: "lang", Op: tt.op, Value: "go"}},
			}
			query, args := buildSearchSQL("emb", opts, 5)

			if !strings.Contains(query, tt.wantClause) {
				t.Errorf("query missing clause %q:\n%s", tt.wantClause, query)
			}
			want := []any{"emb", "", "lang", "go", 5}
			if !reflect.DeepEqual(args, want) {
				t.Errorf("args = %v, want %v", args, want)
			}
			if !strings.HasSuffix(query, " LIMIT $5") {
				t.Errorf("limit placeholder misnumbered: %q", query)
			}
		})
	}
}

func TestBuildSearchSQLUnknownOpIgnored(t *testing.T) {
	opts := &types.SearchOptions{
		MetadataFilters: []types.MetadataFilter{{Key: "k", Op: types.FilterOp("regex"), Value: "v"}},
	}
	query, args := buildSearchSQL("emb", opts, 7)

	if strings.Contains(query, "$3 =") {
		t.Errorf("unknown op should not emit a clause: %q", query)
	}
	want := []any{"emb", "", 7}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildSearchSQLCombined(t *testing.T) {
	opts := &types.SearchOptions{
		ContentTypes: []types.ContentType{types.ContentText, types.ContentImage},
		MinScore:     0.5,
		MetadataFilters: []types.MetadataFilter{
			{Key: "lang", Op: types.FilterEq, Value: "go"},
			{Key: "draft", Op: types.FilterNeq, Value: "true"},
		},
	}
	query, args := buildSearchSQL("emb", opts, 3)

	wantClauses := []string{
		" AND v.content_type = ANY($3)",
		" AND 1 - (v.embedding <=> $1) >= $4",
		fmt.Sprintf(" AND %s ->> $5 = $6", mergedMetadataSQL),
		fmt.Sprintf(" AND (%s ->> $7) IS DISTINCT FROM $8", mergedMetadataSQL),
		" ORDER BY v.embedding <=> $1 LIMIT $9",
	}
	for _, clause := range wantClauses {
		if !strings.Contains(query, clause) {
			t.Errorf("query missing %q:\n%s", clause, query)
		}
	}

	want := []any{"emb", "", []string{"text", "image"}, 0.5, "lang", "go", "draft", "true", 3}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildSearchSQLScopeAndTimeRange(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		opts        *types.SearchOptions
		wantClauses []string
		wantArgs    []any
	}{
		{
			name:        "named scope is bound as $2",
			opts:        &types.SearchOptions{Scope: "tenant-a"},
			wantClauses: []string{"d.scope = $2", " LIMIT $3"},
			wantArgs:    []any{"emb", "tenant-a", 4},
		},
		{
			name: "since and until bound the effective document time",
			opts: &types.SearchOptions{Scope: "tenant-a", Since: since, Until: until},
			wantClauses: []string{
				" AND " + docTimeSQL + " >= $3",
				" AND " + docTimeSQL + " < $4",
				" LIMIT $5",
			},
			wantArgs: []any{"emb", "tenant-a", since, until, 4},
		},
		{
			name:        "open lower bound",
			opts:        &types.SearchOptions{Until: until},
			wantClauses: []string{" AND " + docTimeSQL + " < $3", " LIMIT $4"},
			wantArgs:    []any{"emb", "", until, 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, args := buildSearchSQL("emb", tt.opts, 4)
			for _, clause := range tt.wantClauses {
				if !strings.Contains(query, clause) {
					t.Errorf("query missing %q:\n%s", clause, query)
				}
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}

func TestHasSearchFiltersScopeAndTime(t *testing.T) {
	tests := []struct {
		name string
		opts *types.SearchOptions
		want bool
	}{
		{"nil", nil, false},
		{"default scope only", &types.SearchOptions{}, false},
		{"named scope", &types.SearchOptions{Scope: "a"}, true},
		{"since", &types.SearchOptions{Since: time.Unix(1, 0)}, true},
		{"until", &types.SearchOptions{Until: time.Unix(1, 0)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSearchFilters(tt.opts); got != tt.want {
				t.Errorf("hasSearchFilters = %v, want %v", got, tt.want)
			}
		})
	}
}
