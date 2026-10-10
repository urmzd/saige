package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// SubAgentContext selects which parent messages a sub-agent starts with.
type SubAgentContext int

const (
	// ContextTaskOnly starts the child with the task alone. It is the
	// default and keeps the child's context small and free of the parent's
	// reasoning.
	ContextTaskOnly SubAgentContext = iota
	// ContextFork copies the parent's branch, up to the turn that delegated,
	// into the child's tree before the task. The child keeps its own system
	// prompt. Thinking blocks are dropped, because their signatures belong
	// to the provider that produced them.
	ContextFork
	// ContextFiltered copies the messages SubAgentDef.ContextFilter selects
	// from the same history ContextFork would copy.
	ContextFiltered
)

// String returns the mode's name.
func (c SubAgentContext) String() string {
	switch c {
	case ContextTaskOnly:
		return "task_only"
	case ContextFork:
		return "fork"
	case ContextFiltered:
		return "filtered"
	default:
		return fmt.Sprintf("SubAgentContext(%d)", int(c))
	}
}

// MessageSelector chooses the parent messages a ContextFiltered sub-agent
// starts with. It receives a copy it may reorder or truncate, and must keep
// every tool result with the assistant turn that requested it.
type MessageSelector interface {
	SelectMessages(ctx context.Context, history []types.Message) ([]types.Message, error)
}

// MessageSelectorFunc adapts a function to MessageSelector.
type MessageSelectorFunc func(ctx context.Context, history []types.Message) ([]types.Message, error)

// SelectMessages implements MessageSelector.
func (f MessageSelectorFunc) SelectMessages(ctx context.Context, history []types.Message) ([]types.Message, error) {
	return f(ctx, history)
}

// TextMessagesOnly keeps the text of user and assistant messages and drops
// tool calls, tool results, files, and system notes. The child sees what was
// said, not the tool traffic, so no tool pairing can break.
type TextMessagesOnly struct{}

// SelectMessages implements MessageSelector.
func (TextMessagesOnly) SelectMessages(_ context.Context, history []types.Message) ([]types.Message, error) {
	var out []types.Message
	for _, m := range history {
		switch v := m.(type) {
		case types.UserMessage:
			var content []types.UserPart
			for _, c := range v.Parts {
				if t, ok := c.(types.TextPart); ok {
					content = append(content, t)
				}
			}
			if len(content) > 0 {
				out = append(out, types.UserMessage{Parts: content})
			}
		case types.AssistantMessage:
			var content []types.AssistantPart
			for _, c := range v.Parts {
				if t, ok := c.(types.TextPart); ok {
					content = append(content, t)
				}
			}
			if len(content) > 0 {
				out = append(out, types.AssistantMessage{Parts: content})
			}
		}
	}
	return out, nil
}

// ErrAncestorDelegation refuses a delegation to an agent that is already on
// the call path, which would let two agents call each other without end.
var ErrAncestorDelegation = errors.New("delegation to an ancestor refused")

// callFrame places a run in a delegation tree. A parent attaches its child's
// frame to the child's context; a root run has the zero frame.
type callFrame struct {
	runID   string   // the root run's ID
	agents  []string // agent names from the root run down to the caller
	callIDs []string // tool call IDs from the root run down to this run
}

type callFrameKey struct{}

func frameFrom(ctx context.Context) callFrame {
	f, _ := ctx.Value(callFrameKey{}).(callFrame)
	return f
}

func withFrame(ctx context.Context, f callFrame) context.Context {
	return context.WithValue(ctx, callFrameKey{}, f)
}

// childFrame is the frame of a child that this run starts with callID.
func (a *Agent) childFrame(ctx context.Context, stream *EventStream, callID string) callFrame {
	return callFrame{
		runID:   stream.runID,
		agents:  append(slices.Clone(frameFrom(ctx).agents), a.cfg.Name),
		callIDs: append(slices.Clone(stream.path), callID),
	}
}

// checkAncestor refuses a delegation to name when name is this agent or one
// of its callers.
func (a *Agent) checkAncestor(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	if name == a.cfg.Name || slices.Contains(frameFrom(ctx).agents, name) {
		return fmt.Errorf("%w: %s is already on the call path", ErrAncestorDelegation, name)
	}
	return nil
}

// callerBlock tells a child where it sits and what its caller receives.
func callerBlock(f callFrame, child string) string {
	path := append(slices.Clone(f.agents), child)
	var b strings.Builder
	b.WriteString("<caller>\n")
	fmt.Fprintf(&b, "You are a sub-agent. Call path: %s (depth %d).\n", strings.Join(nonEmpty(path), " > "), len(f.agents))
	b.WriteString("Your final message is the entire result your caller receives. It does not see your other messages or tool results, so put everything it needs in that message.\n")
	if ancestors := nonEmpty(f.agents); len(ancestors) > 0 {
		fmt.Fprintf(&b, "Do not delegate back to %s.\n", strings.Join(ancestors, ", "))
	}
	b.WriteString("</caller>")
	return b.String()
}

func nonEmpty(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// seedMessage is a child's first user message: the caller block, unless the
// definition omits it, then the task.
func seedMessage(f callFrame, child, task string, omitCaller bool) types.UserMessage {
	if omitCaller {
		return types.UserMsg(types.Text(task))
	}
	return types.UserMessage{Parts: []types.UserPart{
		types.TextPart{Text: callerBlock(f, child)},
		types.TextPart{Text: task},
	}}
}

// parentHistory returns the parent's branch as a child may receive it: no
// root system message, no metadata, no thinking, and nothing from the turn
// that is delegating, whose tool calls have no results yet.
func (a *Agent) parentHistory(stream *EventStream) ([]types.Message, error) {
	messages, err := a.cfg.Tree.FlattenBranch(stream.branch)
	if err != nil {
		return nil, err
	}
	_, messages = a.prepareMessages(messages)
	if len(messages) > 0 && messages[0].Role() == types.RoleSystem {
		messages = messages[1:]
	}
	if n := len(messages); n > 0 {
		if am, ok := messages[n-1].(types.AssistantMessage); ok && len(assistantToolCalls(&am)) > 0 {
			messages = messages[:n-1]
		}
	}
	out := make([]types.Message, 0, len(messages))
	for _, m := range messages {
		if am, ok := m.(types.AssistantMessage); ok {
			content := make([]types.AssistantPart, 0, len(am.Parts))
			for _, c := range am.Parts {
				if _, thinking := c.(types.ThinkingPart); !thinking {
					content = append(content, c)
				}
			}
			if len(content) == 0 {
				continue
			}
			m = types.AssistantMessage{Parts: content}
		}
		out = append(out, m)
	}
	return out, nil
}

// childHistory returns the parent messages a child starts with under mode.
func (a *Agent) childHistory(ctx context.Context, stream *EventStream, mode SubAgentContext, filter MessageSelector) ([]types.Message, error) {
	switch mode {
	case ContextTaskOnly:
		return nil, nil
	case ContextFork, ContextFiltered:
	default:
		return nil, fmt.Errorf("unknown sub-agent context mode %s", mode)
	}
	if mode == ContextFiltered && filter == nil {
		return nil, errors.New("filtered sub-agent context needs a ContextFilter")
	}
	history, err := a.parentHistory(stream)
	if err != nil {
		return nil, fmt.Errorf("sub-agent context: %w", err)
	}
	if mode == ContextFork {
		return history, nil
	}
	selected, err := filter.SelectMessages(ctx, history)
	if err != nil {
		return nil, fmt.Errorf("sub-agent context filter: %w", err)
	}
	if err := checkToolPairs(selected); err != nil {
		return nil, fmt.Errorf("sub-agent context filter broke tool pairing: %w", err)
	}
	return selected, nil
}

// checkToolPairs reports a tool result with no earlier tool call, and a tool
// call whose result does not arrive before the next assistant turn or the
// end of msgs. Providers reject either, so a filtered delegation fails here
// with a clear error instead of at the child's first provider call.
func checkToolPairs(msgs []types.Message) error {
	open := map[string]bool{}
	var order []string // open call IDs, in call order
	unanswered := func() error {
		for _, id := range order {
			if open[id] {
				return fmt.Errorf("tool call %s has no result", id)
			}
		}
		return nil
	}
	answer := func(r types.ToolResultPart) error {
		if !open[r.CallID] {
			return fmt.Errorf("tool result %s has no earlier tool call", r.CallID)
		}
		delete(open, r.CallID)
		return nil
	}
	for _, m := range msgs {
		switch v := m.(type) {
		case types.AssistantMessage:
			if err := unanswered(); err != nil {
				return err
			}
			clear(open)
			order = order[:0]
			for _, c := range v.Parts {
				if tu, ok := c.(types.ToolCallPart); ok {
					open[tu.ID] = true
					order = append(order, tu.ID)
				}
			}
		case types.SystemMessage:
			for _, c := range v.Parts {
				if r, ok := c.(types.ToolResultPart); ok {
					if err := answer(r); err != nil {
						return err
					}
				}
			}
		case types.UserMessage:
			for _, c := range v.Parts {
				if r, ok := c.(types.ToolResultPart); ok {
					if err := answer(r); err != nil {
						return err
					}
				}
			}
		}
	}
	return unanswered()
}
