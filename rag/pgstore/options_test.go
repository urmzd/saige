package pgstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	agenttypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/rag/types"
)

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"0.8.0", true}, {"0.8.2", true}, {"0.9", true}, {"1.0.0", true},
		{"0.7.4", false}, {"0.5.1", false}, {"", false}, {"x.y", false}, {"0", false},
	}
	for _, tt := range tests {
		if got := versionAtLeast(tt.version, 0, 8); got != tt.want {
			t.Errorf("versionAtLeast(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestStoreOptionValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults", opts: nil},
		{name: "valid ef_search", opts: []Option{WithEFSearch(100)}},
		{name: "ef_search too small", opts: []Option{WithEFSearch(0)}, wantErr: true},
		{name: "ef_search too large", opts: []Option{WithEFSearch(1001)}, wantErr: true},
		{name: "unknown mode", opts: []Option{WithIterativeScan("fast")}, wantErr: true},
		{name: "valid max scan tuples", opts: []Option{WithMaxScanTuples(20000)}},
		{name: "invalid max scan tuples", opts: []Option{WithMaxScanTuples(0)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := optionsOnly(tt.opts...)
			_, _, err := s.searchSettings(context.Background(), false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSearchSettings(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		filtered bool
		want     []setting
		wantMode IterativeScan
	}{
		{name: "auto unfiltered sets nothing", want: nil, wantMode: IterativeScanOff},
		{name: "off filtered sets nothing", opts: []Option{WithIterativeScan(IterativeScanOff)}, filtered: true, want: nil, wantMode: IterativeScanOff},
		{
			name: "strict filtered", opts: []Option{WithIterativeScan(IterativeScanStrict), WithMaxScanTuples(5000)}, filtered: true,
			want:     []setting{{"hnsw.iterative_scan", "strict_order"}, {"hnsw.max_scan_tuples", "5000"}},
			wantMode: IterativeScanStrict,
		},
		{
			name: "relaxed with ef_search", opts: []Option{WithIterativeScan(IterativeScanRelaxed), WithEFSearch(80)}, filtered: true,
			want:     []setting{{"hnsw.ef_search", "80"}, {"hnsw.iterative_scan", "relaxed_order"}},
			wantMode: IterativeScanRelaxed,
		},
		{name: "ef_search alone unfiltered", opts: []Option{WithEFSearch(64)}, want: []setting{{"hnsw.ef_search", "64"}}, wantMode: IterativeScanOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := optionsOnly(tt.opts...)
			got, mode, err := s.searchSettings(context.Background(), tt.filtered)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) || mode != tt.wantMode {
				t.Errorf("got %v mode %q, want %v mode %q", got, mode, tt.want, tt.wantMode)
			}
		})
	}
}

func TestStoreOptionFirstErrorWins(t *testing.T) {
	s := optionsOnly(WithEFSearch(0), WithMaxScanTuples(0), WithIterativeScan("fast"))
	_, _, err := s.searchSettings(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "ef_search") {
		t.Fatalf("err = %v, want the ef_search error", err)
	}
}

// TestSearchOptionsIterativeTrigger checks which search options make the
// default mode enable iterative scans. A pure distance threshold must not:
// it would keep the scan running to max_scan_tuples whenever fewer than
// limit rows pass it.
func TestSearchOptionsIterativeTrigger(t *testing.T) {
	filter := []types.MetadataFilter{{Key: "lang", Op: types.FilterEq, Value: "go"}}
	tests := []struct {
		name string
		opts *types.SearchOptions
		want bool
	}{
		{name: "nil options", opts: nil, want: false},
		{name: "limit only", opts: &types.SearchOptions{Limit: 5}, want: false},
		{name: "min score only", opts: &types.SearchOptions{MinScore: 0.7}, want: false},
		{name: "content types", opts: &types.SearchOptions{ContentTypes: []types.ContentType{types.ContentText}}, want: true},
		{name: "metadata filter", opts: &types.SearchOptions{MetadataFilters: filter}, want: true},
		{name: "metadata filter with min score", opts: &types.SearchOptions{MinScore: 0.7, MetadataFilters: filter}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSearchFilters(tt.opts); got != tt.want {
				t.Fatalf("hasSearchFilters = %v, want %v", got, tt.want)
			}
			if tt.want {
				return
			}
			// Unfiltered searches never consult the pgvector version, so a
			// nil pool is safe here.
			s := optionsOnly()
			got, mode, err := s.searchSettings(context.Background(), hasSearchFilters(tt.opts))
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range got {
				if st.name == "hnsw.iterative_scan" {
					t.Errorf("unexpected setting %v", st)
				}
			}
			if mode != IterativeScanOff {
				t.Errorf("mode = %q, want off", mode)
			}
		})
	}
}

// optionsOnly applies opts to a store without a database, for the option
// checks searchSettings makes.
func optionsOnly(opts ...Option) *Store {
	s := &Store{}
	for _, o := range opts {
		o(s)
	}
	return s
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, agenttypes.ErrInvalidConfig) {
		t.Fatalf("nil pool: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), "postgres://localhost:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := New(Config{Pool: pool}, WithEFSearch(0)); !errors.Is(err, agenttypes.ErrInvalidConfig) || !strings.Contains(err.Error(), "ef_search") {
		t.Fatalf("invalid option: %v", err)
	}
}
