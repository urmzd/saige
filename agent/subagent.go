package agent

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// SubAgentDef defines a sub-agent that can be delegated to.
//
// A sub-agent is a full agent, so it needs a full config. Its operational
// config is inherited from the parent when the child is built (see
// inheritConfig for the exact lists): logger, metrics, timeouts, tool
// parallelism, gates and policies, compaction, dials, the file pipeline, and
// step-limit behavior. Leaving MaxIter or Provider zero means "same as my
// parent", never "off". Some parent settings are deliberately not inherited,
// such as the tree, store, outcome policy, server tools, StopAtTools and tool
// choice. Delegation is refused for an ancestor of the caller.
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
	// MaxIter is the child's iteration budget. 0 inherits the parent's cap,
	// or DefaultSubAgentMaxIter when the parent has none, so a child is
	// bounded even under an unbounded orchestrator. NoIterLimit removes it.
	// At the cap the child gives a forced final answer with its tools
	// removed (MaxIterForceFinal) instead of failing the delegation; set
	// WithOnMaxIter(MaxIterError) in Options to fail it instead.
	MaxIter int
	// WrapUpAt is the iteration after which the child is told how many
	// iterations remain and that it must return its result now, or call
	// final_answer when the child answers through it. 0 places it
	// DefaultWrapUpMargin iterations before MaxIter; a negative value sends
	// no note.
	WrapUpAt int
	// WrapUpPrompt replaces DefaultWrapUpPrompt in the wrap-up note.
	WrapUpPrompt string

	// Timeout bounds the whole delegated run. Time the child spends waiting
	// for a human approval does not count against it. 0 means no limit beyond
	// the child's own per-call LLMTimeout and ToolTimeout, the parent's
	// context, and the shared Budget. The parent's ToolTimeout never applies
	// to a delegation as a whole.
	Timeout time.Duration

	// Options are applied last, after inheritance and after the fields above,
	// so any inherited value can be overridden per sub-agent. This is the
	// escape hatch that keeps SubAgentDef from having to mirror every field of
	// Config.
	Options []Option

	// ResponseSchema makes the child answer with JSON that matches it. The
	// child uses its provider's structured output when the provider supports
	// it, and the final_answer tool otherwise. With no ResultPolicy the parent
	// receives the validated JSON (see SchemaResult), and an answer that does
	// not match fails the delegation. Decode it with DecodeOutput.
	ResponseSchema *types.ParameterSchema

	// ResultPolicy selects the parent tool result. Nil returns final assistant
	// text, or SchemaResult when ResponseSchema is set.
	ResultPolicy SubAgentResultPolicy
	// ResultSink retains complete traces, including failed calls. Nil retains only
	// the direct invocation stream result, for as long as the caller keeps it.
	ResultSink SubAgentResultSink

	// Mode selects delegate_to_<name>, where the parent waits for the
	// result, or spawn_<name>, where the child runs in the background and
	// the parent receives a handle. The zero value is SubAgentDelegate.
	Mode SubAgentMode

	// Context selects which parent messages the child starts with. The zero
	// value, ContextTaskOnly, sends the task alone. ContextFiltered reads
	// ContextFilter.
	Context       SubAgentContext
	ContextFilter MessageSelector

	// OmitCallerBlock leaves out the <caller> block that otherwise precedes
	// the task. The block names the call path and depth, says that the
	// child's final message is its whole result, and lists the ancestors the
	// child must not delegate back to. Delegation to an ancestor is refused
	// either way.
	OmitCallerBlock bool

	// Scratch configures the child's private scratch workspace. By default
	// every invocation gets a fresh in-memory one with scratch_write,
	// scratch_read, and scratch_search. See SubAgentScratch.
	Scratch SubAgentScratch

	// References sets when the delegation passes data by reference instead
	// of inline: a large task or attached message goes into the child's
	// scratch as an artifact, and a large result comes back to the parent
	// as an artifact URI with a preview. See SubAgentReferences.
	References SubAgentReferences
}

// inheritConfig builds a sub-agent's Config from its definition and its
// parent's config. The split is deliberate:
//
//   - Inherited (operational): Logger, Metrics, LLMTimeout, ToolTimeout,
//     MaxParallelTools, CompactCfg, CompactProvider, Resolvers, Extractors, Conversion, ToolGate,
//     ToolPolicy, ApprovalPolicy, Deps, ToolContext, Tokenizer, ToolRedactor,
//     OnMaxIter, ForceFinalPrompt, MaxConsecutiveErrors, MaxRepeatIterations,
//     InterruptTTL, InterruptPolicy, Dials and DialPolicy. These describe how
//     this deployment runs agents, not what one agent is for, so a child that
//     did not inherit them would quietly run with different guarantees than
//     the parent that delegated to it.
//   - From the definition (identity): Name, SystemPrompt, Tools, SubAgents,
//     MaxIter and WrapUpAt (see childIterBudget), ResponseSchema, and
//     Provider when set. OnMaxIter is MaxIterForceFinal, so a child at its
//     limit answers instead of failing.
//   - Deliberately NOT inherited:
//     Tree, because sub-agents are stateless across delegations and each
//     invocation builds a fresh one;
//     Store, because a fresh tree per call would write a new root into the
//     parent's store on every delegation;
//     the parent's ResponseSchema, because it constrains the parent's final
//     answer, not the child's; the child's own comes from the definition;
//     OutcomePolicy, because the parent observes the child's failure and
//     decides for itself;
//     Handoffs and MaxHandoffs, because a handoff group belongs to the entry
//     agent that owns the shared tree;
//     ServerTools, because they are bound to the parent's provider instance and
//     a child targeting a different model may not support them;
//     StopAtTools and ToolChoice, because they name the parent's tools;
//     every other field not listed above (for example AutoContinue,
//     HandoffContextPolicy and LinkPolicy) starts at its zero value.
//
// Budget is shared rather than inherited: see the comment at the assignment.
// Workspace is narrowed: the child receives a read-only view of the parent's,
// under its private scratch when the factory adds one.
//
// StepRunner is passed separately: the parent's effective runner is only known
// at invocation time, since RunDurable injects one after registration.
func inheritConfig(parent Config, sa SubAgentDef, runner types.StepRunner) Config {
	provider := sa.Provider
	if provider == nil {
		provider = parent.Provider
	}
	if sessions, ok := provider.(types.SessionProvider); ok {
		provider = sessions.NewSession()
	}
	maxIter, wrapUpAt := childIterBudget(parent.MaxIter, sa)
	return Config{
		Name:         sa.Name,
		SystemPrompt: sa.SystemPrompt,
		Provider:     provider,
		Tools:        sa.Tools,
		SubAgents:    sa.SubAgents,
		MaxIter:      maxIter,
		WrapUpAt:     wrapUpAt,
		WrapUpPrompt: sa.WrapUpPrompt,
		StepRunner:   runner,

		// OutputMode stays OutputAuto here. resolveChildOutputMode picks it
		// after SubAgentDef.Options, which may replace the provider.
		ResponseSchema: sa.ResponseSchema,

		// Inherited operational config.
		Logger:           parent.Logger,
		Metrics:          parent.Metrics,
		LLMTimeout:       parent.LLMTimeout,
		ToolTimeout:      parent.ToolTimeout,
		MaxParallelTools: parent.MaxParallelTools,
		CompactCfg:       parent.CompactCfg,
		CompactProvider:  parent.CompactProvider,
		Resolvers:        parent.Resolvers,
		Extractors:       parent.Extractors,
		Conversion:       parent.Conversion,
		ToolGate:         parent.ToolGate,
		ToolPolicy:       parent.ToolPolicy,
		ApprovalPolicy:   parent.ApprovalPolicy,
		Deps:             parent.Deps,
		ToolContext:      parent.ToolContext,
		Tokenizer:        parent.Tokenizer,
		// The redactor is shared so a placeholder in the task means the same
		// value to the child's tools. The child reads the parent's workspace
		// but cannot write to it.
		ToolRedactor: parent.ToolRedactor,
		Workspace:    readOnlyView(parent.Workspace),
		// A bounded child answers at its limit rather than failing the
		// delegation, whatever the parent does at its own. StopAtTools and
		// ToolChoice name the parent's tools and are not inherited.
		OnMaxIter:            MaxIterForceFinal,
		ForceFinalPrompt:     parent.ForceFinalPrompt,
		MaxConsecutiveErrors: parent.MaxConsecutiveErrors,
		MaxRepeatIterations:  parent.MaxRepeatIterations,
		InterruptTTL:         parent.InterruptTTL,
		InterruptPolicy:      parent.InterruptPolicy,
		// Budget is shared by pointer, not copied: a per-child copy would let a
		// run with four sub-agents spend four times its ceiling, which is the
		// precise failure a budget exists to prevent. Give a sub-agent its own
		// Budget through Options to cap that delegation separately.
		Budget: parent.Budget,
		// Dials describe how this deployment wants models to generate, so a
		// child keeps them unless its Options set WithDials.
		Dials:      parent.Dials.Clone(),
		DialPolicy: parent.DialPolicy,
		// Hooks and guardrails are run policy, like the gate: a child runs
		// its parent's, then any its Options add.
		Hooks:            slices.Clone(parent.Hooks),
		HookTimeout:      parent.HookTimeout,
		InputGuardrails:  slices.Clone(parent.InputGuardrails),
		OutputGuardrails: slices.Clone(parent.OutputGuardrails),
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
	def types.ToolDef
	// factory builds the child for one invocation, id, with its private
	// scratch.
	factory func(ctx context.Context, runner types.StepRunner, id string) (*Agent, error)
	name    string
	policy  SubAgentResultPolicy
	sink    SubAgentResultSink
	timeout time.Duration // SubAgentDef.Timeout; 0 means none

	context    SubAgentContext
	filter     MessageSelector
	omitCaller bool

	refs SubAgentReferences // with defaults applied
	// parentWS is the parent's workspace, where a large result is stored
	// when it accepts writes.
	parentWS workspace.Workspace
}

func (t *subAgentTool) Definition() types.ToolDef { return t.def }

// Execute provides a blocking fallback: runs the child agent and returns
// the selected result. The agent loop prefers InvokeAgent for streaming.
// Execute has no consumer to resolve approvals, so a child call that needs
// one fails at once instead of waiting forever.
func (t *subAgentTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	task, _ := args[argTask].(string)
	if t.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}
	stream, err := t.start(ctx, childRun{task: task, id: types.NewID(), nonStreaming: true, frame: frameFrom(ctx)})
	if err != nil {
		return "", err
	}
	for range stream.Deltas() {
	}
	result, err := stream.SubAgentResult()
	return result.ParentText(), err
}

// InvokeAgent creates a fresh child agent and invokes it, returning its stream.
func (t *subAgentTool) InvokeAgent(ctx context.Context, task string) *EventStream {
	stream, err := t.start(ctx, childRun{task: task, id: types.NewID(), frame: frameFrom(ctx)})
	if err != nil {
		return failedStream(ctx, err)
	}
	return stream
}

// childRun describes one start of a sub-agent.
type childRun struct {
	task string
	// history holds parent messages placed before the task.
	history []types.Message
	// runner is the StepRunner the child inherits; nil runs it inline.
	runner types.StepRunner
	id     string
	// nonStreaming marks a child whose caller cannot resolve approvals, so
	// the child fails fast rather than emitting a marker nobody answers.
	nonStreaming bool
	// frame places the child in the delegation tree: its callers and the
	// call IDs that lead to it.
	frame callFrame
	// background marks a spawned child. It takes steering messages and holds
	// a budget reservation from its start until its first provider call.
	background bool
}

// start creates a fresh child agent and runs it on a new stream. A
// background child's budget admission happens here, so a spawn that the
// budget cannot admit fails before anything runs.
func (t *subAgentTool) start(ctx context.Context, req childRun) (*EventStream, error) {
	child, err := t.factory(ctx, req.runner, req.id)
	if err != nil {
		return nil, err
	}
	task, history, referenced, err := t.passByReference(ctx, child, req.task, req.history)
	if err != nil {
		return nil, err
	}
	if referenced {
		registerArtifactTools(child.tools)
	}
	if req.background && child.cfg.Budget != nil {
		admission, err := admitChild(child, req.id)
		if err != nil {
			return nil, err
		}
		child.admission = admission
	}
	ctx = withFrame(ctx, req.frame)
	ctx, cancel := context.WithCancel(ctx)
	stream := newEventStream(ctx, cancel)
	stream.nonStreaming = req.nonStreaming
	if req.frame.runID != "" {
		stream.runID = req.frame.runID
	}
	stream.path = append([]string(nil), req.frame.callIDs...)
	if req.background {
		stream.inbox = &inbox{}
	}
	stream.capture = &subAgentCapture{
		result: SubAgentResult{ID: req.id, Name: t.name, Task: req.task, StartedAt: time.Now().UTC(), MaxIter: child.cfg.MaxIter},
		policy: t.policy, sink: t.sink,
		refs: t.refs, parentWS: t.parentWS, scratch: child.scratch,
	}
	input := append(history, seedMessage(req.frame, t.name, task, t.omitCaller))
	go func() {
		defer child.admission.release(child.cfg.Budget)
		child.runLoop(ctx, stream, input, child.cfg.Tree.Active(), nil)
	}()
	return stream, nil
}

// failedStream returns a stream that ends at once with err.
func failedStream(ctx context.Context, err error) *EventStream {
	ctx, cancel := context.WithCancel(ctx)
	stream := newEventStream(ctx, cancel)
	go func() {
		defer cancel()
		stream.send(types.ErrorDelta{Error: err})
		stream.send(types.DoneDelta{})
		stream.close(err)
	}()
	return stream
}

// prefixStepRunner namespaces step names before delegating to the inner runner.
// Parent and child agents both derive step names from branch+iteration
// ("llm-main-0", ...), so a child sharing the parent's runner would otherwise
// replay the parent's recorded steps.
type prefixStepRunner struct {
	inner        types.StepRunner
	prefix       string
	parentBudget *types.Budget
}

// prefixApprovalRunner is a prefixStepRunner over an inner runner that also
// resolves approvals durably. Only this wrapper satisfies types.ApprovalRunner,
// so a child under a runner without durable approvals streams its approvals
// like the parent does, instead of failing on a capability the inner runner
// never had.
type prefixApprovalRunner struct {
	prefixStepRunner
}

func (r prefixStepRunner) RunStep(ctx context.Context, name string, fn func(ctx context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return r.inner.RunStep(ctx, r.prefix+name, fn)
}

// childStepRunner returns the runner a delegated child agent inherits. The
// inline NoopStepRunner needs no threading (nil lets the child default); a
// durable runner is namespaced under the delegating tool call ID. The child
// can resolve approvals durably only when the inner runner can.
func (a *Agent) childStepRunner(toolCallID string) types.StepRunner {
	if a.cfg.StepRunner == nil {
		return nil
	}
	if _, isNoop := a.cfg.StepRunner.(types.NoopStepRunner); isNoop {
		return nil
	}
	base := prefixStepRunner{inner: a.cfg.StepRunner, prefix: "sub-" + toolCallID + "-", parentBudget: a.cfg.Budget}
	if _, ok := a.cfg.StepRunner.(types.ApprovalRunner); ok {
		return prefixApprovalRunner{base}
	}
	return base
}

// asPrefixRunner unwraps either child runner wrapper.
func asPrefixRunner(r types.StepRunner) (prefixStepRunner, bool) {
	switch v := r.(type) {
	case prefixStepRunner:
		return v, true
	case prefixApprovalRunner:
		return v.prefixStepRunner, true
	default:
		return prefixStepRunner{}, false
	}
}

// ResolveApproval keeps the same namespace as the child's steps.
func (r prefixApprovalRunner) ResolveApproval(ctx context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	runner, ok := r.inner.(types.ApprovalRunner)
	if !ok {
		return types.ApprovalDecision{}, errors.New("durable child approvals require an ApprovalRunner")
	}
	req.ID = r.prefix + req.ID
	return runner.ResolveApproval(ctx, req)
}
func (r prefixStepRunner) ConcurrentSteps() bool {
	runner, ok := r.inner.(types.ConcurrentStepRunner)
	return ok && runner.ConcurrentSteps()
}

func (r prefixStepRunner) RecordReservation(ctx context.Context, name string, receipt types.BudgetReceipt) error {
	if recorder, ok := r.inner.(types.BudgetReservationRunner); ok {
		return recorder.RecordReservation(ctx, r.prefix+name, receipt)
	}
	return nil
}

func (r prefixStepRunner) SharedBudgetOnly() bool {
	runner, ok := r.inner.(types.SharedBudgetRunner)
	return ok && runner.SharedBudgetOnly()
}

// childOutputMode picks how a sub-agent's ResponseSchema reaches its model:
// native structured output when the provider supports it, otherwise the
// final_answer tool, so the schema is never dropped.
func childOutputMode(provider types.Provider, schema *types.ParameterSchema) OutputMode {
	if schema == nil || checkStructuredOutput(provider) == nil {
		return OutputAuto
	}
	return OutputTool
}

// resolveChildOutputMode is the last option applied to a sub-agent. It
// resolves OutputAuto for the final provider and keeps a mode an option set.
func resolveChildOutputMode(c *Config) {
	if c.OutputMode == OutputAuto {
		c.OutputMode = childOutputMode(c.Provider, c.ResponseSchema)
	}
}

// readOnlyView returns a read-only view of ws, or nil.
func readOnlyView(ws workspace.Workspace) workspace.Workspace {
	if ws == nil {
		return nil
	}
	return ws.View(true)
}
