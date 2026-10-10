package types

import "context"

// Extractor converts raw file data into user-consumable content blocks.
// Returns []UserPart: extractors can produce multiple blocks
// (e.g., text + images from a DOCX).
type Extractor interface {
	Extract(ctx context.Context, data []byte, mediaType MediaType) ([]UserPart, error)
}

// ExtractorFunc adapts a plain function to the Extractor interface.
type ExtractorFunc func(ctx context.Context, data []byte, mediaType MediaType) ([]UserPart, error)

// Extract calls f.
func (f ExtractorFunc) Extract(ctx context.Context, data []byte, mediaType MediaType) ([]UserPart, error) {
	return f(ctx, data, mediaType)
}
