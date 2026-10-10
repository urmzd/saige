// Package legacyparts gives the adapters the attachment and tool-output
// views their request mapping was written against, computed from typed
// parts. It keeps each adapter's wire mapping unchanged while messages carry
// parts; an adapter that maps parts directly no longer needs it.
package legacyparts

import "github.com/urmzd/saige/agent/types"

// Media is the attachment view of a media part.
type Media struct {
	URI       string
	MediaType types.MediaType
	Data      []byte
	Filename  string
}

// MediaOf returns the attachment view of p. It reports false for a part
// that carries no media.
func MediaOf(p types.Part) (Media, bool) {
	src, ok := types.SourceOf(p)
	if !ok {
		return Media{}, false
	}
	return Media{URI: src.URI, MediaType: src.MediaType, Data: src.Inline, Filename: src.Filename}, true
}

// BlockKind is the kind of a tool output Block.
type BlockKind string

// Block kinds.
const (
	BlockText  BlockKind = "text"
	BlockImage BlockKind = "image"
	BlockFile  BlockKind = "file" // non-image media
	BlockJSON  BlockKind = "json"
)

// Block is one element of rich tool output.
type Block struct {
	Kind      BlockKind
	Text      string
	MediaType types.MediaType
	URI       string
	Filename  string
	Data      []byte
	JSON      []byte
}

// Plain reports whether tool output is text alone, which adapters send as a
// plain string.
func Plain(parts []types.ToolOutputPart) bool {
	for _, p := range parts {
		if _, ok := p.(types.TextPart); !ok {
			return false
		}
	}
	return true
}

// Blocks returns the rich view of tool output, or nil when it is plain text.
func Blocks(parts []types.ToolOutputPart) []Block {
	if Plain(parts) {
		return nil
	}
	out := make([]Block, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case types.TextPart:
			out = append(out, Block{Kind: BlockText, Text: v.Text})
		case types.JSONPart:
			out = append(out, Block{Kind: BlockJSON, JSON: v.JSON})
		default:
			m, ok := MediaOf(p)
			if !ok {
				continue
			}
			kind := BlockFile
			if _, img := p.(types.ImagePart); img {
				kind = BlockImage
			}
			out = append(out, Block{Kind: kind, MediaType: m.MediaType, URI: m.URI, Filename: m.Filename, Data: m.Data})
		}
	}
	return out
}
