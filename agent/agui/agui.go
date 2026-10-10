// Package agui maps an agent's Delta stream to AG-UI protocol events, so an
// AG-UI client can render a run without knowing this module's wire format.
//
// Text, tool calls, tool results and thinking map to their AG-UI events,
// keyed by part index. Media the model produced, and media in a tool
// result, become a CUSTOM event named MediaEventName that carries a link to
// the media (or, when it has none, its bytes as an attachment); a refusal
// becomes a CUSTOM event named RefusalEventName. Every other delta (markers, interrupts, usage, routes, handoffs, sub-agent
// output) becomes a CUSTOM event named CustomEventName whose value is the
// delta's wire envelope from the agent/types codec, so no information is lost
// and a client that knows the codec can decode it.
package agui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/urmzd/saige/agent/types"
)

// EventType is an AG-UI event type.
type EventType string

// AG-UI event types the mapper emits.
const (
	RunStarted                 EventType = "RUN_STARTED"
	RunFinished                EventType = "RUN_FINISHED"
	RunError                   EventType = "RUN_ERROR"
	TextMessageStart           EventType = "TEXT_MESSAGE_START"
	TextMessageContent         EventType = "TEXT_MESSAGE_CONTENT"
	TextMessageEnd             EventType = "TEXT_MESSAGE_END"
	ToolCallStart              EventType = "TOOL_CALL_START"
	ToolCallArgs               EventType = "TOOL_CALL_ARGS"
	ToolCallEnd                EventType = "TOOL_CALL_END"
	ToolCallResult             EventType = "TOOL_CALL_RESULT"
	ThinkingStart              EventType = "THINKING_START"
	ThinkingTextMessageStart   EventType = "THINKING_TEXT_MESSAGE_START"
	ThinkingTextMessageContent EventType = "THINKING_TEXT_MESSAGE_CONTENT"
	ThinkingTextMessageEnd     EventType = "THINKING_TEXT_MESSAGE_END"
	ThinkingEnd                EventType = "THINKING_END"
	Custom                     EventType = "CUSTOM"
)

// CustomEventName names the CUSTOM events that carry a wire envelope.
const CustomEventName = "saige.delta"

// MediaEventName names the CUSTOM events that carry one media part as a
// Media value.
const MediaEventName = "saige.media"

// RefusalEventName names the CUSTOM events that carry a refusal as a
// Refusal value.
const RefusalEventName = "saige.refusal"

// Media is the value of a MediaEventName event. URL links to the bytes;
// Data carries them only when there is no link. Unresolved says why the
// bytes were not kept.
type Media struct {
	Kind       types.PartKind  `json:"kind"`
	Index      *int            `json:"index,omitempty"`
	MediaType  types.MediaType `json:"media_type,omitempty"`
	Filename   string          `json:"filename,omitempty"`
	Size       int64           `json:"size,omitempty"`
	Digest     string          `json:"sha256,omitempty"`
	URL        string          `json:"url,omitempty"`
	Data       []byte          `json:"data,omitempty"`
	Transcript string          `json:"transcript,omitempty"`
	Unresolved string          `json:"unresolved,omitempty"`
}

// Refusal is the value of a RefusalEventName event.
type Refusal struct {
	Index    int    `json:"index"`
	Text     string `json:"text,omitempty"`
	Category string `json:"category,omitempty"`
}

// CodeIncompleteStream is the RUN_ERROR code for a stream that ended
// without a done or error delta.
const CodeIncompleteStream = "incomplete_stream"

// Roles used in AG-UI messages.
const (
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// Event is one AG-UI event. Only the fields of its Type are set; the rest
// are omitted from the JSON encoding.
type Event struct {
	Type            EventType       `json:"type"`
	ThreadID        string          `json:"threadId,omitempty"`
	RunID           string          `json:"runId,omitempty"`
	MessageID       string          `json:"messageId,omitempty"`
	Role            string          `json:"role,omitempty"`
	Delta           string          `json:"delta,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	ToolCallName    string          `json:"toolCallName,omitempty"`
	ParentMessageID string          `json:"parentMessageId,omitempty"`
	Content         string          `json:"content,omitempty"`
	Message         string          `json:"message,omitempty"`
	Code            string          `json:"code,omitempty"`
	Name            string          `json:"name,omitempty"`
	Value           json.RawMessage `json:"value,omitempty"`
}

// Mapper converts the deltas of one run to AG-UI events. It keeps the open
// text message and thinking block, so it is not safe for concurrent use;
// use one Mapper per run.
type Mapper struct {
	threadID, runID string
	seq             int
	message         string // open text message ID
	lastMessage     string // most recent assistant message ID
	thinking        bool
	thinkingText    bool
	finished        bool
	// parts maps the index of each open part to its kind and, for a tool
	// call, its ID.
	parts map[int]openPart
	link  func(types.Source) string
}

type openPart struct {
	kind types.PartKind
	id   string
	// text accumulates a refusal or an audio transcript.
	text string
}

// Option configures a Mapper.
type Option func(*Mapper)

// WithMediaLink sets how a media source becomes the URL of a media event,
// such as a download route for a saige-artifact:// ref. An empty result
// falls back to the source's URI, then its ref. Without it, the URI or ref
// is used as is.
func WithMediaLink(link func(types.Source) string) Option {
	return func(m *Mapper) { m.link = link }
}

// NewMapper returns a Mapper for one run of a thread. Message IDs are
// derived from runID, so they are stable across a replay of the same run.
func NewMapper(threadID, runID string, opts ...Option) *Mapper {
	m := &Mapper{threadID: threadID, runID: runID}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Start returns the RUN_STARTED event.
func (m *Mapper) Start() Event {
	return Event{Type: RunStarted, ThreadID: m.threadID, RunID: m.runID}
}

// Finished reports whether the run has ended with RUN_FINISHED or RUN_ERROR.
func (m *Mapper) Finished() bool { return m.finished }

func (m *Mapper) nextID() string {
	m.seq++
	return fmt.Sprintf("%s-msg-%d", m.runID, m.seq)
}

// Map returns the events for d. It returns nothing after the run finished.
//
//nolint:gocyclo // one case per delta type
func (m *Mapper) Map(d types.Delta) ([]Event, error) {
	if m.finished {
		return nil, nil
	}
	var out []Event
	switch v := d.(type) {
	case types.PartStart:
		if m.parts == nil {
			m.parts = map[int]openPart{}
		}
		m.parts[v.Index] = openPart{kind: v.Kind, id: v.ID}
		switch v.Kind {
		case types.KindText:
			out = m.closeThinking(out)
			out = m.openText(out)
		case types.KindThinking:
			out = m.closeText(out)
			out = m.openThinking(out)
		case types.KindToolCall:
			out = m.closeText(out)
			out = m.closeThinking(out)
			out = append(out, Event{Type: ToolCallStart, ToolCallID: v.ID, ToolCallName: v.Name, ParentMessageID: m.lastMessage})
		case types.KindRefusal, types.KindImageOut, types.KindAudioOut, types.KindVideoOut:
			// Reported whole when the part ends.
		default:
			return m.custom(d)
		}
	case types.PartDelta:
		p := m.parts[v.Index]
		if p.kind == types.KindRefusal || isMediaOut(p.kind) {
			// Refusal text and transcripts are reported with the part's
			// end; media bytes are reported by link, never streamed.
			p.text += v.Refusal + v.Transcript
			m.parts[v.Index] = p
			return nil, nil
		}
		switch {
		case v.Text != "":
			out = m.closeThinking(out)
			out = m.openText(out)
			out = append(out, Event{Type: TextMessageContent, MessageID: m.message, Delta: v.Text})
		case v.Thinking != "":
			out = m.closeText(out)
			out = m.openThinking(out)
			out = append(out, Event{Type: ThinkingTextMessageContent, Delta: v.Thinking})
		case v.Args != "" && p.kind == types.KindToolCall:
			out = append(out, Event{Type: ToolCallArgs, ToolCallID: p.id, Delta: v.Args})
		case v.Signature != "", v.Text == "" && v.Thinking == "" && v.Args == "" && v.Refusal == "" &&
			len(v.Data) == 0 && v.Transcript == "":
			return nil, nil
		default:
			return m.custom(d)
		}
	case types.PartEnd:
		p, ok := m.parts[v.Index]
		delete(m.parts, v.Index)
		if !ok && v.Part != nil {
			p.kind = v.Part.Kind()
		}
		switch p.kind {
		case types.KindText:
			out = m.closeText(out)
		case types.KindThinking:
			out = m.closeThinking(out)
		case types.KindToolCall:
			out = append(out, Event{Type: ToolCallEnd, ToolCallID: p.id})
		case types.KindRefusal:
			r := Refusal{Index: v.Index, Text: p.text}
			if rp, ok := v.Part.(types.RefusalPart); ok {
				r.Text, r.Category = firstNonEmpty(rp.Text, p.text), rp.Category
			}
			out = m.closeOpen(out)
			ev, err := valueEvent(RefusalEventName, r)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		case types.KindImageOut, types.KindAudioOut, types.KindVideoOut:
			if v.Part == nil {
				return m.custom(d)
			}
			idx := v.Index
			media := m.media(v.Part)
			media.Index = &idx
			if media.Transcript == "" {
				media.Transcript = p.text
			}
			ev, err := valueEvent(MediaEventName, media)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		default:
			return m.custom(d)
		}
	case types.ToolExecEndDelta:
		content := v.Result
		if v.Error != "" {
			content = v.Error
		}
		out = append(out, Event{Type: ToolCallResult, MessageID: m.nextID(), ToolCallID: v.ToolCallID, Content: content, Role: roleTool})
		for _, p := range v.Parts {
			if !types.IsMedia(p) {
				continue
			}
			ev, err := valueEvent(MediaEventName, m.media(p))
			if err != nil {
				return nil, err
			}
			ev.ToolCallID = v.ToolCallID
			out = append(out, ev)
		}
	case types.DoneDelta:
		out = m.closeOpen(out)
		out = append(out, Event{Type: RunFinished, ThreadID: m.threadID, RunID: m.runID})
		m.finished = true
	case types.ErrorDelta:
		out = m.closeOpen(out)
		out = append(out, errorEvent(v.Error))
		m.finished = true
	default:
		return m.custom(d)
	}
	return out, nil
}

func isMediaOut(k types.PartKind) bool {
	return k == types.KindImageOut || k == types.KindAudioOut || k == types.KindVideoOut
}

// media describes a media part for a media event: a link when the source
// has one, else its bytes.
func (m *Mapper) media(p types.Part) Media {
	src, _ := types.SourceOf(p)
	out := Media{Kind: p.Kind(), MediaType: src.MediaType, Filename: src.Filename, Size: src.Size,
		Digest: src.Digest, Unresolved: src.Unresolved}
	if out.Size == 0 {
		out.Size = int64(len(src.Inline))
	}
	if a, ok := p.(types.AudioOutPart); ok {
		out.Transcript = a.Transcript
	}
	if m.link != nil {
		out.URL = m.link(src)
	}
	out.URL = firstNonEmpty(out.URL, src.URI, src.Ref)
	if out.URL == "" {
		out.Data = src.Inline
	}
	return out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// valueEvent returns a CUSTOM event named name whose value is v.
func valueEvent(name string, v any) (Event, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: Custom, Name: name, Value: raw}, nil
}

// custom maps a delta with no AG-UI event of its own to a CUSTOM event.
func (m *Mapper) custom(d types.Delta) ([]Event, error) {
	ev, err := customEvent(d)
	if err != nil {
		return nil, err
	}
	return []Event{ev}, nil
}

// Close ends a run whose stream closed without a done or error delta. It
// closes any open message and returns RUN_ERROR with CodeIncompleteStream.
// After a finished run it returns nothing.
func (m *Mapper) Close() []Event {
	if m.finished {
		return nil
	}
	m.finished = true
	out := m.closeOpen(nil)
	return append(out, Event{Type: RunError, Message: "stream ended before the run finished", Code: CodeIncompleteStream})
}

func (m *Mapper) openText(out []Event) []Event {
	if m.message != "" {
		return out
	}
	m.message = m.nextID()
	m.lastMessage = m.message
	return append(out, Event{Type: TextMessageStart, MessageID: m.message, Role: roleAssistant})
}

func (m *Mapper) closeText(out []Event) []Event {
	if m.message == "" {
		return out
	}
	out = append(out, Event{Type: TextMessageEnd, MessageID: m.message})
	m.message = ""
	return out
}

func (m *Mapper) openThinking(out []Event) []Event {
	if !m.thinking {
		m.thinking = true
		out = append(out, Event{Type: ThinkingStart})
	}
	if !m.thinkingText {
		m.thinkingText = true
		out = append(out, Event{Type: ThinkingTextMessageStart})
	}
	return out
}

func (m *Mapper) closeThinking(out []Event) []Event {
	if m.thinkingText {
		m.thinkingText = false
		out = append(out, Event{Type: ThinkingTextMessageEnd})
	}
	if m.thinking {
		m.thinking = false
		out = append(out, Event{Type: ThinkingEnd})
	}
	return out
}

func (m *Mapper) closeOpen(out []Event) []Event {
	out = m.closeThinking(out)
	return m.closeText(out)
}

func errorEvent(err error) Event {
	ev := Event{Type: RunError, Message: "run failed"}
	if err != nil {
		ev.Message = err.Error()
		if codes := types.ErrorCodes(err); len(codes) > 0 {
			ev.Code = codes[0]
		}
	}
	return ev
}

// customEvent wraps d in its wire envelope. A sub-agent's delta is
// flattened first, so the envelope's path names the tool calls it came
// through.
func customEvent(d types.Delta) (Event, error) {
	path, inner := types.FlattenDelta(d)
	env, err := types.NewDeltaEnvelope(inner)
	if err != nil {
		return Event{}, err
	}
	env.Path = path
	raw, err := json.Marshal(env)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: Custom, Name: CustomEventName, Value: raw}, nil
}

// WriteSSE writes ev as one server-sent event.
func WriteSSE(w io.Writer, ev Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// Stream writes a run as AG-UI server-sent events: RUN_STARTED, the mapped
// deltas, and a terminal RUN_FINISHED or RUN_ERROR. It flushes after each
// delta when w is an http.Flusher. It returns when deltas closes, or with
// ctx's error when ctx ends first; the caller still owns draining deltas.
func Stream(ctx context.Context, w io.Writer, threadID, runID string, deltas <-chan types.Delta) error {
	m := NewMapper(threadID, runID)
	flusher, _ := w.(http.Flusher)
	write := func(events ...Event) error {
		for _, ev := range events {
			if err := WriteSSE(w, ev); err != nil {
				return err
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if err := write(m.Start()); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deltas:
			if !ok {
				return write(m.Close()...)
			}
			events, err := m.Map(d)
			if err != nil {
				return err
			}
			if err := write(events...); err != nil {
				return err
			}
		}
	}
}
