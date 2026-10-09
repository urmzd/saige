package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urmzd/saige/rag/types"
)

// HTTP fetches documents from a list of URLs.
type HTTP struct {
	URLs []string
	// Client is optional. When nil, a client with DefaultHTTPTimeout is used.
	Client *http.Client
	// MaxBytes limits each response body. Zero means DefaultMaxBytes and a
	// negative value disables the limit. A larger body fails that URL with
	// an error wrapping ErrTooLarge.
	MaxBytes int64
	// ContinueOnError keeps fetching the remaining URLs after one fails. Fetch
	// then returns the documents that succeeded together with the joined
	// per-URL errors.
	ContinueOnError bool
}

var defaultHTTPClient = &http.Client{Timeout: DefaultHTTPTimeout}

// Fetch downloads each URL and returns the content as RawDocuments. By
// default the first failure aborts the fetch; see ContinueOnError.
func (s *HTTP) Fetch(ctx context.Context) ([]types.RawDocument, error) {
	client := s.Client
	if client == nil {
		client = defaultHTTPClient
	}
	limit := effectiveLimit(s.MaxBytes)

	docs := make([]types.RawDocument, 0, len(s.URLs))
	var errs []error
	for _, url := range s.URLs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		doc, err := fetchURL(ctx, client, url, limit)
		if err != nil {
			if !s.ContinueOnError || ctx.Err() != nil {
				return nil, err
			}
			errs = append(errs, err)
			continue
		}
		docs = append(docs, doc)
	}
	return docs, errors.Join(errs...)
}

func fetchURL(ctx context.Context, client *http.Client, url string, limit int64) (types.RawDocument, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return types.RawDocument{}, fmt.Errorf("create request for %s: %w", url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return types.RawDocument{}, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check the status before reading so an error page is never buffered.
	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return types.RawDocument{}, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	if limit >= 0 && resp.ContentLength > limit {
		return types.RawDocument{}, fmt.Errorf("fetch %s: %w: Content-Length %d exceeds %d bytes",
			url, ErrTooLarge, resp.ContentLength, limit)
	}

	data, err := readLimited(resp.Body, limit)
	if err != nil {
		return types.RawDocument{}, fmt.Errorf("read body from %s: %w", url, err)
	}

	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	// Strip charset parameters for cleaner MIME type.
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}

	// Last-Modified, when present and valid, is the source's modification
	// time; a missing or malformed header leaves it unknown.
	var modified time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			modified = t
		}
	}

	return types.RawDocument{
		SourceURI:        url,
		MIMEType:         mimeType,
		Data:             data,
		SourceModifiedAt: modified,
	}, nil
}
