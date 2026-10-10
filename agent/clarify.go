package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ClarificationToolName is the name of the tool ClarificationTool returns.
const ClarificationToolName = "ask_user"

// ClarificationTool returns a tool the model calls to ask the person running
// it a question. Register it like any other tool.
//
// A call posts a types.InterruptClarification interrupt whose Payload is a
// types.ClarificationPayload (read it with Interrupt.Clarification) and
// sends a MarkerDelta with one marker of kind "clarification". Answer it
// with EventStream.ReplyInterrupt and a reply from types.Answer, or with ResolveMarkerWithMessage, approved, with the answer as the message.
// The answer becomes the tool result. A refusal becomes a tool error, so the
// model can carry on without it.
//
// The wait follows the same rules as an approval: it does not hold a tool
// slot, ToolTimeout does not bound it, WithInterruptExpiry can bound it, and
// a run with no consumer fails at once unless its runner implements
// types.ApprovalRunner, which then records the answer as the decision
// message. A clarification raised inside a sub-agent reaches the parent's
// consumer the same way as the sub-agent's approvals.
func ClarificationTool() types.Tool {
	return &clarificationTool{}
}

type clarificationTool struct{}

func (*clarificationTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        ClarificationToolName,
		Description: "Ask the user a question when the task is ambiguous or needs information only the user has. Use it sparingly; the run waits for the answer.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argQuestion},
			Properties: map[string]types.PropertyDef{
				argQuestion: {Type: types.SchemaString, Description: "One clear question for the user."},
			},
		},
		Capability: types.ToolCapabilityRead,
	}
}

// Execute is reached only outside the agent loop, where nobody can answer.
func (*clarificationTool) Execute(context.Context, map[string]any) (string, error) {
	return "", errNoClarifier
}

var errNoClarifier = errors.New("ask_user needs an agent run with a consumer to answer it")

// askClarification posts a clarification interrupt for tc and returns the
// answer as the call's result.
func (a *Agent) askClarification(ctx context.Context, stream *EventStream, tc types.ToolCallPart) toolResult {
	question := strings.TrimSpace(stringArg(tc.Arguments, argQuestion))
	if question == "" {
		return failedTool(stream, tc.ID, tc.Name, "question is empty")
	}
	payload, _ := json.Marshal(types.ClarificationPayload{Question: question})
	d, ok := a.awaitInterrupt(ctx, stream, interruptRequest{
		kind:    types.InterruptClarification,
		phase:   "clarification",
		call:    tc,
		markers: []types.Marker{{Kind: string(types.InterruptClarification), Message: question}},
		payload: payload,
	})
	if !ok {
		return refusedTool(stream, tc.ID, tc.Name, d.message)
	}
	if strings.TrimSpace(d.message) == "" {
		return refusedTool(stream, tc.ID, tc.Name, "the user gave no answer")
	}
	res := toolResult{toolCallID: tc.ID, result: d.message}
	stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Name: tc.Name, Result: res.result})
	return res
}
