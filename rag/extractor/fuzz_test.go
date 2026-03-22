package extractor

import (
	"context"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

func FuzzPlainTextExtractor(f *testing.F) {
	f.Add([]byte("Hello world\n\nSecond paragraph"))
	f.Add([]byte(""))
	f.Add([]byte("Single line"))
	f.Add([]byte("\n\n\n\n"))
	f.Add([]byte("Line1\nLine2\nLine3"))

	ext := &PlainText{}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := &ragtypes.RawDocument{
			Data:      data,
			SourceURI: "fuzz://test",
		}
		// Must not panic
		doc, err := ext.Extract(context.Background(), raw)
		if err != nil {
			return
		}
		if doc == nil {
			t.Error("non-error result should not be nil")
		}
	})
}

func FuzzHTMLExtractor(f *testing.F) {
	f.Add([]byte("<html><body><p>Hello</p></body></html>"))
	f.Add([]byte(""))
	f.Add([]byte("<h1>Title</h1><p>Content</p>"))
	f.Add([]byte("not html at all"))
	f.Add([]byte("<script>alert('xss')</script><p>safe</p>"))

	ext := &HTML{}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := &ragtypes.RawDocument{
			Data:      data,
			SourceURI: "fuzz://test",
		}
		// Must not panic
		doc, err := ext.Extract(context.Background(), raw)
		if err != nil {
			return
		}
		if doc == nil {
			t.Error("non-error result should not be nil")
		}
	})
}
