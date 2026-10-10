package ollama

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// imageTypes are the formats the chat API's images field takes. Whether the
// model can read them depends on the pulled weights (a vision model).
var imageTypes = map[types.MediaType]bool{
	types.MediaJPEG: true,
	types.MediaPNG:  true,
}

// partError reports a part the chat API cannot carry. It wraps sentinel
// (types.ErrModalityUnsupported or types.ErrMediaUnavailable) and names the
// part by its path, kind and media type.
func partError(sentinel error, path types.PartPath, p types.Part, reason string) error {
	desc := string(p.Kind())
	if src, ok := types.SourceOf(p); ok && src.MediaType != "" {
		desc += " " + string(src.MediaType)
	}
	return fmt.Errorf("%w: ollama: part %s (%s): %s", sentinel, path, desc, reason)
}

// inlineImage returns the base64 form of an image part for the images
// field. Ollama reads media inline only: a part reachable only by URI,
// workspace reference or vendor file is rejected, as is a part with no
// locator left.
func inlineImage(path types.PartPath, p types.ImagePart) (string, error) {
	src := p.Source
	switch {
	case src.Unresolved != "":
		return "", partError(types.ErrMediaUnavailable, path, p, src.Unresolved)
	case src.Elided():
		return "", partError(types.ErrMediaUnavailable, path, p, "no locator is left: the bytes were not persisted")
	case !imageTypes[src.MediaType]:
		return "", partError(types.ErrModalityUnsupported, path, p, "the images field takes JPEG and PNG only")
	case len(src.Inline) == 0:
		return "", partError(types.ErrModalityUnsupported, path, p,
			fmt.Sprintf("ollama takes inline image bytes only, the source has %v", src.Kinds()))
	}
	return base64.StdEncoding.EncodeToString(src.Inline), nil
}

// toOllamaMessages maps typed parts to chat messages. Text goes to content,
// images to the images field, thinking to the thinking field, tool calls to
// tool_calls and tool results to tool messages. Metadata parts are never
// sent. Any other part is rejected before the request: the chat API has no
// audio, video, document or file input, and no server tools.
//
//nolint:gocyclo // one case per part kind and role
func toOllamaMessages(msgs []types.Message) ([]ChatMessage, error) {
	out := make([]ChatMessage, 0, len(msgs))
	// names maps a tool call ID to its tool name, for tool messages.
	names := map[string]string{}
	for i, m := range msgs {
		at := func(j int) types.PartPath { return types.PartPath{Message: i, Part: j, Nested: -1} }
		switch v := m.(type) {
		case types.SystemMessage:
			// Text goes to the system role, tool results to the tool role.
			var text []string
			var tools []ChatMessage
			for j, p := range v.Parts {
				switch bc := p.(type) {
				case types.TextPart:
					text = append(text, bc.Text)
				case types.ToolResultPart:
					tm, err := toolMessage(i, j, bc, names)
					if err != nil {
						return nil, err
					}
					tools = append(tools, tm)
				default:
					if !types.IsMetadata(p) {
						return nil, partError(types.ErrModalityUnsupported, at(j), p, "not supported in a system message")
					}
				}
			}
			if len(text) > 0 {
				out = append(out, ChatMessage{Role: "system", Content: strings.Join(text, "")})
			}
			out = append(out, tools...)
		case types.UserMessage:
			// Text and images go to the user role, tool results to the tool
			// role.
			var text, images []string
			var tools []ChatMessage
			for j, p := range v.Parts {
				switch bc := p.(type) {
				case types.TextPart:
					text = append(text, bc.Text)
				case types.ImagePart:
					img, err := inlineImage(at(j), bc)
					if err != nil {
						return nil, err
					}
					images = append(images, img)
				case types.ToolResultPart:
					tm, err := toolMessage(i, j, bc, names)
					if err != nil {
						return nil, err
					}
					tools = append(tools, tm)
				default:
					if !types.IsMetadata(p) {
						return nil, partError(types.ErrModalityUnsupported, at(j), p, "the chat API has no input for this kind")
					}
				}
			}
			if len(text) > 0 || len(images) > 0 {
				out = append(out, ChatMessage{Role: "user", Content: strings.Join(text, ""), Images: images})
			}
			out = append(out, tools...)
		case types.AssistantMessage:
			msg := ChatMessage{Role: "assistant"}
			for j, p := range v.Parts {
				switch bc := p.(type) {
				case types.TextPart:
					msg.Content += bc.Text
				case types.RefusalPart:
					// A refusal is what the model said.
					msg.Content += bc.Text
				case types.ThinkingPart:
					// A redacted block has no text, and a signature has no
					// meaning to this runtime, so only the text is replayed.
					msg.Thinking += bc.Text
				case types.ToolCallPart:
					args := bc.Arguments
					if args == nil {
						args = map[string]any{}
					}
					names[bc.ID] = bc.Name
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: bc.ID, Function: ToolCallFunction{Name: bc.Name, Arguments: args}})
				case types.CitationPart:
					// A citation annotates text already in content.
				default:
					if !types.IsMetadata(p) {
						return nil, partError(types.ErrModalityUnsupported, at(j), p, "the chat API cannot replay this kind")
					}
				}
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

// toolMessage maps a tool result at message i, part j to a tool message:
// its text and JSON parts become content and its images the images field.
func toolMessage(i, j int, tr types.ToolResultPart, names map[string]string) (ChatMessage, error) {
	msg := ChatMessage{Role: "tool", ToolCallID: tr.CallID, ToolName: names[tr.CallID]}
	for k, p := range tr.Parts {
		path := types.PartPath{Message: i, Part: j, Nested: k}
		switch bc := p.(type) {
		case types.TextPart, types.JSONPart:
		case types.ImagePart:
			img, err := inlineImage(path, bc)
			if err != nil {
				return ChatMessage{}, err
			}
			msg.Images = append(msg.Images, img)
		default:
			return ChatMessage{}, partError(types.ErrModalityUnsupported, path, p, "a tool message carries text and images only")
		}
	}
	msg.Content = tr.Text()
	if tr.IsError {
		msg.Content = "[TOOL ERROR] " + msg.Content
	}
	return msg, nil
}
