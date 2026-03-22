package source_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/source"
)

func TestFilesystemFetch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "doc1.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "doc2.txt"), []byte("world"), 0o644)
	os.WriteFile(filepath.Join(dir, "image.png"), []byte("binary"), 0o644)

	s := &source.Filesystem{
		Dir:        dir,
		Extensions: []string{".txt"},
	}

	docs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(docs) != 2 {
		t.Fatalf("expected 2 docs (txt only), got %d", len(docs))
	}

	for _, doc := range docs {
		if doc.MIMEType != "text/plain" {
			t.Errorf("expected text/plain, got %q", doc.MIMEType)
		}
		if len(doc.Data) == 0 {
			t.Error("expected non-empty data")
		}
	}
}

func TestFilesystemRecursive(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	os.Mkdir(subdir, 0o755)
	os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(subdir, "nested.txt"), []byte("nested"), 0o644)

	// Non-recursive should only find root.
	s := &source.Filesystem{Dir: dir, Recursive: false}
	docs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("non-recursive: expected 1 doc, got %d", len(docs))
	}

	// Recursive should find both.
	s.Recursive = true
	docs, err = s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("recursive: expected 2 docs, got %d", len(docs))
	}
}

func TestFilesystemAllExtensions(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("txt"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.html"), []byte("html"), 0o644)

	s := &source.Filesystem{Dir: dir} // no extension filter
	docs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
}

func TestHTTPFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello from server"))
	}))
	defer server.Close()

	s := &source.HTTP{URLs: []string{server.URL + "/doc1"}}
	docs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
	if string(docs[0].Data) != "hello from server" {
		t.Errorf("unexpected data: %q", string(docs[0].Data))
	}
	if docs[0].MIMEType != "text/plain" {
		t.Errorf("expected text/plain, got %q", docs[0].MIMEType)
	}
	if docs[0].SourceURI != server.URL+"/doc1" {
		t.Errorf("expected source URI to be URL, got %q", docs[0].SourceURI)
	}
}

func TestHTTPFetchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	s := &source.HTTP{URLs: []string{server.URL + "/missing"}}
	_, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}
