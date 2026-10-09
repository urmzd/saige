package source_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urmzd/saige/rag/source"
)

func TestFilesystemSourceModifiedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2025, 5, 4, 3, 2, 1, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	docs, err := (&source.Filesystem{Dir: dir}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || !docs[0].SourceModifiedAt.Equal(mtime) {
		t.Fatalf("SourceModifiedAt = %v, want %v", docs[0].SourceModifiedAt, mtime)
	}
}

func TestHTTPSourceModifiedAt(t *testing.T) {
	stamp := time.Date(2025, 5, 4, 3, 2, 1, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Time
	}{
		{"valid Last-Modified", stamp.Format(http.TimeFormat), stamp},
		{"absent", "", time.Time{}},
		{"malformed", "yesterday", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Last-Modified", tt.header)
				}
				_, _ = w.Write([]byte("body"))
			}))
			defer srv.Close()
			docs, err := (&source.HTTP{URLs: []string{srv.URL}}).Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !docs[0].SourceModifiedAt.Equal(tt.want) {
				t.Errorf("SourceModifiedAt = %v, want %v", docs[0].SourceModifiedAt, tt.want)
			}
		})
	}
}
