// Package message turns the runs that answer one request into one streamed
// assistant message, and folds that stream back into the message.
//
// A request is answered by one root run. Every run it calls, at any depth,
// is a tool call of its caller: the called run's events carry the root's id
// (event.Envelope.Root), the run that called it (ParentRun) and the call
// (ParentToolCall). A Projector reads the tree's envelopes in total order
// (Less) and writes the stream: at the root, plain Anthropic Messages
// streaming events, so a stock client folds the root's content; under each
// call, the called agent's own events wrapped in Nested, each lane a
// complete message with its own block indices; and Humagen's ToolStatus and
// Progress. A Folder reads the stream back into a Message.
//
// Both are pure: no I/O, no clock, no goroutines. A host projects live from
// the bus and again from a stored log, and gets the same stream, so a live
// view and a replay are the same thing.
package message

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/urmzd/saige/flow/event"
)

// Event types. The first eight are Anthropic's; the rest are Humagen's.
const (
	TypeMessageStart      = "message_start"
	TypeContentBlockStart = "content_block_start"
	TypeContentBlockDelta = "content_block_delta"
	TypeContentBlockStop  = "content_block_stop"
	TypeMessageDelta      = "message_delta"
	TypeMessageStop       = "message_stop"
	TypePing              = "ping"
	TypeError             = "error"
	TypeNested            = "humagen.nested"
	TypeToolStatus        = "humagen.tool_status"
	TypeProgress          = "humagen.progress"
)

// Event is one event of the stream. Its JSON is the wire form, with "type"
// first; Decode reads one back.
type Event interface {
	EventType() string
}

// Head is what the host says about the response, on message_start.
type Head struct {
	// ID is the response's id, the assistant message's.
	ID        string `json:"-"`
	RequestID string `json:"request_id,omitempty"`
	RootRun   string `json:"root_run"`
	Agent     string `json:"agent,omitempty"`
	Ref       string `json:"ref,omitempty"`
	TraceURL  string `json:"trace_url,omitempty"`
}

// MessageStart opens a message: the response at the root, a called agent's
// lane under its call.
type MessageStart struct {
	Message StartedMessage `json:"message"`
}

// StartedMessage is the message as it opens: no content yet.
type StartedMessage struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"` // "message"
	Role    string  `json:"role"` // "assistant"
	Content []Block `json:"content"`
	Humagen *Head   `json:"humagen,omitempty"`
}

// ContentBlockStart opens block Index.
type ContentBlockStart struct {
	Index        int   `json:"index"`
	ContentBlock Block `json:"content_block"`
}

// ContentBlockDelta adds to open block Index.
type ContentBlockDelta struct {
	Index int        `json:"index"`
	Delta BlockDelta `json:"delta"`
}

// ContentBlockStop closes block Index.
type ContentBlockStop struct {
	Index int `json:"index"`
}

// MessageDelta ends a message's content: why it stopped, and what it used.
type MessageDelta struct {
	Delta   StopDelta  `json:"delta"`
	Usage   *Usage     `json:"usage,omitempty"`
	Humagen *TreeUsage `json:"humagen,omitempty"`
}

// StopDelta is why a message stopped.
type StopDelta struct {
	StopReason string `json:"stop_reason"`
}

// Stop reasons.
const (
	StopEndTurn       = "end_turn"
	StopMaxTokens     = "max_tokens"
	StopRefusal       = "refusal"
	StopContextWindow = "model_context_window_exceeded"
	StopStopped       = "stopped"
	StopFailed        = "failed"
)

// MessageStop ends a message. At the root it is sent only once every call
// that is not in the background has ended.
type MessageStop struct{}

// Ping keeps a connection open.
type Ping struct{}

// Error reports a stream that cannot go on.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is an Error's detail.
type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Nested is an event of a called agent's lane: Event as the agent's own
// stream has it, under the call ParentToolUseID, Depth calls below the root.
type Nested struct {
	ParentToolUseID string `json:"parent_tool_use_id"`
	AgentRunID      string `json:"agent_run_id"`
	Depth           int    `json:"depth"`
	Event           Event  `json:"event"`
}

// Tool call statuses.
const (
	StatusQueued           = "queued"
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval"
	StatusAwaitingInput    = "awaiting_input"
	StatusDone             = "done"
	StatusError            = "error"
	StatusDenied           = "denied"
	StatusStopped          = "stopped"
)

// ToolStatus is a tool call's state, at any depth. Path is the call ids from
// the root's call down to ToolUseID. A call awaiting a person, or one that
// failed, is also reported at the root on its depth-one call, with the same
// Path, so it is never hidden below a collapsed lane.
type ToolStatus struct {
	ToolUseID string    `json:"tool_use_id"`
	Path      []string  `json:"path"`
	Status    string    `json:"status"`
	Title     string    `json:"title,omitempty"`
	Question  *Question `json:"question,omitempty"`
	ElapsedMs int64     `json:"elapsed_ms"`
}

// Question is what a call awaits from a person.
type Question struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
	Who     []string `json:"who,omitempty"`
}

// Progress is a transient status line for a lane, or for the response when
// ParentToolUseID is empty.
type Progress struct {
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
	Text            string `json:"text"`
}

// Block types.
const (
	BlockText             = "text"
	BlockThinking         = "thinking"
	BlockServerToolUse    = "server_tool_use"
	BlockServerToolResult = "server_tool_result"
)

// Tool kinds, on a server_tool_use block.
const (
	KindAgent    = "agent"
	KindTool     = "tool"
	KindContext  = "context"
	KindApproval = "approval"
	KindProvider = "provider"
)

// Block is one content block. Type says which fields it uses.
type Block struct {
	Type string `json:"type"`
	// text
	Text      string     `json:"text,omitempty"`
	Citations []Citation `json:"citations,omitempty"`
	// thinking: a summary only; signatures never leave the runtime.
	Thinking string `json:"thinking,omitempty"`
	// server_tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Kind  string          `json:"kind,omitempty"`
	Title string          `json:"title,omitempty"`
	// server_tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []ResultContent `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ResultContent is one item of a tool result: text, or a search_result built
// from the cited sources of the agent a call ran, so they survive the call.
type ResultContent struct {
	Type      string     `json:"type"` // "text" or "search_result"
	Text      string     `json:"text,omitempty"`
	Source    string     `json:"source,omitempty"`
	Title     string     `json:"title,omitempty"`
	Content   []TextItem `json:"content,omitempty"`
	Citations *Enabled   `json:"citations,omitempty"`
}

// TextItem is a text item inside a search_result.
type TextItem struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Enabled is Anthropic's {"enabled": true}.
type Enabled struct {
	Enabled bool `json:"enabled"`
}

// Citation is a text block's citation of a source.
type Citation struct {
	Type      string `json:"type"` // "search_result_location"
	Source    string `json:"source,omitempty"`
	Title     string `json:"title,omitempty"`
	CitedText string `json:"cited_text,omitempty"`
	Index     int    `json:"search_result_index"`
}

// BlockDelta is what a content_block_delta adds.
type BlockDelta struct {
	Type        string    `json:"type"` // text_delta, thinking_delta, input_json_delta, citations_delta
	Text        string    `json:"text,omitempty"`
	Thinking    string    `json:"thinking,omitempty"`
	PartialJSON string    `json:"partial_json,omitempty"`
	Citation    *Citation `json:"citation,omitempty"`
}

// Usage is one run's own tokens, Anthropic shaped.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// TreeUsage is what the whole tree used: by model, and its cost.
type TreeUsage struct {
	Usage ModelUsage `json:"usage"`
}

// ModelUsage is usage by model, and the total cost in US dollars.
type ModelUsage struct {
	ByModel map[string]Usage `json:"by_model"`
	CostUSD float64          `json:"cost_usd"`
}

// EventType implements Event.
func (MessageStart) EventType() string { return TypeMessageStart }

// EventType implements Event.
func (ContentBlockStart) EventType() string { return TypeContentBlockStart }

// EventType implements Event.
func (ContentBlockDelta) EventType() string { return TypeContentBlockDelta }

// EventType implements Event.
func (ContentBlockStop) EventType() string { return TypeContentBlockStop }

// EventType implements Event.
func (MessageDelta) EventType() string { return TypeMessageDelta }

// EventType implements Event.
func (MessageStop) EventType() string { return TypeMessageStop }

// EventType implements Event.
func (Ping) EventType() string { return TypePing }

// EventType implements Event.
func (Error) EventType() string { return TypeError }

// EventType implements Event.
func (Nested) EventType() string { return TypeNested }

// EventType implements Event.
func (ToolStatus) EventType() string { return TypeToolStatus }

// EventType implements Event.
func (Progress) EventType() string { return TypeProgress }

// Less is the stream's total order of envelopes: time, then run, then the
// run's sequence.
func Less(a, b event.Envelope) bool {
	switch {
	case !a.Time.Equal(b.Time):
		return a.Time.Before(b.Time)
	case a.Run != b.Run:
		return a.Run < b.Run
	}
	return a.Seq < b.Seq
}

// Cursor is how far a stream has been read: each run's last sequence. A
// host encodes it as the opaque Last-Event-ID.
type Cursor map[string]uint64

// ToolUseID is the id the API gives a run's n-th tool call, counting from
// one in the order the run asked for them: htu_<run>_<n>. Provider call ids
// never leave the runtime.
func ToolUseID(run string, n int) string { return fmt.Sprintf("htu_%s_%d", run, n) }

// Within reports whether e is shown to a reader who asked for depth: 0 is
// the root alone, a positive depth that many calls down, and a negative one
// every depth.
func Within(e Event, depth int) bool {
	n, ok := e.(Nested)
	return !ok || depth < 0 || n.Depth <= depth
}

// Decode reads one event from its wire form.
func Decode(raw json.RawMessage) (Event, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	var e Event
	switch head.Type {
	case TypeMessageStart:
		e = &MessageStart{}
	case TypeContentBlockStart:
		e = &ContentBlockStart{}
	case TypeContentBlockDelta:
		e = &ContentBlockDelta{}
	case TypeContentBlockStop:
		e = &ContentBlockStop{}
	case TypeMessageDelta:
		e = &MessageDelta{}
	case TypeMessageStop:
		return MessageStop{}, nil
	case TypePing:
		return Ping{}, nil
	case TypeError:
		e = &Error{}
	case TypeNested:
		e = &Nested{}
	case TypeToolStatus:
		e = &ToolStatus{}
	case TypeProgress:
		e = &Progress{}
	default:
		return nil, fmt.Errorf("message: unknown event type %q", head.Type)
	}
	if err := json.Unmarshal(raw, e); err != nil {
		return nil, err
	}
	return reflect.ValueOf(e).Elem().Interface().(Event), nil
}

// MarshalEvent is an event's wire form: its fields, with "type" first.
func MarshalEvent(e Event) ([]byte, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	typ, _ := json.Marshal(e.EventType())
	if string(body) == "{}" {
		return []byte(`{"type":` + string(typ) + `}`), nil
	}
	return []byte(`{"type":` + string(typ) + `,` + string(body[1:])), nil
}

// MarshalJSON writes the nested event in its wire form.
func (n Nested) MarshalJSON() ([]byte, error) {
	inner, err := MarshalEvent(n.Event)
	if err != nil {
		return nil, err
	}
	type plain struct {
		ParentToolUseID string          `json:"parent_tool_use_id"`
		AgentRunID      string          `json:"agent_run_id"`
		Depth           int             `json:"depth"`
		Event           json.RawMessage `json:"event"`
	}
	return json.Marshal(plain{n.ParentToolUseID, n.AgentRunID, n.Depth, inner})
}

// UnmarshalJSON reads the nested event from its wire form.
func (n *Nested) UnmarshalJSON(b []byte) error {
	var plain struct {
		ParentToolUseID string          `json:"parent_tool_use_id"`
		AgentRunID      string          `json:"agent_run_id"`
		Depth           int             `json:"depth"`
		Event           json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(b, &plain); err != nil {
		return err
	}
	inner, err := Decode(plain.Event)
	if err != nil {
		return err
	}
	*n = Nested{ParentToolUseID: plain.ParentToolUseID, AgentRunID: plain.AgentRunID, Depth: plain.Depth, Event: inner}
	return nil
}

// MarshalJSON writes a block as Anthropic's API does: only the fields its
// type has, with an empty text or thinking written out, so a client that
// appends each delta to the field starts from "", and a tool call's input
// written as {} until it has one.
func (b Block) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case BlockText:
		return json.Marshal(struct {
			Type      string     `json:"type"`
			Text      string     `json:"text"`
			Citations []Citation `json:"citations,omitempty"`
		}{b.Type, b.Text, b.Citations})
	case BlockThinking:
		return json.Marshal(struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}{b.Type, b.Thinking})
	case BlockServerToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Kind  string          `json:"kind,omitempty"`
			Title string          `json:"title,omitempty"`
		}{b.Type, b.ID, b.Name, input, b.Kind, b.Title})
	case BlockServerToolResult:
		content := b.Content
		if content == nil {
			content = []ResultContent{}
		}
		return json.Marshal(struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   []ResultContent `json:"content"`
			IsError   bool            `json:"is_error,omitempty"`
		}{b.Type, b.ToolUseID, content, b.IsError})
	}
	type plain Block
	return json.Marshal(plain(b))
}
