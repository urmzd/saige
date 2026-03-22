package extractor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dslipak/pdf"
	"github.com/google/uuid"
	"github.com/urmzd/graph-agent-dev-kit/rag/ragtypes"
)

// PDF extracts text content from PDF documents, creating one section per page.
type PDF struct{}

// Extract parses a PDF and creates sections from each page's text content.
func (e *PDF) Extract(_ context.Context, raw *ragtypes.RawDocument) (*ragtypes.Document, error) {
	reader, err := pdf.NewReader(bytes.NewReader(raw.Data), int64(len(raw.Data)))
	if err != nil {
		return nil, fmt.Errorf("parse pdf: %w", err)
	}

	docUUID := uuid.New().String()
	now := time.Now()
	numPages := reader.NumPage()

	sections := make([]ragtypes.Section, 0, numPages)
	for i := 1; i <= numPages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue // skip pages that fail to extract
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		secUUID := uuid.New().String()
		varUUID := uuid.New().String()
		sections = append(sections, ragtypes.Section{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        i - 1,
			Heading:      fmt.Sprintf("Page %d", i),
			Variants: []ragtypes.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: ragtypes.ContentText,
				MIMEType:    "application/pdf",
				Text:        text,
				Metadata:    raw.Metadata,
			}},
		})
	}

	title := raw.SourceURI
	if len(sections) > 0 {
		title = titleFromText(sections[0].Variants[0].Text)
	}

	return &ragtypes.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     title,
		Metadata:  raw.Metadata,
		Sections:  sections,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}
