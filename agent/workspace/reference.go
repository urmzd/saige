package workspace

import (
	"context"
	"fmt"
	"strings"
)

// Default reference sizes, in estimated tokens.
const (
	// DefaultInlineTokens is the largest text sent to a model whole when a
	// caller passes data by reference only above a threshold.
	DefaultInlineTokens = 2000
	// DefaultPreviewTokens is how much of a referenced text the model sees.
	DefaultPreviewTokens = 200
)

// charsPerToken matches types.EstimateTokens, so thresholds here and the
// agent's compaction estimate agree.
const charsPerToken = 4

// EstimateTokens approximates the tokens of s at four bytes per token. It
// needs no tokenizer, so a reference decision costs nothing and is the same on
// every run.
func EstimateTokens(s string) int { return (len(s) + charsPerToken - 1) / charsPerToken }

// Reference is stored content as a model should see it: where the whole of it
// lives and a short preview. String renders it for a prompt or a tool result.
type Reference struct {
	Ref Ref
	// Preview is the start of the content, cut at a word boundary.
	Preview string
	// Tokens is the estimated size of the whole content.
	Tokens int
}

// URI returns the content's saige-artifact:// URI.
func (r Reference) URI() string { return r.Ref.URI() }

// String renders the reference with its preview and says how to read the
// rest with read_artifact and search_artifact.
func (r Reference) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[artifact %s", r.URI())
	if r.Ref.Name != "" {
		fmt.Fprintf(&b, " name=%q", r.Ref.Name)
	}
	fmt.Fprintf(&b, ": %d bytes, about %d tokens.", r.Ref.Size, r.Tokens)
	if r.Preview == "" {
		fmt.Fprintf(&b, " Read it with %s or find passages with %s.]", ReadArtifactToolName, SearchArtifactToolName)
		return b.String()
	}
	b.WriteString(" Preview:]\n")
	b.WriteString(r.Preview)
	if int64(len(r.Preview)) < r.Ref.Size {
		fmt.Fprintf(&b, "\n[preview ends at byte %d of %d. Read more with %s (uri %s, offset %d) or find passages with %s.]",
			len(r.Preview), r.Ref.Size, ReadArtifactToolName, r.URI(), len(r.Preview), SearchArtifactToolName)
	}
	return b.String()
}

// ReferenceOptions configures NewReference.
type ReferenceOptions struct {
	// PreviewTokens bounds the preview. 0 uses DefaultPreviewTokens; a
	// negative value gives no preview.
	PreviewTokens int
	// Meta is stored with the artifact.
	Meta map[string]string
}

// NewReference stores data in ws under name and returns a Reference to it.
// Hosts and tools use it to pass large data by reference: put the
// Reference's String in a task, a message, or a tool result instead of the
// data, and the model reads only what it needs with read_artifact. Storing
// the same bytes again returns the same URI.
func NewReference(ctx context.Context, ws Workspace, name string, data []byte, opts ReferenceOptions) (Reference, error) {
	if ws == nil {
		return Reference{}, fmt.Errorf("workspace: no workspace configured")
	}
	ref, err := ws.Put(ctx, name, data, opts.Meta)
	if err != nil {
		return Reference{}, err
	}
	return Reference{Ref: ref, Preview: preview(string(data), opts.PreviewTokens), Tokens: EstimateTokens(string(data))}, nil
}

// preview returns the start of text, at most tokens long, cut at a word
// boundary.
func preview(text string, tokens int) string {
	if tokens == 0 {
		tokens = DefaultPreviewTokens
	}
	if tokens < 0 {
		return ""
	}
	limit := tokens * charsPerToken
	if limit >= len(text) {
		return text
	}
	return text[:cutAtBoundary(text, limit)]
}

// Copy stores the content of src under name in dst and returns the new Ref.
// The content passes through the process, never through a model's context.
// Content is addressed by digest, so the copy shares src's ID and a backend
// that already holds the bytes stores nothing new. src and dst may be the
// same workspace.
func Copy(ctx context.Context, src Workspace, from Ref, dst Workspace, name string) (Ref, error) {
	if src == nil || dst == nil {
		return Ref{}, fmt.Errorf("workspace: no workspace configured")
	}
	stat, err := src.Stat(ctx, from)
	if err != nil {
		return Ref{}, err
	}
	data, err := src.Read(ctx, stat, 0, 0)
	if err != nil {
		return Ref{}, err
	}
	meta := copyMeta(stat.Meta)
	if stat.Name != "" {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["copied_from"] = stat.Name
	}
	return dst.Put(ctx, name, data, meta)
}
