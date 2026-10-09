package extractor

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/types"
)

func FuzzPlainTextExtractor(f *testing.F) {
	f.Add([]byte("Hello world\n\nSecond paragraph"))
	f.Add([]byte(""))
	f.Add([]byte("Single line"))
	f.Add([]byte("\n\n\n\n"))
	f.Add([]byte("Line1\nLine2\nLine3"))
	f.Add([]byte("a\x00b\xffc"))
	f.Add([]byte(strings.Repeat("é", 120)))

	ext := &PlainText{}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := &types.RawDocument{
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
			return
		}
		assertCleanText(t, doc)
	})
}

func FuzzHTMLExtractor(f *testing.F) {
	f.Add([]byte("<html><body><p>Hello</p></body></html>"))
	f.Add([]byte(""))
	f.Add([]byte("<h1>Title</h1><p>Content</p>"))
	f.Add([]byte("not html at all"))
	f.Add([]byte("<script>alert('xss')</script><p>safe</p>"))
	f.Add([]byte("<title>t\x00</title><h1>h\xff</h1><p>a\x00b</p>"))

	ext := &HTML{}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := &types.RawDocument{
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
			return
		}
		assertCleanText(t, doc)
	})
}

// assertCleanText fails when any text the stores persist as TEXT or JSONB
// contains a NUL byte or invalid UTF-8.
func assertCleanText(t *testing.T, doc *types.Document) {
	t.Helper()
	check := func(field, s string) {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			t.Errorf("%s is not clean text: %q", field, s)
		}
	}
	check("title", doc.Title)
	check("source uri", doc.SourceURI)
	for k, v := range doc.Metadata {
		check("document metadata", k+v)
	}
	for _, sec := range doc.Sections {
		check("heading", sec.Heading)
		for _, v := range sec.Variants {
			check("variant text", v.Text)
			for mk, mv := range v.Metadata {
				check("variant metadata", mk+mv)
			}
		}
	}
}

func TestExtractorsProduceCleanText(t *testing.T) {
	tests := []struct {
		name string
		ext  types.ContentExtractor
		raw  *types.RawDocument
	}{
		{
			name: "plain text with nul and invalid utf8",
			ext:  &PlainText{},
			raw: &types.RawDocument{
				Data:     []byte("a\x00b\xffc\n\nsecond\x00"),
				Metadata: map[string]string{"k": "v\x00"},
			},
		},
		{
			name: "plain text long multibyte title",
			ext:  &PlainText{},
			raw:  &types.RawDocument{Data: []byte(strings.Repeat("é", 120))},
		},
		{
			name: "html with nul and invalid utf8",
			ext:  &HTML{},
			raw:  &types.RawDocument{Data: []byte("<title>t\x00</title><h1>h\xff</h1><p>a\x00b</p>")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := tt.ext.Extract(context.Background(), tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Sections) == 0 {
				t.Fatal("expected at least one section")
			}
			assertCleanText(t, doc)
			if tt.raw.Metadata != nil && tt.raw.Metadata["k"] != "v\x00" {
				t.Error("raw metadata must not be modified")
			}
		})
	}
}
