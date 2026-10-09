package textclean

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/types"
)

func TestString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "clean text unchanged", in: "hello world", want: "hello world"},
		{name: "empty", in: "", want: ""},
		{name: "nul removed", in: "a\x00b", want: "ab"},
		{name: "invalid utf8 replaced", in: "a\xffc", want: "a�c"},
		{name: "nul and invalid", in: "a\x00b\xffc", want: "ab�c"},
		{name: "multibyte kept", in: "café 日本", want: "café 日本"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.in)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
				t.Errorf("String(%q) = %q is not clean", tt.in, got)
			}
		})
	}
}

func TestMetadataDoesNotMutateInput(t *testing.T) {
	in := map[string]string{"k\x00": "v\xff", "ok": "fine"}
	out := Metadata(in)
	if _, ok := in["k\x00"]; !ok {
		t.Fatal("input map was modified")
	}
	if out["k"] != "v�" || out["ok"] != "fine" {
		t.Errorf("unexpected cleaned metadata: %q", out)
	}

	clean := map[string]string{"a": "b"}
	if got := Metadata(clean); got["a"] != "b" {
		t.Errorf("clean metadata changed: %v", got)
	}
}

func TestDocument(t *testing.T) {
	doc := &types.Document{
		Title:    "t\x00",
		Metadata: map[string]string{"m": "\x00"},
		Sections: []types.Section{{
			Heading: "h\xff",
			Variants: []types.ContentVariant{{
				Text:     "a\x00b\xffc",
				Metadata: map[string]string{"x": "y\x00"},
				Data:     []byte{0, 1, 2},
			}},
		}},
	}
	Document(doc)
	v := doc.Sections[0].Variants[0]
	if doc.Title != "t" || doc.Metadata["m"] != "" || doc.Sections[0].Heading != "h�" ||
		v.Text != "ab�c" || v.Metadata["x"] != "y" {
		t.Errorf("document not cleaned: %+v", doc)
	}
	if len(v.Data) != 3 || v.Data[0] != 0 {
		t.Errorf("binary data must be untouched, got %v", v.Data)
	}
	Document(nil)
}
