package agent

import (
	"context"
	"errors"
	"fmt"

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
		if !ok || len(src.Inline) == 0 || a.holds(ctx, ws, src) {
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

// holds reports whether ws already holds the artifact src references, so a
// ref resolved from the workspace is not written again. A ref the
// workspace does not hold, such as one a host's client upload store
// issued, is stored, so the record stays restorable from the workspace.
func (a *Agent) holds(ctx context.Context, ws workspace.Workspace, src types.Source) bool {
	if src.Ref == "" {
		return false
	}
	ref, err := workspace.ParseRef(src.Ref)
	if err != nil {
		return true // not an artifact ref: nothing to write it under
	}
	_, err = ws.Stat(ctx, ref)
	return err == nil
}

// resolveRefs attaches the bytes of every media part whose only bytes are
// a saige-artifact reference, as a stored conversation reads back, so the
// provider is sent the media rather than a locator it cannot read. The
// bytes come from the agent's workspace, or else from the Resolver
// registered for the saige-artifact scheme (a host's upload store). A ref
// neither holds, or whose bytes do not match its digest, marks the part
// unavailable with the reason when no other locator is left, so the
// attempt's conversion plan rejects it (or omits it, when the dial
// permits) instead of the part being dropped. The record is not changed.
func (a *Agent) resolveRefs(ctx context.Context, messages []types.Message) []types.Message {
	var out []types.Message
	for i, msg := range messages {
		changed := false
		res := mapMedia(msg, func(p types.Part) types.Part {
			src, ok := types.SourceOf(p)
			if !ok || len(src.Inline) > 0 || src.Ref == "" || src.Unresolved != "" {
				return p
			}
			changed = true
			data, mt, err := a.readRef(ctx, src.Ref)
			if err == nil && src.Digest != "" && workspace.Digest(data) != src.Digest {
				err = fmt.Errorf("the bytes of %s do not match sha256 %s", src.Ref, src.Digest)
			}
			if err != nil {
				if src.URI != "" || len(src.Files) > 0 {
					return p // another locator may still serve it
				}
				a.cfg.Logger.Warn("media reference could not be resolved",
					"agent", a.cfg.Name, "ref", src.Ref, "media_type", src.MediaType, "error", err)
				return setSource(p, src.Unavailable(err.Error()))
			}
			if src.MediaType == "" {
				src.MediaType = mt
			}
			return setSource(p, types.Bytes(src.MediaType, data).With(src))
		})
		if !changed {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		if out == nil {
			out = make([]types.Message, 0, len(messages))
			out = append(out, messages[:i]...)
		}
		out = append(out, res)
	}
	if out == nil {
		return messages
	}
	return out
}

// readRef returns the bytes and media type of a saige-artifact reference.
func (a *Agent) readRef(ctx context.Context, uri string) ([]byte, types.MediaType, error) {
	ref, err := workspace.ParseRef(uri)
	if err != nil {
		return nil, "", err
	}
	if ws := a.cfg.Workspace; ws != nil {
		data, err := ws.Read(ctx, ref, 0, 0)
		if err == nil {
			var mt types.MediaType
			if st, err := ws.Stat(ctx, ref); err == nil {
				mt = types.MediaType(st.Meta["media_type"])
			}
			return data, mt, nil
		}
		if !errors.Is(err, workspace.ErrNotFound) {
			return nil, "", err
		}
	}
	if r, ok := a.cfg.Resolvers[workspace.URIScheme]; ok {
		f, err := r.Resolve(ctx, uri)
		if err != nil {
			return nil, "", err
		}
		return f.Data, f.MediaType, nil
	}
	if a.cfg.Workspace == nil {
		return nil, "", fmt.Errorf("no workspace or %s resolver to read %s", workspace.URIScheme, uri)
	}
	return nil, "", fmt.Errorf("%w: %s", workspace.ErrNotFound, uri)
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
