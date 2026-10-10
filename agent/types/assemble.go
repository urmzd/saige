package types

import (
	"sort"
	"strings"
)

// PartAssembler builds the parts of an assistant turn from part deltas.
// Deltas for different indices may interleave in any order; the parts come
// out sorted by index. It is the engine behind the agent's DefaultAggregator,
// and it serves any consumer that needs the turn a stream describes.
//
// The rules:
//   - PartStart opens its index. A start for an open index restarts it; a
//     start for a closed index is a violation and is ignored.
//   - PartDelta appends to its open index. A delta for an index that is not
//     open is a violation and is dropped: there is no implicit start.
//   - PartEnd closes its index. A non-nil End.Part is the part; otherwise
//     the part is built from the fragments. Tool-call arguments that are not
//     valid JSON set ArgumentsError and never yield an empty map (D-36).
//     An end for an index that is not open is a violation and is ignored.
//
// It is not safe for concurrent use.
type PartAssembler struct {
	open       map[int]*openPart
	closed     map[int]AssistantPart
	violations int
}

type openPart struct {
	kind      PartKind
	mediaType MediaType
	id, name  string
	text      strings.Builder
	sig       strings.Builder
	args      strings.Builder
	refusal   strings.Builder
	trans     strings.Builder
	data      []byte
}

// NewPartAssembler returns an empty assembler.
func NewPartAssembler() *PartAssembler {
	return &PartAssembler{open: map[int]*openPart{}, closed: map[int]AssistantPart{}}
}

func (a *PartAssembler) init() {
	if a.open == nil {
		a.open = map[int]*openPart{}
		a.closed = map[int]AssistantPart{}
	}
}

// Push applies one delta. Deltas other than part deltas are ignored.
func (a *PartAssembler) Push(d Delta) {
	a.init()
	switch v := d.(type) {
	case PartStart:
		if _, done := a.closed[v.Index]; done {
			a.violations++
			return
		}
		a.open[v.Index] = &openPart{kind: v.Kind, mediaType: v.MediaType, id: v.ID, name: v.Name}
	case PartDelta:
		o := a.open[v.Index]
		if o == nil {
			a.violations++
			return
		}
		o.text.WriteString(v.Text)
		o.text.WriteString(v.Thinking)
		o.sig.WriteString(v.Signature)
		o.args.WriteString(v.Args)
		o.refusal.WriteString(v.Refusal)
		o.trans.WriteString(v.Transcript)
		o.data = append(o.data, v.Data...)
	case PartEnd:
		o := a.open[v.Index]
		if o == nil {
			a.violations++
			return
		}
		delete(a.open, v.Index)
		p := v.Part
		if p == nil {
			p = o.build()
		}
		if p == nil {
			a.violations++
			return
		}
		a.closed[v.Index] = p
	}
}

// build makes the part an end without a Part closes.
func (o *openPart) build() AssistantPart {
	switch o.kind {
	case KindText:
		return TextPart{Text: o.text.String()}
	case KindThinking:
		return ThinkingPart{Text: o.text.String(), Signature: o.sig.String()}
	case KindToolCall:
		args, argsErr := DecodeToolArguments(o.args.String())
		return ToolCallPart{ID: o.id, Name: o.name, Arguments: args, ArgumentsError: argsErr}
	case KindServerToolCall:
		args, _ := DecodeToolArguments(o.args.String())
		return ServerToolCallPart{ID: o.id, Name: o.name, Input: args}
	case KindServerToolResult:
		return ServerToolResultPart{CallID: o.id, Text: o.text.String()}
	case KindRefusal:
		return RefusalPart{Text: o.refusal.String()}
	case KindAudioOut:
		return AudioOutPart{Source: o.source(), Transcript: o.trans.String()}
	case KindImageOut:
		return ImageOutPart{Source: o.source()}
	case KindVideoOut:
		return VideoOutPart{Source: o.source()}
	default:
		return nil
	}
}

func (o *openPart) source() Source {
	if len(o.data) == 0 {
		return Source{MediaType: o.mediaType}
	}
	return Bytes(o.mediaType, o.data)
}

// Parts returns the turn so far: the closed parts and any text part still
// streaming, sorted by index. Open parts of other kinds are not included.
func (a *PartAssembler) Parts() []AssistantPart {
	idx := make([]int, 0, len(a.closed)+len(a.open))
	for i := range a.closed {
		idx = append(idx, i)
	}
	for i, o := range a.open {
		if o.kind == KindText && o.text.Len() > 0 {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	sort.Ints(idx)
	out := make([]AssistantPart, 0, len(idx))
	for _, i := range idx {
		if p, ok := a.closed[i]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, TextPart{Text: a.open[i].text.String()})
	}
	return out
}

// Truncated reports whether a part is still open. A stream that stops in
// this state was cut short rather than finished.
func (a *PartAssembler) Truncated() bool { return len(a.open) > 0 }

// OpenToolCalls returns the IDs of tool calls that started but have not
// ended, in index order.
func (a *PartAssembler) OpenToolCalls() []string {
	var idx []int
	for i, o := range a.open {
		if o.kind == KindToolCall {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	sort.Ints(idx)
	ids := make([]string, len(idx))
	for k, i := range idx {
		ids[k] = a.open[i].id
	}
	return ids
}

// Flush closes the turn for a partial commit, for example after a stop
// request or a max-token cutoff. Open text is kept as complete text. Open
// thinking is dropped because its signature would be invalid, open tool
// calls because their arguments are incomplete and must never run (D-23),
// and open media and refusals because they are incomplete. It returns the
// committed parts, whether anything was open, and the kinds dropped, in
// index order.
func (a *PartAssembler) Flush() (parts []AssistantPart, truncated bool, dropped []PartKind) {
	a.init()
	truncated = a.Truncated()
	idx := make([]int, 0, len(a.open))
	for i := range a.open {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		o := a.open[i]
		if o.kind == KindText {
			if o.text.Len() > 0 {
				a.closed[i] = TextPart{Text: o.text.String()}
			}
			continue
		}
		dropped = append(dropped, o.kind)
	}
	a.open = map[int]*openPart{}
	return a.Parts(), truncated, dropped
}

// Violations counts deltas that broke the protocol: a delta or end for an
// index that is not open, a start for a closed index, or an end that left
// nothing to build.
func (a *PartAssembler) Violations() int { return a.violations }

// Reset empties the assembler for the next turn.
func (a *PartAssembler) Reset() {
	a.open = map[int]*openPart{}
	a.closed = map[int]AssistantPart{}
	a.violations = 0
}

// PartDeltas returns the deltas that stream p at index: a start, the text
// or thinking as one fragment where the kind streams fragments, and an end
// that carries p. Replays, batch results and test fixtures use it to turn a
// finished part back into a stream.
func PartDeltas(index int, p AssistantPart) []Delta {
	start := PartStart{Index: index, Kind: p.Kind()}
	var mid []Delta
	switch v := p.(type) {
	case TextPart:
		if v.Text != "" {
			mid = append(mid, PartDelta{Index: index, Text: v.Text})
		}
	case ThinkingPart:
		if v.Text != "" {
			mid = append(mid, PartDelta{Index: index, Thinking: v.Text})
		}
	case ToolCallPart:
		start.ID, start.Name = v.ID, v.Name
	case ServerToolCallPart:
		start.ID, start.Name = v.ID, v.Name
	case ServerToolResultPart:
		start.ID = v.CallID
	}
	if src, ok := SourceOf(p); ok {
		start.MediaType = src.MediaType
	}
	out := append([]Delta{start}, mid...)
	return append(out, PartEnd{Index: index, Part: p})
}

// MessageDeltas streams every part of a message at consecutive indices,
// starting at 0.
func MessageDeltas(m AssistantMessage) []Delta {
	var out []Delta
	for i, p := range m.Parts {
		out = append(out, PartDeltas(i, p)...)
	}
	return out
}
