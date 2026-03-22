package extraction

import (
	"fmt"
	"strings"

	"github.com/urmzd/saige/knowledge/types"
)

// BuildExtractionPrompt creates an ontology-aware extraction prompt.
func BuildExtractionPrompt(text string, ont *types.Ontology) string {
	var b strings.Builder
	b.WriteString(`Extract entities and relationships from this text. Return ONLY valid JSON with no extra text:
{"entities": [{"name": "...", "type": "...", "summary": "..."}],
 "relations": [{"source": "...", "target": "...", "type": "...", "fact": "..."}]}`)

	if ont != nil && (len(ont.EntityTypes) > 0 || len(ont.RelationTypes) > 0) {
		b.WriteString("\n\nKnown entity types: ")
		for i, et := range ont.EntityTypes {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(et.Name)
		}
		b.WriteString("\nKnown relation types: ")
		for i, rt := range ont.RelationTypes {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(rt.Name)
		}
	}

	fmt.Fprintf(&b, "\n\nText: %s", text)
	return b.String()
}
