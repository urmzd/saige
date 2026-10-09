// Package source provides types.Source implementations for fetching documents.
package source

import (
	"context"
	"errors"
	"fmt"
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
	// MaxBytes limits each file. Zero means DefaultMaxBytes and a negative
	// value disables the limit. A larger file fails with an error wrapping
	// ErrTooLarge.
	MaxBytes int64
	// ContinueOnError keeps walking after a file or directory fails. Fetch
	// then returns the documents that succeeded together with the joined
	// per-path errors.
	ContinueOnError bool
}

// Fetch walks the directory and returns a RawDocument for each matching file.
// It stops with ctx.Err() as soon as ctx is done.
func (s *Filesystem) Fetch(ctx context.Context) ([]types.RawDocument, error) {
	var docs []types.RawDocument
	var errs []error
	limit := effectiveLimit(s.MaxBytes)

	// fail records err and keeps walking under ContinueOnError, or aborts.
	fail := func(err error) error {
		if s.ContinueOnError {
			errs = append(errs, err)
			return nil
		}
		return err
	}

	walkFn := func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if info != nil && info.IsDir() && s.ContinueOnError {
				errs = append(errs, err)
				return filepath.SkipDir
			}
			return fail(err)
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
		if limit >= 0 && info.Size() > limit {
			return fail(fmt.Errorf("read %s: %w: %d bytes exceeds %d", path, ErrTooLarge, info.Size(), limit))
		}

		data, err := readFile(path, limit)
		if err != nil {
			return fail(fmt.Errorf("read %s: %w", path, err))
		}

		docs = append(docs, types.RawDocument{
			SourceURI:        path,
			MIMEType:         detectMIME(path),
			Data:             data,
			SourceModifiedAt: info.ModTime(),
		})
		return nil
	}

	if err := filepath.Walk(s.Dir, walkFn); err != nil {
		return nil, err
	}
	return docs, errors.Join(errs...)
}

// readFile reads path with the size limit enforced on the bytes actually
// read, which also covers files that grow after the size check.
func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from filepath.Walk of a trusted root
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readLimited(f, limit)
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
