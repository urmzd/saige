package convert

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// AsExtractor runs a converter as a types.Extractor, outside any request:
// the bytes become a media part of their type, and the converter's text
// parts are the extraction. It lets a converter that calls a model, such as
// Describe, serve the RAG extractor registry (rag/extractor.Auto
// RegisterImages and RegisterParts), so an image is ingested by its
// description. Bytes the converter does not accept are an error.
func AsExtractor(c types.Converter) types.Extractor {
	return types.ExtractorFunc(func(ctx context.Context, data []byte, mt types.MediaType) ([]types.UserPart, error) {
		part := types.Media(types.Bytes(mt, data))
		if !c.Accepts(part) {
			return nil, fmt.Errorf("%s@%s does not accept %s", c.Name(), c.Version(), mt)
		}
		parts, _, err := c.Convert(ctx, part, types.ConvertEnv{})
		if err != nil {
			return nil, err
		}
		out := make([]types.UserPart, 0, len(parts))
		for _, p := range parts {
			if up, ok := p.(types.UserPart); ok {
				out = append(out, up)
			}
		}
		return out, nil
	})
}
