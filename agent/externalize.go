package agent

import (
	"context"
	"errors"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// externalize stores the media bytes of msg in the agent's workspace before
// the message is committed to the tree. Stores never hold bytes (a stored
// source keeps only its locators and digest), so a source whose only
// locator is its bytes would read back elided; with the bytes in the
// workspace it reads back with a saige-artifact reference instead. The
// bytes stay on the in-memory message, so the run itself is unaffected.
// Content addressing makes the write idempotent, so a durable replay writes
// nothing new.
//
// Without a workspace, or through a read-only one, msg is returned as it
// is: its sources keep their digest and size and lose only the bytes.
func (a *Agent) externalize(ctx context.Context, msg types.Message) types.Message {
	ws := a.cfg.Workspace
	if ws == nil || msg == nil {
		return msg
	}
	put := func(p types.Part) types.Part {
		src, ok := types.SourceOf(p)
		if !ok || len(src.Inline) == 0 || src.Ref != "" {
			return p
		}
		ref, err := ws.Put(ctx, "media/"+workspace.Digest(src.Inline), src.Inline, map[string]string{"media_type": string(src.MediaType)})
		if err != nil {
			if !errors.Is(err, workspace.ErrReadOnly) {
				a.cfg.Logger.Warn("media bytes could not be stored in the workspace; the stored message keeps only their digest",
					"agent", a.cfg.Name, "media_type", src.MediaType, "error", err)
			}
			return p
		}
		src.Ref, src.Digest, src.Size = ref.URI(), ref.ID, int64(len(src.Inline))
		return setSource(p, src)
	}
	return mapMedia(msg, put)
}

// mapMedia returns msg with f applied to each media part, including the
// media inside tool and server tool results. It copies what it changes.
func mapMedia(msg types.Message, f func(types.Part) types.Part) types.Message {
	part := func(p types.Part) types.Part {
		switch v := p.(type) {
		case types.ToolResultPart:
			out := make([]types.ToolOutputPart, len(v.Parts))
			for i, o := range v.Parts {
				out[i] = f(o).(types.ToolOutputPart)
			}
			v.Parts = out
			return v
		case types.ServerToolResultPart:
			if v.Outputs == nil {
				return v
			}
			out := make([]types.Part, len(v.Outputs))
			for i, o := range v.Outputs {
				out[i] = f(o)
			}
			v.Outputs = out
			return v
		}
		return f(p)
	}
	switch m := msg.(type) {
	case types.SystemMessage:
		return types.SystemMessage{Parts: mapParts(m.Parts, part)}
	case types.UserMessage:
		return types.UserMessage{Parts: mapParts(m.Parts, part)}
	case types.AssistantMessage:
		return types.AssistantMessage{Parts: mapParts(m.Parts, part)}
	}
	return msg
}

func mapParts[P types.Part](ps []P, f func(types.Part) types.Part) []P {
	if ps == nil {
		return nil
	}
	out := make([]P, len(ps))
	for i, p := range ps {
		out[i] = f(p).(P)
	}
	return out
}

// setSource returns media part p with its source replaced.
func setSource(p types.Part, src types.Source) types.Part {
	switch v := p.(type) {
	case types.ImagePart:
		v.Source = src
		return v
	case types.AudioPart:
		v.Source = src
		return v
	case types.VideoPart:
		v.Source = src
		return v
	case types.DocumentPart:
		v.Source = src
		return v
	case types.FilePart:
		v.Source = src
		return v
	case types.AudioOutPart:
		v.Source = src
		return v
	case types.ImageOutPart:
		v.Source = src
		return v
	case types.VideoOutPart:
		v.Source = src
		return v
	}
	return p
}
