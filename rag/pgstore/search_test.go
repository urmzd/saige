package pgstore

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/types"
)

func TestBuildSearchSQLNoFilters(t *testing.T) {
	query, args := buildSearchSQL("emb", nil, 10)

	if !strings.HasPrefix(query, searchBaseSQL) {
		t.Error("query should start with the base SQL")
	}
	if !strings.HasSuffix(query, " ORDER BY v.embedding <=> $1 LIMIT $2") {
		t.Errorf("unexpected suffix: %q", query)
	}
	// No over-fetch: the limit arg is exactly the requested limit.
	want := []any{"emb", 10}
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
			wantClause: fmt.Sprintf(" AND %s ->> $2 = $3", mergedMetadataSQL),
		},
		{
			name:       "neq passes absent keys via IS DISTINCT FROM",
			op:         types.FilterNeq,
			wantClause: fmt.Sprintf(" AND (%s ->> $2) IS DISTINCT FROM $3", mergedMetadataSQL),
		},
		{
			name:       "contains uses position to avoid LIKE escaping",
			op:         types.FilterContains,
			wantClause: fmt.Sprintf(" AND position($3 IN %s ->> $2) > 0", mergedMetadataSQL),
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
			want := []any{"emb", "lang", "go", 5}
			if !reflect.DeepEqual(args, want) {
				t.Errorf("args = %v, want %v", args, want)
			}
			if !strings.HasSuffix(query, " LIMIT $4") {
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

	if strings.Contains(query, "metadata") && strings.Contains(query, "$2 =") {
		t.Errorf("unknown op should not emit a clause: %q", query)
	}
	want := []any{"emb", 7}
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
		" AND v.content_type = ANY($2)",
		" AND 1 - (v.embedding <=> $1) >= $3",
		fmt.Sprintf(" AND %s ->> $4 = $5", mergedMetadataSQL),
		fmt.Sprintf(" AND (%s ->> $6) IS DISTINCT FROM $7", mergedMetadataSQL),
		" ORDER BY v.embedding <=> $1 LIMIT $8",
	}
	for _, clause := range wantClauses {
		if !strings.Contains(query, clause) {
			t.Errorf("query missing %q:\n%s", clause, query)
		}
	}

	want := []any{"emb", []string{"text", "image"}, 0.5, "lang", "go", "draft", "true", 3}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}
