package selector

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"read_file", []string{"read", "file"}},
		{"readFile", []string{"read", "file"}},
		{"HTTPServer", []string{"httpserver"}},
		{"Search the WEB, now!", []string{"search", "the", "web", "now"}},
		{"v2 api", []string{"v2", "api"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := Tokenize(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("Tokenize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBM25Select(t *testing.T) {
	docs := []string{
		"read_file reads a file from disk",
		"write_file writes a file to disk",
		"web_search searches the web",
		"get_weather returns the weather forecast for a city",
		"delete_file removes a file",
	}
	sel := NewBM25(func(s string) string { return s })
	tests := []struct {
		name    string
		query   string
		k       int
		want    []string
		wantErr error
	}{
		{"best match first", "weather forecast", 3, []string{docs[3]}, nil},
		{"shorter document ranks higher at equal frequency", "file", 2, []string{docs[4], docs[0]}, nil},
		{"k zero returns all matches", "file", 0, []string{docs[4], docs[0], docs[1]}, nil},
		{"rarer term ranks higher", "web file", 1, []string{docs[2]}, nil},
		{"no match", "database", 5, nil, nil},
		{"empty query", "  !! ", 5, nil, ErrEmptyQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sel.Select(context.Background(), tt.query, docs, tt.k)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Select(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestBM25Deterministic(t *testing.T) {
	items := []string{"alpha tool", "alpha tool", "alpha tool"}
	type item struct{ id int }
	wrapped := []item{{0}, {1}, {2}}
	sel := NewBM25(func(i item) string { return items[i.id] })
	for range 20 {
		got, err := sel.Select(context.Background(), "alpha", wrapped, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, wrapped) {
			t.Fatalf("ties must keep input order, got %v", got)
		}
	}
}

func TestBM25CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewBM25(func(s string) string { return s }).Select(ctx, "x", []string{"x"}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSelectorFunc(t *testing.T) {
	var sel Selector[int] = SelectorFunc[int](func(_ context.Context, _ string, items []int, k int) ([]int, error) {
		return items[:k], nil
	})
	got, _ := sel.Select(context.Background(), "q", []int{3, 2, 1}, 2)
	if !slices.Equal(got, []int{3, 2}) {
		t.Fatalf("got %v", got)
	}
}
