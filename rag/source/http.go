package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/urmzd/saige/rag/types"
)

// HTTP fetches documents from a list of URLs.
type HTTP struct {
	URLs   []string
	Client *http.Client // Optional; defaults to http.DefaultClient.
}

// Fetch downloads each URL and returns the content as RawDocuments.
func (s *HTTP) Fetch(ctx context.Context) ([]types.RawDocument, error) {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}

	docs := make([]types.RawDocument, 0, len(s.URLs))
	for _, url := range s.URLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("create request for %s: %w", url, err)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}

		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body from %s: %w", url, err)
		}

		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
		}

		mimeType := resp.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		// Strip charset parameters for cleaner MIME type.
		if idx := strings.Index(mimeType, ";"); idx >= 0 {
			mimeType = strings.TrimSpace(mimeType[:idx])
		}

		docs = append(docs, types.RawDocument{
			SourceURI: url,
			MIMEType:  mimeType,
			Data:      data,
		})
	}

	return docs, nil
}
