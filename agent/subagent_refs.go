package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// DefaultResultInlineTokens is the largest sub-agent result returned to the
// parent inline. A larger one is returned as a reference with a preview.
const DefaultResultInlineTokens = workspace.DefaultInlineTokens

// SubAgentReferences sets when a delegation passes data by reference.
// Sizes are estimated tokens (see workspace.EstimateTokens), so the decision
// needs no tokenizer and is the same on every run and replay.
//
// A task, or a text block of a message the child starts with (see Context),
// over InputTokens is stored in the child's scratch. The child receives the
// artifact's URI and a preview in its place, and read_artifact and
// search_artifact to pull what it needs. With scratch off, inputs stay
// inline.
//
// A result over ResultTokens is stored in the parent's workspace when it
// accepts writes, and in the child's scratch otherwise. The parent model
// receives the URI and a preview, and reads more with read_artifact, which
// an agent with sub-agents has. SubAgentResult.Output keeps the whole result
// for the host either way.
type SubAgentReferences struct {
	// Off passes everything inline.
	Off bool
	// InputTokens is the largest task or attached text sent inline. 0 uses
	// workspace.DefaultInlineTokens.
	InputTokens int
	// ResultTokens is the largest result returned inline. 0 uses
	// DefaultResultInlineTokens.
	ResultTokens int
	// PreviewTokens bounds each preview. 0 uses
	// workspace.DefaultPreviewTokens.
	PreviewTokens int
}

func (r SubAgentReferences) withDefaults() SubAgentReferences {
	if r.InputTokens <= 0 {
		r.InputTokens = workspace.DefaultInlineTokens
	}
	if r.ResultTokens <= 0 {
		r.ResultTokens = DefaultResultInlineTokens
	}
	if r.PreviewTokens == 0 {
		r.PreviewTokens = workspace.DefaultPreviewTokens
	}
	return r
}

// referencesResults reports whether any definition may return its result by
// reference, so the parent needs read_artifact to follow it.
func referencesResults(defs []SubAgentDef) bool {
	for _, sa := range defs {
		if !sa.References.Off {
			return true
		}
	}
	return false
}

// Ref stores data in the workspace of the tool call ctx belongs to and
// returns a reference to show the model in place of the data: its
// saige-artifact:// URI and a preview. A tool returns it as its result so a
// large value costs the context only what the model then reads with
// read_artifact. Hosts outside a tool call use workspace.NewReference with
// their workspace directly.
func Ref(ctx context.Context, name string, data []byte) (string, error) {
	ws, ok := workspace.FromContext(ctx)
	if !ok {
		return "", errors.New("agent.Ref: no workspace attached to this call; configure one with WithWorkspace")
	}
	ref, err := workspace.NewReference(ctx, ws, name, data, workspace.ReferenceOptions{})
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

// passByReference moves a large task, and large text blocks of the history
// the child starts with, into the child's scratch. It returns what the child
// receives instead, and whether the child should get the artifact tools:
// something was stored, or the task already names an artifact.
func (t *subAgentTool) passByReference(ctx context.Context, child *Agent, task string, history []types.Message) (string, []types.Message, bool, error) {
	named := strings.Contains(task, workspace.URIScheme+"://")
	refs := t.refs.withDefaults()
	if refs.Off || child.scratch == nil {
		return task, history, named, nil
	}
	stored := false
	ref := func(name, text string) (string, error) {
		if workspace.EstimateTokens(text) <= refs.InputTokens {
			return text, nil
		}
		r, err := workspace.NewReference(ctx, child.scratch, name, []byte(text), workspace.ReferenceOptions{PreviewTokens: refs.PreviewTokens})
		if err != nil {
			return "", fmt.Errorf("store delegated input %s: %w", name, err)
		}
		stored = true
		return r.String(), nil
	}
	if text, err := ref("inputs/task", task); err != nil {
		return "", nil, false, err
	} else if text != task {
		task = "The task is stored as an artifact. Read the parts you need before you start.\n" + text
	}
	out := make([]types.Message, len(history))
	for i, m := range history {
		var err error
		if out[i], err = referenceMessage(m, i, ref); err != nil {
			return "", nil, false, err
		}
	}
	return task, out, stored || named, nil
}

// referenceMessage replaces the large text blocks and tool results of m with
// references. Tool results keep their call IDs, so pairing is unchanged.
func referenceMessage(m types.Message, index int, ref func(name, text string) (string, error)) (types.Message, error) {
	name := func(j int) string { return fmt.Sprintf("inputs/message-%d-%d", index, j) }
	switch v := m.(type) {
	case types.UserMessage:
		content := make([]types.UserContent, len(v.Content))
		for j, c := range v.Content {
			out, err := referenceBlock(c, name(j), ref)
			if err != nil {
				return nil, err
			}
			content[j] = out.(types.UserContent)
		}
		return types.UserMessage{Content: content}, nil
	case types.SystemMessage:
		content := make([]types.SystemContent, len(v.Content))
		for j, c := range v.Content {
			out, err := referenceBlock(c, name(j), ref)
			if err != nil {
				return nil, err
			}
			content[j] = out.(types.SystemContent)
		}
		return types.SystemMessage{Content: content}, nil
	case types.AssistantMessage:
		content := make([]types.AssistantContent, len(v.Content))
		for j, c := range v.Content {
			out, err := referenceBlock(c, name(j), ref)
			if err != nil {
				return nil, err
			}
			content[j] = out.(types.AssistantContent)
		}
		return types.AssistantMessage{Content: content}, nil
	}
	return m, nil
}

// referenceBlock replaces the text of a text block or a text-only tool
// result. Other blocks are returned unchanged.
func referenceBlock(c any, name string, ref func(name, text string) (string, error)) (any, error) {
	switch v := c.(type) {
	case types.TextContent:
		text, err := ref(name, v.Text)
		v.Text = text
		return v, err
	case types.ToolResultContent:
		if len(v.Blocks) > 0 {
			return v, nil
		}
		text, err := ref(name, v.Text)
		v.Text = text
		return v, err
	}
	return c, nil
}

// referenceResult stores a large result and returns the text the parent
// model receives in its place. It prefers the parent's workspace, which may
// outlive the run, and falls back to the child's scratch.
func (c *subAgentCapture) referenceResult(ctx context.Context, r *SubAgentResult) {
	refs := c.refs.withDefaults()
	if refs.Off || workspace.EstimateTokens(r.Output) <= refs.ResultTokens {
		return
	}
	name := "subagents/" + r.Name + "/" + r.ID + "/result"
	opts := workspace.ReferenceOptions{PreviewTokens: refs.PreviewTokens, Meta: map[string]string{"subagent": r.Name, "call": r.ID}}
	for _, ws := range []workspace.Workspace{c.parentWS, c.scratch} {
		if ws == nil {
			continue
		}
		ref, err := workspace.NewReference(ctx, ws, name, []byte(r.Output), opts)
		if err != nil {
			continue
		}
		r.OutputRef = ref.URI()
		r.parentText = "The result is stored as an artifact.\n" + ref.String()
		return
	}
}

// ParentText returns what the parent model receives for this result: the
// output, or a reference to it with a preview when it was too large to send
// inline, and a note when the child was forced to answer at a step limit.
func (r SubAgentResult) ParentText() string {
	text := r.Output
	if r.parentText != "" {
		text = r.parentText
	}
	if r.Forced {
		text = fmt.Sprintf("[The sub-agent reached its step limit after %d iterations and was made to answer without tools; the result may be incomplete.]\n%s", r.Iterations, text)
	}
	return text
}
