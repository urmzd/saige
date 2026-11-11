package native

import (
	"context"
	"strconv"
	"strings"

	"github.com/urmzd/saige/flow/event"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// Retriever grounds a turn in a corpus.
//
// It is the narrow half of saige's rag Pipeline: a turn retrieves, it does not
// ingest, so flow depends on one method instead of on a pipeline with a
// store, an embedder, and a chunker behind it. A deployment with a real RAG
// pipeline adapts it in four lines (Search, then take .Context); a deployment
// with none passes nothing and the agent answers from the model alone.
//
// The returned context is used twice: its Prompt is prepended to the turn's
// input so the model can see the passages, and its Blocks become citation
// events so a reader can check them.
type Retriever interface {
	Retrieve(ctx context.Context, query string) (*ragtypes.AssembledContext, error)
}

// Citations converts an assembled RAG context into citation event bodies, in
// the order the context lists them.
//
// The retrieval result already carries exactly what a footnote needs: the
// exact source text, the inline label the assembled prompt used, and
// resolvable provenance. Deriving citations from retrieval rather than from
// the model also makes them honest: they report what the turn was GIVEN,
// which a model cannot misattribute, rather than what it claims to have used.
// The sources a tool cites are published as well, from saige's CitationDelta
// (see Translator.Delta); one that repeats a retrieved citation is not.
//
// A nil or empty context yields no bodies. A block with neither text nor a
// label is skipped: it would render as an empty footnote.
func Citations(ac *ragtypes.AssembledContext) []event.Body {
	if ac == nil || len(ac.Blocks) == 0 {
		return nil
	}
	out := make([]event.Body, 0, len(ac.Blocks))
	for i, b := range ac.Blocks {
		text := strings.TrimSpace(b.Text)
		label := strings.TrimSpace(b.Citation)
		if text == "" && label == "" {
			continue
		}
		if label == "" {
			// Number it: a sink renders footnotes in order and needs a
			// marker even when the assembler did not emit one.
			label = "[" + strconv.Itoa(i+1) + "]"
		}
		out = append(out, event.Citation{
			Label:  label,
			Text:   text,
			Index:  i,
			Source: provenance(b.Provenance),
		})
	}
	return out
}

// provenance copies saige's provenance into the event body. It is a field
// for field copy so pkg/event stays free of saige.
func provenance(p ragtypes.Provenance) event.Provenance {
	return event.Provenance{
		DocumentUUID:   p.DocumentUUID,
		DocumentTitle:  p.DocumentTitle,
		SourceURI:      p.SourceURI,
		SectionUUID:    p.SectionUUID,
		SectionHeading: p.SectionHeading,
		SectionIndex:   p.SectionIndex,
	}
}
