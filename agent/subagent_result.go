package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// SubAgentResult is a detached record of one delegation. Trace uses tree.Print's
// native JSON document. It includes every branch and message, including tool
// calls and results. Failed calls retain their partial trace but have no Output.
type SubAgentResult struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Task        string          `json:"task"`
	Branch      types.BranchID  `json:"branch"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt time.Time       `json:"completed_at"`
	Trace       json.RawMessage `json:"trace"`
	Output      string          `json:"output"`
	Error       string          `json:"error,omitempty"`
}

// Tree restores an independent tree. Editing it cannot change the child or
// another reader's result. A trace is data, not permission to resume its tools.
func (r SubAgentResult) Tree() (*tree.Tree, error) {
	var tr tree.Tree
	if err := json.Unmarshal(r.Trace, &tr); err != nil {
		return nil, err
	}
	return &tr, nil
}

// Messages returns the selected branch's visible messages. Use Tree to inspect
// archived nodes, other branches, timestamps, or individual node IDs.
func (r SubAgentResult) Messages() ([]types.Message, error) {
	tr, err := r.Tree()
	if err != nil {
		return nil, err
	}
	return tr.FlattenBranch(r.Branch)
}

// Node reads any native node record by ID, including archived or other-branch
// messages. The returned JSON includes timestamps and is independently owned.
func (r SubAgentResult) Node(id types.NodeID) (json.RawMessage, error) {
	var document struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(r.Trace, &document); err != nil {
		return nil, err
	}
	for _, raw := range document.Content {
		var node struct {
			ID types.NodeID `json:"id"`
		}
		if err := json.Unmarshal(raw, &node); err != nil {
			return nil, err
		}
		if node.ID == id {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("subagent node %q not found", id)
}

// FinalAssistant returns the terminal assistant message. It rejects failed
// calls and tool-call turns so partial progress cannot be mistaken for success.
func (r SubAgentResult) FinalAssistant() (types.AssistantMessage, error) {
	if r.Error != "" {
		return types.AssistantMessage{}, errors.New(r.Error)
	}
	tr, err := r.Tree()
	if err != nil {
		return types.AssistantMessage{}, err
	}
	tip, err := tr.Tip(r.Branch)
	if err != nil {
		return types.AssistantMessage{}, err
	}
	msg, ok := tip.Message.(types.AssistantMessage)
	if !ok || len(assistantToolCalls(&msg)) != 0 {
		return types.AssistantMessage{}, errors.New("subagent has no terminal assistant message")
	}
	return msg, nil
}

// SubAgentResultPolicy selects the data returned to the parent as a tool
// result. It can validate structured JSON, select messages, or return a stored
// artifact reference. It must not treat child text as trusted instructions.
type SubAgentResultPolicy interface {
	Select(SubAgentResult) (string, error)
}

// SubAgentResultFunc adapts a function to a result policy.
type SubAgentResultFunc func(SubAgentResult) (string, error)

func (f SubAgentResultFunc) Select(r SubAgentResult) (string, error) { return f(r) }

// FinalAssistantText is the default result policy. Intermediate assistant text
// and nested delegation output are excluded.
type FinalAssistantText struct{}

func (FinalAssistantText) Select(r SubAgentResult) (string, error) {
	msg, err := r.FinalAssistant()
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if content, ok := block.(types.TextContent); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String(), nil
}

// SubAgentResultSink retains a completed result for an upstream service. Save
// must finish before the parent receives success. A save error fails delegation.
// Implementations must support concurrent calls and enforce tenant scope,
// retention, and size limits. This interface does not promise crash recovery.
type SubAgentResultSink interface {
	Save(context.Context, SubAgentResult) error
}

type subAgentCapture struct {
	result SubAgentResult
	policy SubAgentResultPolicy
	sink   SubAgentResultSink
}

func (a *Agent) captureSubAgent(ctx context.Context, stream *EventStream, runErr error) error {
	capture := stream.capture
	if capture == nil {
		return runErr
	}
	r := capture.result
	r.CompletedAt = time.Now().UTC()
	r.Branch = stream.branch
	var trace bytes.Buffer
	if err := tree.Print(&trace, a.cfg.Tree); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("subagent trace: %w", err))
	} else {
		r.Trace = append(json.RawMessage(nil), trace.Bytes()...)
	}
	if runErr != nil {
		r.Error = runErr.Error()
	} else {
		policy := capture.policy
		if policy == nil {
			policy = FinalAssistantText{}
		}
		// A policy gets its own bytes. It cannot corrupt the retained trace.
		selected, err := policy.Select(cloneSubAgentResult(r))
		if err != nil {
			runErr = fmt.Errorf("subagent result policy: %w", err)
			r.Error = runErr.Error()
		} else {
			r.Output = selected
		}
	}
	if capture.sink != nil {
		if err := capture.sink.Save(ctx, cloneSubAgentResult(r)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("save subagent result: %w", err))
			r.Error = runErr.Error()
			r.Output = ""
		}
	}
	stream.result = &r
	return runErr
}

func cloneSubAgentResult(r SubAgentResult) SubAgentResult {
	r.Trace = append(json.RawMessage(nil), r.Trace...)
	return r
}

// SubAgentResult waits for completion and returns a detached record, including
// a partial trace on failure. Drain Deltas concurrently before calling this,
// as with Wait. Ordinary Invoke and Replay streams have no subagent result.
func (s *EventStream) SubAgentResult() (SubAgentResult, error) {
	err := s.Wait()
	if s.result == nil {
		return SubAgentResult{}, errors.Join(err, errors.New("stream is not a subagent invocation"))
	}
	return cloneSubAgentResult(*s.result), err
}

// InvokeSubAgent lets a service invoke a configured child without asking the
// parent model to call a tool. The returned stream supports SubAgentResult.
// No parent conversation messages are appended by this operation.
func (a *Agent) InvokeSubAgent(ctx context.Context, name, task string) (*EventStream, error) {
	tool, ok := a.tools.Get("delegate_to_" + name)
	if !ok {
		return nil, fmt.Errorf("unknown subagent %q", name)
	}
	child, ok := tool.(*subAgentTool)
	if !ok {
		return nil, fmt.Errorf("tool for %q is not a registered subagent", name)
	}
	id := types.NewID()
	return child.invokeWithRunner(ctx, task, a.childStepRunner(id), id), nil
}
