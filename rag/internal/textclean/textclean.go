// Package textclean normalizes extracted text so every store can persist it.
//
// PostgreSQL TEXT and JSONB columns reject NUL bytes and invalid UTF-8, so a
// single stray byte in a source file would otherwise fail the whole document
// write. Cleaning happens at extraction and again at the pipeline boundary,
// which also covers custom extractors and chunkers.
package textclean

import (
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/rag/types"
)

// String returns s as valid UTF-8 with every NUL byte removed. Invalid byte
// sequences become U+FFFD. It returns s unchanged (no allocation) when s is
// already clean.
func String(s string) string {
	if utf8.ValidString(s) && strings.IndexByte(s, 0) < 0 {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "")
}

// Metadata returns a cleaned copy of m when any key or value needs cleaning,
// and m itself otherwise. The input map is never modified because extractors
// often share one metadata map across many sections and variants.
func Metadata(m map[string]string) map[string]string {
	dirty := false
	for k, v := range m {
		if String(k) != k || String(v) != v {
			dirty = true
			break
		}
	}
	if !dirty {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[String(k)] = String(v)
	}
	return out
}

// Document cleans doc's title, source URI, and metadata, plus each section
// heading and each variant's text and metadata, in place. Binary variant data
// is left untouched because it is stored as bytes.
func Document(doc *types.Document) {
	if doc == nil {
		return
	}
	doc.Title = String(doc.Title)
	doc.SourceURI = String(doc.SourceURI)
	doc.Metadata = Metadata(doc.Metadata)
	for i := range doc.Sections {
		sec := &doc.Sections[i]
		sec.Heading = String(sec.Heading)
		for j := range sec.Variants {
			v := &sec.Variants[j]
			v.Text = String(v.Text)
			v.Metadata = Metadata(v.Metadata)
		}
	}
}
