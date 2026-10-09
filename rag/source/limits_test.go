package source_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/rag/source"
)

// streamHandler writes n bytes in 64 KiB pieces without a Content-Length.
func streamHandler(n int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for written := 0; written < n; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func TestHTTPFetchLimits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/huge", streamHandler(100<<20))
	mux.HandleFunc("/declared", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5000000")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/small", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	tests := []struct {
		name            string
		paths           []string
		continueOnError bool
		wantDocs        int
		wantErr         error
		wantAnyErr      bool
	}{
		{name: "streamed body over limit", paths: []string{"/huge"}, wantErr: source.ErrTooLarge},
		{name: "declared length over limit", paths: []string{"/declared"}, wantErr: source.ErrTooLarge},
		{name: "first failure aborts", paths: []string{"/huge", "/small"}, wantErr: source.ErrTooLarge},
		{name: "continue on error keeps others", paths: []string{"/huge", "/small", "/missing", "/small"}, continueOnError: true, wantDocs: 2, wantErr: source.ErrTooLarge},
		{name: "within limit", paths: []string{"/small"}, wantDocs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urls := make([]string, len(tt.paths))
			for i, p := range tt.paths {
				urls[i] = server.URL + p
			}
			s := &source.HTTP{URLs: urls, MaxBytes: 1 << 20, ContinueOnError: tt.continueOnError}
			docs, err := s.Fetch(context.Background())
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(docs) != tt.wantDocs {
				t.Errorf("got %d docs, want %d", len(docs), tt.wantDocs)
			}
			for _, d := range docs {
				if string(d.Data) != "ok" || d.MIMEType != "text/plain" {
					t.Errorf("unexpected doc %q %q", d.Data, d.MIMEType)
				}
			}
		})
	}
}

func TestFilesystemFetchLimits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		maxBytes        int64
		continueOnError bool
		wantDocs        int
		wantTooLarge    bool
	}{
		{name: "over limit aborts", maxBytes: 1024, wantTooLarge: true},
		{name: "continue on error keeps small file", maxBytes: 1024, continueOnError: true, wantDocs: 1, wantTooLarge: true},
		{name: "default limit", maxBytes: 0, wantDocs: 2},
		{name: "unlimited", maxBytes: -1, wantDocs: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &source.Filesystem{Dir: dir, MaxBytes: tt.maxBytes, ContinueOnError: tt.continueOnError}
			docs, err := s.Fetch(context.Background())
			if tt.wantTooLarge != errors.Is(err, source.ErrTooLarge) {
				t.Fatalf("ErrTooLarge = %v, want %v (err %v)", errors.Is(err, source.ErrTooLarge), tt.wantTooLarge, err)
			}
			if len(docs) != tt.wantDocs {
				t.Errorf("got %d docs, want %d", len(docs), tt.wantDocs)
			}
		})
	}
}

func TestFilesystemFetchHonorsCancelledContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	docs, err := (&source.Filesystem{Dir: dir}).Fetch(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if docs != nil {
		t.Errorf("expected no docs, got %d", len(docs))
	}
}
