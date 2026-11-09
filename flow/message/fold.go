package message

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
type Folder struct{}

// NewFolder returns an empty folder.
func NewFolder() *Folder { return &Folder{} }

// Add folds one event. A block index sent again, as a resumed stream does
// from the start of the block it was in, replaces the block.
func (f *Folder) Add(e Event) {}

// Message is what has been folded, with lanes down to depth: 0 is the root
// alone, and a negative depth every depth.
func (f *Folder) Message(depth int) Message { return Message{} }

// Fold folds a whole stream.
func Fold(events []Event, depth int) Message {
	f := NewFolder()
	for _, e := range events {
		f.Add(e)
	}
	return f.Message(depth)
}
