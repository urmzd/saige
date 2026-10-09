package extractor

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/urmzd/saige/rag/internal/textclean"
	"github.com/urmzd/saige/rag/types"
	"golang.org/x/net/html"
)

// HTML extracts text content from HTML documents, splitting on heading elements.
type HTML struct{}

// Extract parses HTML and creates sections from heading-delimited blocks.
func (e *HTML) Extract(_ context.Context, raw *types.RawDocument) (*types.Document, error) {
	doc, err := html.Parse(bytes.NewReader(raw.Data))
	if err != nil {
		return nil, err
	}

	docUUID := uuid.New().String()
	now := time.Now()
	title := extractHTMLTitle(doc)

	blocks := extractTextBlocks(doc)

	sections := make([]types.Section, 0, len(blocks))
	for i, block := range blocks {
		text := strings.TrimSpace(block.text)
		if text == "" {
			continue
		}
		secUUID := uuid.New().String()
		varUUID := uuid.New().String()
		sections = append(sections, types.Section{
			UUID:         secUUID,
			DocumentUUID: docUUID,
			Index:        i,
			Heading:      block.heading,
			Variants: []types.ContentVariant{{
				UUID:        varUUID,
				SectionUUID: secUUID,
				ContentType: types.ContentText,
				MIMEType:    "text/html",
				Text:        text,
				Metadata:    raw.Metadata,
			}},
		})
	}

	if title == "" && len(sections) > 0 {
		title = titleFromText(sections[0].Variants[0].Text)
	}

	out := &types.Document{
		UUID:      docUUID,
		SourceURI: raw.SourceURI,
		Title:     title,
		Metadata:  raw.Metadata,
		Sections:  sections,
		CreatedAt: now,
		UpdatedAt: now,
	}
	textclean.Document(out)
	return out, nil
}

type textBlock struct {
	heading string
	text    string
}

func extractHTMLTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "title" {
		return extractInnerText(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if t := extractHTMLTitle(c); t != "" {
			return t
		}
	}
	return ""
}

func extractInnerText(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(extractInnerText(c))
	}
	return sb.String()
}

func isHeading(tag string) bool {
	return tag == "h1" || tag == "h2" || tag == "h3" || tag == "h4" || tag == "h5" || tag == "h6"
}

func isSkippedElement(tag string) bool {
	return tag == "script" || tag == "style" || tag == "nav" || tag == "footer" || tag == "header"
}

func extractTextBlocks(n *html.Node) []textBlock {
	var blocks []textBlock
	var currentHeading string
	var currentText strings.Builder

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if isSkippedElement(n.Data) {
				return
			}
			if isHeading(n.Data) {
				// Flush current block.
				if text := currentText.String(); strings.TrimSpace(text) != "" {
					blocks = append(blocks, textBlock{heading: currentHeading, text: text})
				}
				currentHeading = strings.TrimSpace(extractInnerText(n))
				currentText.Reset()
				return
			}
			if n.Data == "p" || n.Data == "div" || n.Data == "li" || n.Data == "br" {
				currentText.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			currentText.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)

	// Flush remaining.
	if text := currentText.String(); strings.TrimSpace(text) != "" {
		blocks = append(blocks, textBlock{heading: currentHeading, text: text})
	}

	return blocks
}
