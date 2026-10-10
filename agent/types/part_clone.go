package types

import (
	"bytes"
	"encoding/json"
	"slices"
)

// ClonePart returns a deep copy of p: byte slices, maps and nested parts are
// copied, so the copy shares no memory with p.
func ClonePart[P Part](p P) P {
	c, _ := clonePart(Part(p)).(P)
	return c
}

// CloneParts deep-copies each part of ps.
func CloneParts[P Part](ps []P) []P {
	if ps == nil {
		return nil
	}
	out := make([]P, len(ps))
	for i, p := range ps {
		out[i] = ClonePart(p)
	}
	return out
}

//nolint:gocyclo // one case per part kind
func clonePart(p Part) Part {
	switch v := p.(type) {
	case JSONPart:
		v.JSON = bytes.Clone(v.JSON)
		return v
	case ImagePart:
		v.Source = v.Source.clone()
		return v
	case AudioPart:
		v.Source = v.Source.clone()
		return v
	case VideoPart:
		v.Source = v.Source.clone()
		return v
	case DocumentPart:
		v.Source = v.Source.clone()
		return v
	case FilePart:
		v.Source = v.Source.clone()
		return v
	case AudioOutPart:
		v.Source = v.Source.clone()
		return v
	case ImageOutPart:
		v.Source = v.Source.clone()
		return v
	case VideoOutPart:
		v.Source = v.Source.clone()
		return v
	case ToolResultPart:
		v.Parts = CloneParts(v.Parts)
		v.Citations = cloneCitations(v.Citations)
		return v
	case ToolCallPart:
		v.Arguments = cloneMap(v.Arguments)
		return v
	case ServerToolCallPart:
		v.Input = cloneMap(v.Input)
		return v
	case ServerToolResultPart:
		v.Result = bytes.Clone(v.Result)
		v.Outputs = CloneParts(v.Outputs)
		return v
	case CitationPart:
		v.Citation = cloneCitations([]Citation{v.Citation})[0]
		if v.Anchor != nil {
			a := *v.Anchor
			v.Anchor = &a
		}
		return v
	case TruncationPart:
		v.Dropped = slices.Clone(v.Dropped)
		return v
	default:
		// Text, thinking, refusal and the metadata parts hold no memory a
		// caller could mutate through a copy except metadata internals that
		// the loop treats as immutable.
		return p
	}
}

func (s Source) clone() Source {
	s.Inline = bytes.Clone(s.Inline)
	s.Files = slices.Clone(s.Files)
	return s
}

func cloneCitations(cs []Citation) []Citation {
	if cs == nil {
		return nil
	}
	out := slices.Clone(cs)
	for i := range out {
		out[i].Meta = cloneMap(out[i].Meta)
	}
	return out
}

// cloneMap deep-copies a JSON-shaped map. Values that are not maps, slices,
// json.Number or scalars are copied by reference.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	case json.RawMessage:
		return bytes.Clone(x)
	default:
		return v
	}
}
