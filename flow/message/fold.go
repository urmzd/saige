package message

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Message is a response as its stream folds: the root's content, and under
// each agent call the called agent's lane, down to the depth asked for.
type Message struct {
	ID         string     `json:"id"`
	Head       *Head      `json:"humagen,omitempty"`
	Content    []Content  `json:"content"`
	StopReason string     `json:"stop_reason,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	TreeUsage  *TreeUsage `json:"tree_usage,omitempty"`
	// Done is true once message_stop was read.
	Done bool `json:"done"`
}

// Content is one block of a lane, with what the stream said about it: a
// tool call's latest status, and for an agent call the called agent's lane.
type Content struct {
	Block
	Status *ToolStatus `json:"status,omitempty"`
	Lane   *Lane       `json:"lane,omitempty"`
}

// Lane is a called agent's message.
type Lane struct {
	AgentRunID string    `json:"agent_run_id"`
	Depth      int       `json:"depth"`
	Content    []Content `json:"content"`
	StopReason string    `json:"stop_reason,omitempty"`
	Done       bool      `json:"done"`
}

// Folder reads a stream into a Message. It is not safe for concurrent use.
type Folder struct {
	msg  Message
	root *folded
	// lanes are the called agents' messages, by the call they answer.
	lanes map[string]*folded
	// status is each call's latest status, by its id.
	status map[string]ToolStatus
}

// folded is one message as it folds: the root's, or a lane's.
type folded struct {
	lane    Lane
	content []foldedBlock
}

// A foldedBlock owns the fragments for one index. Builders are pointers so
// growing the content slice never copies a live strings.Builder.
type foldedBlock struct {
	Block
	text, thinking *strings.Builder
	input          json.RawMessage
}

// NewFolder returns an empty folder.
func NewFolder() *Folder {
	return &Folder{root: &folded{}, lanes: map[string]*folded{}, status: map[string]ToolStatus{}}
}

// Add folds one event. A block index sent again, as a resumed stream does
// from the start of the block it was in, replaces the block.
func (f *Folder) Add(e Event) {
	switch v := valueOf(e).(type) {
	case Nested:
		f.nested(v)
	case ToolStatus:
		f.status[v.ToolUseID] = v
	case MessageStart:
		f.msg.ID = v.Message.ID
		if v.Message.Humagen != nil {
			h := *v.Message.Humagen
			h.ID = v.Message.ID
			f.msg.Head = &h
		}
	case MessageDelta:
		f.msg.StopReason = v.Delta.StopReason
		f.msg.Usage = v.Usage
		f.msg.TreeUsage = v.Humagen
	case MessageStop:
		f.msg.Done = true
	default:
		f.root.add(v)
	}
}

func (f *Folder) nested(n Nested) {
	l := f.lanes[n.ParentToolUseID]
	if l == nil || l.lane.AgentRunID != n.AgentRunID {
		l = &folded{}
		f.lanes[n.ParentToolUseID] = l
	}
	l.lane.AgentRunID, l.lane.Depth = n.AgentRunID, n.Depth
	switch v := valueOf(n.Event).(type) {
	case Nested:
		f.nested(v)
	case ToolStatus:
		f.status[v.ToolUseID] = v
	case MessageDelta:
		l.lane.StopReason = v.Delta.StopReason
	case MessageStop:
		l.lane.Done = true
	default:
		l.add(v)
	}
}

// add folds one block event into the message.
func (m *folded) add(e Event) {
	switch v := e.(type) {
	case ContentBlockStart:
		if v.Index < 0 {
			return
		}
		for len(m.content) <= v.Index {
			m.content = append(m.content, foldedBlock{})
		}
		block := v.ContentBlock
		block.Citations = slices.Clip(block.Citations)
		m.content[v.Index] = foldedBlock{Block: block}
	case ContentBlockDelta:
		if v.Index < 0 || v.Index >= len(m.content) {
			return
		}
		m.content[v.Index].delta(v.Delta)
	case ContentBlockStop:
		if v.Index < 0 || v.Index >= len(m.content) {
			return
		}
		b := &m.content[v.Index]
		if b.input != nil {
			b.Input = b.input
			b.input = nil
		}
		b.text, b.thinking = nil, nil
	}
}

func (b *foldedBlock) delta(d BlockDelta) {
	switch d.Type {
	case deltaText:
		b.Text = appendFragment(&b.text, b.Text, d.Text)
	case deltaThinking:
		b.Thinking = appendFragment(&b.thinking, b.Thinking, d.Thinking)
	case deltaInputJSON:
		// A present, empty fragment replaces the initial input at stop.
		if b.input == nil {
			b.input = json.RawMessage{}
		}
		b.input = append(b.input, d.PartialJSON...)
	case deltaCitations:
		if d.Citation != nil {
			b.Citations = append(b.Citations, *d.Citation)
		}
	}
}

// A single fragment is already immutable. For subsequent fragments, append
// into a growing buffer; earlier Message snapshots keep their original prefix.
func appendFragment(buf **strings.Builder, current, fragment string) string {
	if fragment == "" {
		return current
	}
	if current == "" {
		return fragment
	}
	if *buf == nil {
		*buf = new(strings.Builder)
		(*buf).Grow(len(current) + len(fragment))
		(*buf).WriteString(current)
	}
	(*buf).WriteString(fragment)
	return (*buf).String()
}

// Message is what has been folded, with lanes down to depth: 0 is the root
// alone, and a negative depth every depth.
func (f *Folder) Message(depth int) Message {
	m := f.msg
	if m.Head != nil {
		h := *m.Head
		m.Head = &h
	}
	if m.Usage != nil {
		u := *m.Usage
		m.Usage = &u
	}
	if m.TreeUsage != nil {
		t := TreeUsage{Usage: ModelUsage{ByModel: maps.Clone(m.TreeUsage.Usage.ByModel), CostUSD: m.TreeUsage.Usage.CostUSD}}
		m.TreeUsage = &t
	}
	m.Content = f.content(f.root.content, depth, map[string]bool{})
	return m
}

// content copies a message's blocks, with each call's status and the lanes
// under its agent calls down to depth.
func (f *Folder) content(in []foldedBlock, depth int, seen map[string]bool) []Content {
	out := make([]Content, len(in))
	for i, block := range in {
		c := Content{Block: block.Block}
		c.Citations = slices.Clone(c.Citations)
		c.Input = slices.Clone(c.Input)
		c.Content = cloneResult(c.Content)
		if c.Type == BlockServerToolUse && c.ID != "" && !seen[c.ID] {
			if s, ok := f.status[c.ID]; ok {
				s.Path = slices.Clone(s.Path)
				if s.Question != nil {
					q := *s.Question
					s.Question = &q
				}
				c.Status = &s
			}
			if l := f.lanes[c.ID]; l != nil && (depth < 0 || l.lane.Depth <= depth) {
				seen[c.ID] = true
				lane := l.lane
				lane.Content = f.content(l.content, depth, seen)
				c.Lane = &lane
			}
		}
		out[i] = c
	}
	return out
}

func cloneResult(in []ResultContent) []ResultContent {
	if in == nil {
		return nil
	}
	out := make([]ResultContent, len(in))
	for i, r := range in {
		r.Content = slices.Clone(r.Content)
		if r.Citations != nil {
			c := *r.Citations
			r.Citations = &c
		}
		out[i] = r
	}
	return out
}

// valueOf returns an event by value, as Decode does.
func valueOf(e Event) Event {
	v := reflect.ValueOf(e)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		if out, ok := v.Elem().Interface().(Event); ok {
			return out
		}
	}
	return e
}

// Fold folds a whole stream.
func Fold(events []Event, depth int) Message {
	f := NewFolder()
	for _, e := range events {
		f.Add(e)
	}
	return f.Message(depth)
}

// MarshalJSON writes the block as Block does, with the call's status and
// lane beside its fields.
func (c Content) MarshalJSON() ([]byte, error) {
	block, err := json.Marshal(c.Block)
	if err != nil {
		return nil, err
	}
	extra, err := json.Marshal(struct {
		Status *ToolStatus `json:"status,omitempty"`
		Lane   *Lane       `json:"lane,omitempty"`
	}{c.Status, c.Lane})
	if err != nil {
		return nil, err
	}
	if string(extra) == "{}" {
		return block, nil
	}
	return append(append(block[:len(block)-1:len(block)-1], ','), extra[1:]...), nil
}
