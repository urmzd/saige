package agent

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// SubAgentDef defines a sub-agent that can be delegated to.
type SubAgentDef struct {
	Name         string
	Description  string
	SystemPrompt string
	Provider     types.Provider
	Tools        *types.ToolRegistry
	SubAgents    []SubAgentDef // sub-agents can have their own sub-agents
	MaxIter      int
}

// SubAgentInvoker is implemented by tools that wrap a sub-agent.
// The agent loop checks for this interface to enable delta forwarding
// instead of opaque Execute().
type SubAgentInvoker interface {
	InvokeAgent(ctx context.Context, task string) *EventStream
}

// subAgentTool wraps a sub-agent as a tool. It implements both types.Tool and
// SubAgentInvoker so the agent loop can forward child deltas. The factory takes
// the StepRunner the child should inherit (nil = inline execution) because the
// parent's effective runner is only known at invocation time: RunDurable
// injects a runner into a shallow clone after the tool was registered.
type subAgentTool struct {
	def     types.ToolDef
	factory func(runner types.StepRunner) *Agent
}

func (t *subAgentTool) Definition() types.ToolDef { return t.def }

// Execute provides a blocking fallback: runs the child agent and returns
// the concatenated text. The agent loop prefers InvokeAgent for streaming.
func (t *subAgentTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	task, _ := args["task"].(string)
	stream := t.InvokeAgent(ctx, task)
	var result string
	for d := range stream.Deltas() {
		if tc, ok := d.(types.TextContentDelta); ok {
			result += tc.Content
		}
	}
	return result, stream.Wait()
}

// InvokeAgent creates a fresh child agent and invokes it, returning its stream.
func (t *subAgentTool) InvokeAgent(ctx context.Context, task string) *EventStream {
	return t.invokeWithRunner(ctx, task, nil)
}

// invokeWithRunner creates a fresh child agent that inherits the given
// StepRunner and invokes it. The agent loop uses this so durable execution
// covers delegated work too.
func (t *subAgentTool) invokeWithRunner(ctx context.Context, task string, runner types.StepRunner) *EventStream {
	child := t.factory(runner)
	return child.Invoke(ctx, []types.Message{types.NewUserMessage(task)})
}

// prefixStepRunner namespaces step names before delegating to the inner runner.
// Parent and child agents both derive step names from branch+iteration
// ("llm-main-0", ...), so a child sharing the parent's runner would otherwise
// replay the parent's recorded steps.
type prefixStepRunner struct {
	inner  types.StepRunner
	prefix string
}

func (r prefixStepRunner) RunStep(ctx context.Context, name string, fn func(ctx context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return r.inner.RunStep(ctx, r.prefix+name, fn)
}

// childStepRunner returns the runner a delegated child agent inherits. The
// inline NoopStepRunner needs no threading (nil lets the child default); a
// durable runner is namespaced under the delegating tool call ID.
func (a *Agent) childStepRunner(toolCallID string) types.StepRunner {
	if _, isNoop := a.cfg.StepRunner.(types.NoopStepRunner); isNoop {
		return nil
	}
	return prefixStepRunner{inner: a.cfg.StepRunner, prefix: "sub-" + toolCallID + "-"}
}
