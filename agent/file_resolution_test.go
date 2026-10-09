package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestResolveFilesReportsFailures(t *testing.T) {
	failing := types.ResolverFunc(func(context.Context, string) (types.ResolvedFile, error) {
		return types.ResolvedFile{}, errors.New("permission denied")
	})
	docx := types.ResolverFunc(func(context.Context, string) (types.ResolvedFile, error) {
		return types.ResolvedFile{Data: []byte("PK\x03\x04binary"), MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}, nil
	})
	brokenExtractor := types.ExtractorFunc(func(context.Context, []byte, types.MediaType) ([]types.UserContent, error) {
		return nil, errors.New("corrupt archive")
	})
	tests := []struct {
		name       string
		resolvers  map[string]types.Resolver
		extractors map[types.MediaType]types.Extractor
		uri        string
		wantNotice string // empty means the FileContent passes through
	}{
		{name: "resolver error", resolvers: map[string]types.Resolver{"s3": failing}, uri: "s3://bucket/a.pdf", wantNotice: "permission denied"},
		{name: "no resolver for scheme", resolvers: map[string]types.Resolver{"s3": failing}, uri: "file:///tmp/a.pdf", wantNotice: `no resolver for scheme "file"`},
		{name: "no resolvers configured", uri: "file:///tmp/a.pdf", wantNotice: `no resolver for scheme "file"`},
		{name: "web URL left for the provider", resolvers: map[string]types.Resolver{"s3": failing}, uri: "https://example.com/a.png"},
		{
			name:       "unsupported type without an extractor",
			resolvers:  map[string]types.Resolver{"file": docx},
			uri:        "file:///tmp/a.docx",
			wantNotice: "no extractor is registered",
		},
		{
			name:       "extractor error",
			resolvers:  map[string]types.Resolver{"file": docx},
			extractors: map[types.MediaType]types.Extractor{"application/vnd.openxmlformats-officedocument.wordprocessingml.document": brokenExtractor},
			uri:        "file:///tmp/a.docx",
			wantNotice: "corrupt archive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			a := NewAgent(AgentConfig{
				Provider:   &capturingProvider{response: "ok"},
				Resolvers:  tt.resolvers,
				Extractors: tt.extractors,
				Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
			})
			in := []types.Message{types.UserMessage{Content: []types.UserContent{types.FileContent{URI: tt.uri}}}}
			out := a.resolveFiles(context.Background(), in)
			content := out[0].(types.UserMessage).Content
			if len(content) != 1 {
				t.Fatalf("content = %#v, want one block", content)
			}
			if tt.wantNotice == "" {
				if _, ok := content[0].(types.FileContent); !ok {
					t.Fatalf("block = %#v, want the FileContent kept", content[0])
				}
				return
			}
			text, ok := content[0].(types.TextContent)
			if !ok {
				t.Fatalf("block = %#v, want a text notice instead of the file", content[0])
			}
			if !strings.Contains(text.Text, tt.uri) || !strings.Contains(text.Text, tt.wantNotice) {
				t.Errorf("notice = %q, want it to name %s and %q", text.Text, tt.uri, tt.wantNotice)
			}
			if !strings.Contains(logs.String(), "file could not be loaded") {
				t.Errorf("no warning logged: %s", logs.String())
			}
		})
	}
}
