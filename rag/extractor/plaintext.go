// Package extractor provides ContentExtractor implementations for common document formats.
package extractor

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/urmzd/saige/rag/internal/textclean"
	"github.com/urmzd/saige/rag/types"
)

// PlainText extracts text documents by splitting on paragraph boundaries.
type PlainText struct{}

// Extract splits raw text data into sections by double-newline paragraph boundaries.
func (e *PlainText) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	text := textclean.String(string(raw.Data))
	docUUID := uuid.New().String()
	now := time.Now()

	paragraphs := splitParagraphs(text)

	sections := make([]types.Section, 0, len(paragraphs))
	for i, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		secUUID := uuid.New().String()
		varUUID := uuid.New().String()
		sections = append(sections, types.Section{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        i,
			Variants: []types.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: types.ContentText,
				MIMEType:    "text/plain",
				Text:        para,
				Metadata:    raw.Metadata,
			}},
		})
	}

	doc := &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     titleFromText(text),
		Metadata:  raw.Metadata,
		Sections:  sections,
		CreatedAt: now,
		UpdatedAt: now,
	}
	textclean.Document(doc)
	return doc, nil
}

func splitParagraphs(text string) []string {
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) <= 1 {
		// Fall back to single-newline splitting for content without double newlines.
		paragraphs = strings.Split(text, "\n")
	}
	return paragraphs
}

func titleFromText(text string) string {
	// Use the first line (trimmed) as the title, truncated to 100 chars.
	firstLine := strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
	firstLine = strings.TrimSpace(firstLine)
	if len(firstLine) > 100 {
		// Cut on a rune boundary so the title stays valid UTF-8.
		cut := 100
		for cut > 0 && !utf8.RuneStart(firstLine[cut]) {
			cut--
		}
		firstLine = firstLine[:cut] + "..."
	}
	return firstLine
}
