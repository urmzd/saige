package types

import (
	"errors"
	"testing"

	agenttypes "github.com/urmzd/saige/agent/types"
)

func TestVariantPartRoundTrip(t *testing.T) {
	data := []byte("png bytes")
	parts := []agenttypes.UserPart{
		agenttypes.Text("hello"),
		agenttypes.Image(agenttypes.Bytes(agenttypes.MediaPNG, data)),
		agenttypes.Audio(agenttypes.Bytes(agenttypes.MediaWAV, data)),
		agenttypes.Video(agenttypes.Bytes("video/mp4", data)),
		agenttypes.Document(agenttypes.Bytes(agenttypes.MediaPDF, data)),
	}
	want := []ContentType{ContentText, ContentImage, ContentAudio, ContentVideo, ContentDocument}
	for i, p := range parts {
		v, err := VariantFromPart(p)
		if err != nil {
			t.Fatalf("%s: %v", p.Kind(), err)
		}
		if v.ContentType != want[i] {
			t.Fatalf("%s: content type %s, want %s", p.Kind(), v.ContentType, want[i])
		}
		back, err := v.Part()
		if err != nil {
			t.Fatalf("%s: %v", p.Kind(), err)
		}
		if back.Kind() != p.Kind() {
			t.Fatalf("round trip kind %s, want %s", back.Kind(), p.Kind())
		}
		if src, ok := agenttypes.SourceOf(back); ok {
			orig, _ := agenttypes.SourceOf(p)
			if src.Digest != orig.Digest || src.MediaType != orig.MediaType {
				t.Fatalf("source %+v, want %+v", src, orig)
			}
		}
	}
}

func TestVariantPartRefusals(t *testing.T) {
	if _, err := VariantFromPart(agenttypes.Image(agenttypes.URL("https://x/y.png", agenttypes.MediaPNG))); !errors.Is(err, ErrVariantPart) {
		t.Fatalf("a part without bytes made a variant: %v", err)
	}
	if _, err := VariantFromPart(agenttypes.File(agenttypes.Bytes("application/zip", []byte("z")))); !errors.Is(err, ErrVariantPart) {
		t.Fatalf("a file part made a variant: %v", err)
	}
	// A media variant stored without bytes reads as its text.
	p, err := ContentVariant{ContentType: ContentImage, Text: "a red square"}.Part()
	if err != nil || p.Kind() != agenttypes.KindText {
		t.Fatalf("part = %#v, %v", p, err)
	}
	if _, err := (ContentVariant{ContentType: ContentImage}).Part(); !errors.Is(err, ErrVariantPart) {
		t.Fatalf("an empty image variant made a part: %v", err)
	}
	table, err := ContentVariant{ContentType: ContentTable, Text: "| a |"}.Part()
	if err != nil || table.Kind() != agenttypes.KindText {
		t.Fatalf("table = %#v, %v", table, err)
	}
}
