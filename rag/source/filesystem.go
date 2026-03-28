// Package source provides types.Source implementations for fetching documents.
package source

import (
	"context"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// Filesystem fetches documents from a local directory.
type Filesystem struct {
	Dir        string
	Extensions []string // e.g., [".txt", ".html", ".pdf"]. Empty means all files.
	Recursive  bool
}

// Fetch walks the directory and returns a RawDocument for each matching file.
func (s *Filesystem) Fetch(_ context.Context) ([]types.RawDocument, error) {
	var docs []types.RawDocument

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if !s.Recursive && path != s.Dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !s.matchesExtension(path) {
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec // path comes from filepath.Walk of a trusted root
		if err != nil {
			return err
		}

		docs = append(docs, types.RawDocument{
			SourceURI: path,
			MIMEType:  detectMIME(path),
			Data:      data,
		})
		return nil
	}

	if err := filepath.Walk(s.Dir, walkFn); err != nil {
		return nil, err
	}
	return docs, nil
}

func (s *Filesystem) matchesExtension(path string) bool {
	if len(s.Extensions) == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range s.Extensions {
		if ext == strings.ToLower(allowed) {
			return true
		}
	}
	return false
}

func detectMIME(path string) string {
	ext := filepath.Ext(path)
	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		// Strip charset parameters.
		if idx := strings.Index(mimeType, ";"); idx >= 0 {
			mimeType = strings.TrimSpace(mimeType[:idx])
		}
		return mimeType
	}
	// Common fallbacks.
	switch strings.ToLower(ext) {
	case ".txt", ".text":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	case ".pdf":
		return "application/pdf"
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}
