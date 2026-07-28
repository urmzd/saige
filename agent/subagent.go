package agent

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// SubAgentDef defines a sub-agent that can be delegated to.
//
// A sub-agent is a full agent, so it needs a full config. Everything not named
// here is inherited from the parent at registration time (see inheritConfig):
// logger, metrics, timeouts, tool parallelism, compaction, and the file
// resolver/extractor pipeline. Leaving a field zero means "same as my parent",
// never "off" -- a delegated child that silently ran without the parent's LLM
// timeout or file resolvers is the failure this inheritance exists to prevent.
//
// Use Options for anything inheritance gets wrong for a particular child.
type SubAgentDef struct {
	Name         string
	Description  string
	SystemPrompt string
	// Provider targets this sub-agent at its own model. nil inherits the
	// parent's provider, which is the common case: most sub-agents differ by
	// prompt and tools, not by model.
	Provider  types.Provider
	Tools     *types.ToolRegistry
	SubAgents []SubAgentDef // sub-agents can have their own sub-agents
	MaxIter   int           // 0 inherits the parent's MaxIter

	// Options are applied last, after inheritance and after the fields above,
	// so any inherited value can be overridden per sub-agent. This is the
	// escape hatch that keeps SubAgentDef from having to mirror every field of
	// AgentConfig.
	Options []AgentOption
}

// inheritConfig builds a sub-agent's AgentConfig from its definition and its
// parent's config. The split is deliberate:
//
//   - Inherited (operational): Logger, Metrics, LLMTimeout, ToolTimeout,
//     MaxParallelTools, CompactCfg, Resolvers, Extractors. These describe how
//     this deployment runs agents, not what one agent is for, so a child that
//     did not inherit them would quietly run with different guarantees than the
//     parent that delegated to it.
//   - From the definition (identity): Name, SystemPrompt, Tools, SubAgents,
//     MaxIter, and Provider when set.
//   - Deliberately NOT inherited:
//     Tree, because sub-agents are stateless across delegations and each
//     invocation builds a fresh one;
//     Store, because a fresh tree per call would write a new root into the
//     parent's store on every delegation;
//     ResponseSchema, because it constrains the parent's final answer, not the
//     child's working output;
//     Handoffs and MaxHandoffs, because a handoff group belongs to the entry
//     agent that owns the shared tree;
//     ServerTools, because they are bound to the parent's provider instance and
//     a child targeting a different model may not support them.
//
// Budget is shared rather than inherited: see the comment at the assignment.
//
// StepRunner is passed separately: the parent's effective runner is only known
// at invocation time, since RunDurable injects one after registration.
func inheritConfig(parent AgentConfig, sa SubAgentDef, runner types.StepRunner) AgentConfig {
	provider := sa.Provider
	if provider == nil {
		provider = parent.Provider
	}
	maxIter := sa.MaxIter
	if maxIter <= 0 {
		maxIter = parent.MaxIter
	}
	return AgentConfig{
		Name:         sa.Name,
		SystemPrompt: sa.SystemPrompt,
		Provider:     provider,
		Tools:        sa.Tools,
		SubAgents:    sa.SubAgents,
		MaxIter:      maxIter,
		StepRunner:   runner,

		// Inherited operational config.
		Logger:           parent.Logger,
		Metrics:          parent.Metrics,
		LLMTimeout:       parent.LLMTimeout,
		ToolTimeout:      parent.ToolTimeout,
		MaxParallelTools: parent.MaxParallelTools,
		CompactCfg:       parent.CompactCfg,
		Resolvers:        parent.Resolvers,
		Extractors:       parent.Extractors,
		ToolGate:         parent.ToolGate,
		ToolContext:      parent.ToolContext,
		// Budget is shared by pointer, not copied: a per-child copy would let a
		// run with four sub-agents spend four times its ceiling, which is the
		// precise failure a budget exists to prevent. Give a sub-agent its own
		// Budget through Options to cap that delegation separately.
		Budget: parent.Budget,
	}
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
