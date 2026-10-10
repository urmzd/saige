package types

import (
	"reflect"
	"testing"
)

func TestBytesSource(t *testing.T) {
	s := Bytes(MediaText, []byte("abc"))
	if s.Size != 3 || s.Digest != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("source = %+v", s)
	}
	if k, ok := s.Locator("anthropic", ""); !ok || k != SourceInline {
		t.Fatalf("locator = %v, %v", k, ok)
	}
}

func TestSourceWith(t *testing.T) {
	a := Bytes(MediaPNG, []byte{1})
	b := VendorFileID("anthropic", "ws", "file_1", MediaPNG)
	b.Digest = a.Digest
	b.URI = "https://x/a.png"
	got := a.With(b).With(VendorFileID("anthropic", "ws", "file_1", MediaPNG))
	if got.URI != "https://x/a.png" || len(got.Files) != 1 || len(got.Inline) != 1 {
		t.Fatalf("merged = %+v", got)
	}
	other := Bytes(MediaPNG, []byte{2})
	if !reflect.DeepEqual(a.With(other), a) {
		t.Fatal("sources with different digests were merged")
	}
	if a.Files != nil {
		t.Fatal("With changed its receiver")
	}
}

func TestSourceLocatorPreference(t *testing.T) {
	s := Source{MediaType: MediaPDF, Inline: []byte{1}, URI: "https://x/d.pdf", Ref: "saige-artifact://d",
		Files: []VendorFile{{Provider: "anthropic", Endpoint: "ws-a", ID: "file_a"}}}
	tests := []struct {
		provider, endpoint string
		want               SourceKind
	}{
		{"anthropic", "ws-a", SourceFile},
		{"anthropic", "", SourceFile},
		{"anthropic", "ws-b", SourceURI}, // another workspace's file is unusable
		{"openai", "", SourceURI},
	}
	for _, tt := range tests {
		if got, ok := s.Locator(tt.provider, tt.endpoint); !ok || got != tt.want {
			t.Errorf("Locator(%s, %s) = %v, %v; want %v", tt.provider, tt.endpoint, got, ok, tt.want)
		}
	}
	refOnly := Artifact("saige-artifact://d", MediaPDF)
	if _, ok := refOnly.Locator("openai", ""); ok {
		t.Error("a workspace reference was offered to a provider")
	}
	if refOnly.Digest != "d" || refOnly.Elided() {
		t.Errorf("artifact source = %+v", refOnly)
	}
	if !reflect.DeepEqual(s.Kinds(), []SourceKind{SourceFile, SourceURI, SourceInline, SourceRef}) {
		t.Errorf("Kinds = %v", s.Kinds())
	}
}

func TestSourceElidedAndUnavailable(t *testing.T) {
	s := Bytes(MediaPNG, []byte{1})
	s.Inline = nil
	if !s.Elided() {
		t.Fatal("a source with only a digest is not elided")
	}
	u := s.Unavailable("no resolver for scheme s3")
	if u.Unresolved == "" || s.Unresolved != "" {
		t.Fatalf("Unavailable = %+v (receiver %+v)", u, s)
	}
}
