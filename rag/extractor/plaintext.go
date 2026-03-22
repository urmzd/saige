// Package extractor provides ContentExtractor implementations for common document formats.
package extractor

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

// PlainText extracts text documents by splitting on paragraph boundaries.
type PlainText struct{}

// Extract splits raw text data into sections by double-newline paragraph boundaries.
func (e *PlainText) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	text := string(raw.Data)
	docUUID := uuid.New().String()
	now := time.Now()

	paragraphs := splitParagraphs(text)

	sections := make([]ragtypes.Section, 0, len(paragraphs))
	for i, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		secUUID := uuid.New().String()
		varUUID := uuid.New().String()
		sections = append(sections, ragtypes.Section{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        i,
			Variants: []ragtypes.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: ragtypes.ContentText,
				MIMEType:    "text/plain",
				Text:        para,
				Metadata:    raw.Metadata,
			}},
		})
	}

	return &ragtypes.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     titleFromText(text),
		Metadata:  raw.Metadata,
		Sections:  sections,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
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
		firstLine = firstLine[:100] + "..."
	}
	return firstLine
}
