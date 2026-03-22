package extractor_test

import (
	"context"
	"testing"

	"github.com/urmzd/graph-agent-dev-kit/rag/extractor"
	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

func TestPlainTextExtractor(t *testing.T) {
	ext := &extractor.PlainText{}
	raw := &ragtypes.RawDocument{
		SourceURI: "test://doc.txt",
		MIMEType:  "text/plain",
		Data:      []byte("First paragraph.\n\nSecond paragraph.\n\nThird paragraph."),
	}

	doc, err := ext.Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	if doc.UUID == "" {
		t.Error("expected non-empty UUID")
	}
	if doc.SourceURI != "test://doc.txt" {
		t.Errorf("expected source URI test://doc.txt, got %q", doc.SourceURI)
	}
	if len(doc.Sections) != 3 {
		t.Fatalf("expected 3 sections, got %d", len(doc.Sections))
	}
	if doc.Sections[0].Variants[0].Text != "First paragraph." {
		t.Errorf("unexpected first section text: %q", doc.Sections[0].Variants[0].Text)
	}
	if doc.Sections[0].Variants[0].ContentType != ragtypes.ContentText {
		t.Errorf("expected ContentText, got %q", doc.Sections[0].Variants[0].ContentType)
	}
}

func TestPlainTextSingleParagraph(t *testing.T) {
	ext := &extractor.PlainText{}
	raw := &ragtypes.RawDocument{
		Data: []byte("Just one line of text."),
	}

	doc, err := ext.Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	if len(doc.Sections) != 1 {
		t.Fatalf("expected 1 section, got %d", len(doc.Sections))
	}
}

func TestHTMLExtractor(t *testing.T) {
	ext := &extractor.HTML{}
	raw := &ragtypes.RawDocument{
		SourceURI: "test://page.html",
		MIMEType:  "text/html",
		Data: []byte(`<!DOCTYPE html>
<html>
<head><title>Test Page</title></head>
<body>
<h1>Introduction</h1>
<p>This is the introduction.</p>
<h2>Details</h2>
<p>Here are the details.</p>
</body>
</html>`),
	}

	doc, err := ext.Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	if doc.Title != "Test Page" {
		t.Errorf("expected title 'Test Page', got %q", doc.Title)
	}
	if len(doc.Sections) < 2 {
		t.Fatalf("expected at least 2 sections (one per heading), got %d", len(doc.Sections))
	}

	// First section should have heading "Introduction".
	foundIntro := false
	for _, sec := range doc.Sections {
		if sec.Heading == "Introduction" {
			foundIntro = true
		}
	}
	if !foundIntro {
		t.Error("expected a section with heading 'Introduction'")
	}
}

func TestHTMLSkipsScripts(t *testing.T) {
	ext := &extractor.HTML{}
	raw := &ragtypes.RawDocument{
		Data: []byte(`<html><body>
<p>Visible text.</p>
<script>var x = 1;</script>
<style>.hidden{}</style>
</body></html>`),
	}

	doc, err := ext.Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	for _, sec := range doc.Sections {
		for _, v := range sec.Variants {
			if contains(v.Text, "var x") || contains(v.Text, ".hidden") {
				t.Error("script/style content should be excluded")
			}
		}
	}
}

func TestAutoExtractor(t *testing.T) {
	auto := extractor.NewAuto()

	tests := []struct {
		name     string
		mime     string
		data     string
		wantErr  bool
	}{
		{"plain text", "text/plain", "hello world", false},
		{"html", "text/html", "<p>hello</p>", false},
		{"text with charset", "text/plain; charset=utf-8", "hello", false},
		{"markdown as text", "text/markdown", "# Hello", false},
		{"unsupported", "application/octet-stream", "binary", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := &ragtypes.RawDocument{
				MIMEType: tt.mime,
				Data:     []byte(tt.data),
			}
			_, err := auto.Extract(context.Background(), raw)
			if tt.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestPlainTextMetadata(t *testing.T) {
	ext := &extractor.PlainText{}
	meta := map[string]string{"author": "test"}
	raw := &ragtypes.RawDocument{
		Data:     []byte("content"),
		Metadata: meta,
	}

	doc, err := ext.Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}

	if doc.Metadata["author"] != "test" {
		t.Error("metadata should be preserved on document")
	}
	if doc.Sections[0].Variants[0].Metadata["author"] != "test" {
		t.Error("metadata should be propagated to variants")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
