package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
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
	// StopToolCallID is set when a StopAtTools tool ended the child run. Its
	// result, not an assistant turn, is then the child's answer.
	StopToolCallID string `json:"stop_tool_call_id,omitempty"`

	// Iterations is how many model turns the child used, its forced final
	// answer included. MaxIter is its cap, NoIterLimit when it had none.
	Iterations int `json:"iterations"`
	MaxIter    int `json:"max_iter"`
	// Forced is set when the child reached a step limit and its answer was
	// forced with its tools removed (MaxIterForceFinal). ForcedReason names
	// the limit. The parent model is told, through ParentText.
	Forced       bool   `json:"forced,omitempty"`
	ForcedReason string `json:"forced_reason,omitempty"`
	// OutputRef is the saige-artifact:// URI of Output when it was too
	// large to return inline (see SubAgentReferences). The parent model
	// then received the URI and a preview; Output still holds all of it.
	OutputRef string `json:"output_ref,omitempty"`
	// Scratch is a read-only view of the child's private scratch, nil when
	// the definition turned it off. It is not serialized.
	Scratch workspace.Workspace `json:"-"`

	// parentText is what the parent model receives instead of Output when
	// Output went by reference.
	parentText string
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

// StopToolResult returns the result of the tool call that ended the run
// through StopAtTools.
func (r SubAgentResult) StopToolResult() (types.ToolResultPart, error) {
	if r.Error != "" {
		return types.ToolResultPart{}, errors.New(r.Error)
	}
	if r.StopToolCallID == "" {
		return types.ToolResultPart{}, errors.New("subagent did not stop at a tool")
	}
	msgs, err := r.Messages()
	if err != nil {
		return types.ToolResultPart{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, res := range toolResultsOf(msgs[i]) {
			if res.CallID == r.StopToolCallID {
				return res, nil
			}
		}
	}
	return types.ToolResultPart{}, fmt.Errorf("subagent stop tool result %q not found", r.StopToolCallID)
}

// toolResultsOf returns the tool results a message carries.
func toolResultsOf(m types.Message) []types.ToolResultPart {
	var out []types.ToolResultPart
	switch v := m.(type) {
	case types.SystemMessage:
		for _, c := range v.Parts {
			if res, ok := c.(types.ToolResultPart); ok {
				out = append(out, res)
			}
		}
	case types.UserMessage:
		for _, c := range v.Parts {
			if res, ok := c.(types.ToolResultPart); ok {
				out = append(out, res)
			}
		}
	}
	return out
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
// and nested delegation output are excluded. A child that ended at a
// StopAtTools tool returns that tool's result text.
type FinalAssistantText struct{}

func (FinalAssistantText) Select(r SubAgentResult) (string, error) {
	if r.StopToolCallID != "" {
		res, err := r.StopToolResult()
		return res.Text(), err
	}
	msg, err := r.FinalAssistant()
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, block := range msg.Parts {
		if content, ok := block.(types.TextPart); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String(), nil
}

// SchemaResult returns the child's answer as compact JSON after checking it
// against Schema. The answer is the final text, or the result of the tool
// that ended the run, such as final_answer. An answer with no JSON, or JSON
// that does not match, fails the delegation, so the parent never receives
// unchecked output as success. It is the default policy for a SubAgentDef
// with a ResponseSchema.
type SchemaResult struct {
	Schema *types.ParameterSchema
	// Extract pulls the JSON out of the answer. nil uses ExtractJSON.
	Extract func(string) (string, error)
}

func (p SchemaResult) Select(r SubAgentResult) (string, error) {
	text, err := FinalAssistantText{}.Select(r)
	if err != nil {
		return "", err
	}
	extract := p.Extract
	if extract == nil {
		extract = ExtractJSON
	}
	raw, err := extract(text)
	if err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoJSON, err)
	}
	if p.Schema != nil {
		if err := types.ValidateJSON(*p.Schema, value); err != nil {
			return "", err
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return "", err
	}
	return compact.String(), nil
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

	refs     SubAgentReferences
	parentWS workspace.Workspace
	scratch  workspace.Workspace // the child's private scratch, or nil
}

func (a *Agent) captureSubAgent(ctx context.Context, stream *EventStream, runErr error) error {
	capture := stream.capture
	if capture == nil {
		return runErr
	}
	r := capture.result
	r.CompletedAt = time.Now().UTC()
	r.Branch = stream.branch
	r.StopToolCallID = stream.stopToolCallID
	r.Iterations, r.Forced, r.ForcedReason = stream.iterations, stream.forced, stream.forcedReason
	r.Scratch = readOnlyView(capture.scratch)
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
			capture.referenceResult(ctx, &r)
		}
	}
	if capture.sink != nil {
		if err := capture.sink.Save(ctx, cloneSubAgentResult(r)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("save subagent result: %w", err))
			r.Error = runErr.Error()
			r.Output, r.OutputRef, r.parentText = "", "", ""
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
// No parent conversation messages are appended by this operation, and the
// child starts with the task alone whatever its Context mode, since there is
// no delegating turn to copy from. It works for delegate and spawn
// definitions alike; the caller owns the returned stream either way.
func (a *Agent) InvokeSubAgent(ctx context.Context, name, task string) (*EventStream, error) {
	child, err := a.subAgentTool(name)
	if err != nil {
		return nil, err
	}
	id := types.NewID()
	frame := callFrame{agents: append(slices.Clone(frameFrom(ctx).agents), a.cfg.Name), callIDs: []string{id}}
	return child.start(ctx, childRun{task: task, runner: a.childStepRunner(id), id: id, frame: frame})
}

// subAgentTool finds the registered tool for the sub-agent name, in either
// mode.
func (a *Agent) subAgentTool(name string) (*subAgentTool, error) {
	for _, prefix := range []string{"delegate_to_", "spawn_"} {
		tool, ok := a.tools.Get(prefix + name)
		if !ok {
			continue
		}
		switch t := tool.(type) {
		case *subAgentTool:
			return t, nil
		case *spawnTool:
			return t.subAgentTool, nil
		}
		return nil, fmt.Errorf("tool for %q is not a registered subagent", name)
	}
	return nil, fmt.Errorf("unknown subagent %q", name)
}
