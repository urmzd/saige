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

// resolveSources only fills locators: it never replaces a part, and a part
// it cannot resolve is marked unavailable for the conversion plan to reject.
func TestResolveSourcesFillsLocatorsAndNeverReplaces(t *testing.T) {
	failing := types.ResolverFunc(func(context.Context, string) (types.ResolvedFile, error) {
		return types.ResolvedFile{}, errors.New("permission denied")
	})
	pdf := types.ResolverFunc(func(context.Context, string) (types.ResolvedFile, error) {
		return types.ResolvedFile{Data: []byte("%PDF-1.4"), MediaType: types.MediaPDF}, nil
	})
	tests := []struct {
		name        string
		resolvers   map[string]types.Resolver
		part        types.UserPart
		unavailable string // substring of Source.Unresolved; empty means resolvable
		wantBytes   bool
		wantURI     string
	}{
		{name: "resolver error", resolvers: map[string]types.Resolver{"s3": failing},
			part: types.Media(types.URL("s3://bucket/a.pdf")), unavailable: "permission denied", wantURI: "s3://bucket/a.pdf"},
		{name: "no resolver for the scheme is left for the provider", resolvers: map[string]types.Resolver{"s3": failing},
			part: types.Media(types.URL("gs://bucket/a.png", types.MediaPNG)), wantURI: "gs://bucket/a.png"},
		{name: "web URL left for the provider", part: types.Media(types.URL("https://example.com/a.png")),
			wantURI: "https://example.com/a.png"},
		{name: "resolved bytes keep the URI", resolvers: map[string]types.Resolver{"file": pdf},
			part: types.Media(types.URL("file:///tmp/a.pdf")), wantBytes: true, wantURI: "file:///tmp/a.pdf"},
		{name: "inline bytes get a digest", part: types.Image(types.Source{MediaType: types.MediaPNG, Inline: []byte("png")}), wantBytes: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			a := NewAgent(AgentConfig{
				Provider:  &capturingProvider{response: "ok"},
				Resolvers: tt.resolvers,
				Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
			})
			out := a.resolveSources(context.Background(), []types.Message{types.UserMsg(tt.part)})
			parts := out[0].(types.UserMessage).Parts
			if len(parts) != 1 || !types.IsMedia(parts[0]) {
				t.Fatalf("parts = %#v, want the media part kept", parts)
			}
			src, _ := types.SourceOf(parts[0])
			if src.URI != tt.wantURI {
				t.Errorf("URI = %q, want %q", src.URI, tt.wantURI)
			}
			if tt.unavailable != "" {
				if !strings.Contains(src.Unresolved, tt.unavailable) {
					t.Errorf("Unresolved = %q, want it to contain %q", src.Unresolved, tt.unavailable)
				}
				if !strings.Contains(logs.String(), "media could not be resolved") {
					t.Errorf("no warning logged: %s", logs.String())
				}
				return
			}
			if src.Unresolved != "" {
				t.Errorf("Unresolved = %q, want none", src.Unresolved)
			}
			if got := len(src.Inline) > 0; got != tt.wantBytes {
				t.Errorf("has bytes = %v, want %v", got, tt.wantBytes)
			}
			if tt.wantBytes && src.Digest == "" {
				t.Error("resolved bytes have no digest")
			}
		})
	}
}
