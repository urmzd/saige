package agent

import (
	"context"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// Resolution holds the consumer's decision for a marked tool call.
type Resolution struct {
	Approved     bool
	ModifiedArgs map[string]any // nil = use original args
	Message      string         // optional reason (shown to LLM on rejection)
}

// EventStream is the consumer handle for streaming agent deltas.
type EventStream struct {
	deltas      chan types.Delta
	done        chan struct{}
	err         error
	cancel      context.CancelFunc
	once        sync.Once
	ctx         context.Context
	resMu       sync.Mutex
	resolutions map[string]chan Resolution
}

func newEventStream(ctx context.Context, cancel context.CancelFunc) *EventStream {
	return &EventStream{
		deltas: make(chan types.Delta, 128),
		done:   make(chan struct{}),
		cancel: cancel,
		ctx:    ctx,
	}
}

// Deltas returns a channel that yields deltas. Closed on completion.
func (s *EventStream) Deltas() <-chan types.Delta {
	return s.deltas
}

// Wait blocks until the stream is done and returns any error.
func (s *EventStream) Wait() error {
	<-s.done
	return s.err
}

// Cancel stops the stream.
func (s *EventStream) Cancel() {
	s.once.Do(func() {
		s.cancel()
	})
}

func (s *EventStream) send(d types.Delta) {
	select {
	case s.deltas <- d:
	case <-s.ctx.Done():
	}
}

func (s *EventStream) close(err error) {
	s.err = err
	close(s.deltas)
	close(s.done)
}

// ResolveMarker provides the consumer's decision for a marked tool call.
// Call this in response to a MarkerDelta to unblock tool execution.
func (s *EventStream) ResolveMarker(toolCallID string, approved bool, modifiedArgs map[string]any) {
	s.resMu.Lock()
	ch, ok := s.resolutions[toolCallID]
	s.resMu.Unlock()
	if ok {
		ch <- Resolution{Approved: approved, ModifiedArgs: modifiedArgs}
	}
}

// ResolveMarkerWithMessage provides the consumer's decision with an optional message.
func (s *EventStream) ResolveMarkerWithMessage(toolCallID string, approved bool, modifiedArgs map[string]any, message string) {
	s.resMu.Lock()
	ch, ok := s.resolutions[toolCallID]
	s.resMu.Unlock()
	if ok {
		ch <- Resolution{Approved: approved, ModifiedArgs: modifiedArgs, Message: message}
	}
}

// awaitResolution creates a resolution channel for a tool call.
func (s *EventStream) awaitResolution(toolCallID string) <-chan Resolution {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	ch := make(chan Resolution, 1)
	if s.resolutions == nil {
		s.resolutions = make(map[string]chan Resolution)
	}
	s.resolutions[toolCallID] = ch
	return ch
}

// ── Replay ──────────────────────────────────────────────────────────

// Replay converts stored messages into a stream of deltas, enabling
// session restoration. Clients receive the same delta types as if the
// conversation happened live. Only assistant messages and tool results
// produce deltas — system and user text messages are context, not events.
func Replay(messages []types.Message) *EventStream {
	ctx, cancel := context.WithCancel(context.Background())
	stream := newEventStream(ctx, cancel)

	go func() {
		defer func() {
			stream.send(types.DoneDelta{})
			stream.close(nil)
		}()

		for _, msg := range messages {
			switch v := msg.(type) {
			case types.AssistantMessage:
				for _, c := range v.Content {
					switch bc := c.(type) {
					case types.TextContent:
						stream.send(types.TextStartDelta{})
						stream.send(types.TextContentDelta{Content: bc.Text})
						stream.send(types.TextEndDelta{})
					case types.ToolUseContent:
						stream.send(types.ToolCallStartDelta{ID: bc.ID, Name: bc.Name})
						stream.send(types.ToolCallEndDelta{Arguments: bc.Arguments})
					}
				}
			case types.SystemMessage:
				replayToolResults(stream, v.Content)
			case types.UserMessage:
				replayUserToolResults(stream, v.Content)
			}
		}
	}()

	return stream
}

func replayToolResults(stream *EventStream, content []types.SystemContent) {
	for _, c := range content {
		if tr, ok := c.(types.ToolResultContent); ok {
			stream.send(types.ToolExecStartDelta{ToolCallID: tr.ToolCallID})
			stream.send(types.ToolExecEndDelta{ToolCallID: tr.ToolCallID, Result: tr.Text})
		}
	}
}

func replayUserToolResults(stream *EventStream, content []types.UserContent) {
	for _, c := range content {
		switch v := c.(type) {
		case types.ToolResultContent:
			stream.send(types.ToolExecStartDelta{ToolCallID: v.ToolCallID})
			stream.send(types.ToolExecEndDelta{ToolCallID: v.ToolCallID, Result: v.Text})
		case types.FeedbackContent:
			stream.send(types.FeedbackDelta(v))
		}
	}
}
