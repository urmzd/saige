package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/store/walrecover"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// AgentConfig holds configuration for an Agent.
type AgentConfig struct {
	Name         string
	SystemPrompt string
	Provider     types.Provider
	Tools        *types.ToolRegistry
	CompactCfg   *types.CompactConfig // initial compaction config (replaces Compactor)
	MaxIter      int
	SubAgents    []SubAgentDef
	Tree         *tree.Tree // optional; auto-created if nil

	// Agent handoffs: a group of agents that share this (entry) agent's tree and
	// transfer control via handoff_to_<name> tools (see agent/handoff.go).
	HandoffContextPolicy HandoffContextPolicy
	Handoffs             []HandoffDef
	LinkPolicy           LinkPolicy // nil adds direct return links
	MaxHandoffs          int        // max control transfers per run (default 8); ping-pong guard

	// StepRunner durably memoizes LLM and tool calls so a crashed process can
	// resume without repeating them. Defaults to types.NoopStepRunner (inline,
	// today's streaming behavior). Durable runners live in agent/durable/local
	// and agent/durable/duraturo.
	StepRunner types.StepRunner

	// Store persists the conversation tree. Defaults to nil, which keeps the
	// tree in memory only. When set and Tree is nil, NewAgent builds the tree
	// with tree.WithStore, so every tree change persists: run nodes,
	// compaction branches, feedback, the active branch, archive state,
	// rewinds and checkpoints. A store write that fails then fails the change
	// and the run that made it. A tree passed in Tree should also be built
	// with tree.WithStore(store); otherwise only nodes the run adds are
	// written, best-effort. Reload a stored tree with LoadTreeFromStore.
	Store types.Store

	// LLMTimeout bounds each provider (LLM) call. A child context with this
	// deadline wraps the call in getAssistantMessage. 0 means no timeout.
	LLMTimeout time.Duration

	// ToolTimeout bounds each individual tool execution. A child context with
	// this deadline wraps each tool step in executeOneTool. Time spent waiting
	// for an approval is not counted, and a delegation to a sub-agent is not
	// bounded by it as a whole (see SubAgentDef.Timeout); the child applies it
	// to each of its own tool calls. 0 means no timeout.
	ToolTimeout time.Duration

	// MaxParallelTools caps how many tool goroutines run concurrently when tools
	// are fanned out. 0 means unlimited. 1 means tools run sequentially in the
	// order the model requested them, with no goroutines at all. A durable
	// StepRunner runs tools sequentially unless it implements
	// types.ConcurrentStepRunner and reports true (the local engine does), so
	// this has no effect under the others.
	MaxParallelTools int

	// File pipeline configuration.
	Resolvers  map[string]types.Resolver           // URI scheme → Resolver (e.g. "file", "https", "s3")
	Extractors map[types.MediaType]types.Extractor // MediaType → Extractor for non-native types

	// ResponseSchema constrains the final answer to this JSON schema. See
	// WithResponseSchema for how it combines with tools.
	ResponseSchema *types.ParameterSchema
	// OutputMode selects how ResponseSchema reaches the model. The zero
	// value, OutputAuto, means OutputNative here. See OutputMode.
	OutputMode OutputMode

	// OutcomePolicy may switch the model after an outcome such as a failed
	// sub-agent or structured output that never validated. nil never
	// switches. See types.OutcomePolicy.
	OutcomePolicy types.OutcomePolicy

	// ToolGate decides whether each tool call may proceed, before it runs. It
	// sees the resolved definition and the model's actual arguments, so it can
	// allow a read and stop a write on the same tool. Defaults to
	// types.AllowAllGate. The pre-existing MarkedTool mechanism still applies
	// and runs after the gate.
	ToolGate   types.ToolGate
	ToolPolicy ToolPolicy // nil exposes all tools

	// ApprovalPolicy lets approvals adapt within a conversation: grants a
	// person attaches to an approval, a denial limit, capability-class
	// defaults, and an opt-in approval ramp. nil asks about every held call.
	// See ApprovalPolicy.
	ApprovalPolicy *ApprovalPolicy

	// ToolContext carries the configurable knobs tools read (see
	// types.ToolContext). It is attached to every tool call's context, so a
	// tool reads per-deployment configuration without its signature changing.
	ToolContext types.ToolContext

	// Deps holds the host's typed dependencies for Func tools: a database
	// handle, a client, a tenant. A Func tool receives it as RunContext.Deps.
	// Deps attached to the run's context with ContextWithDeps take
	// precedence. nil attaches nothing.
	Deps any

	// optionErr records an option that could not be applied, such as
	// WithHarnessTools with a missing root. Every run then fails with it.
	optionErr error

	// ToolRedactor keeps sensitive values on the tool side of the boundary.
	// Arguments are restored just before a tool executes, after the gate and
	// any approval saw the placeholders, and results are tokenized before
	// they are recorded, streamed, or sent to the provider. nil passes values
	// through unchanged. Sub-agents share it, so a placeholder means the same
	// value across a delegation. See package privacy.
	ToolRedactor types.ToolRedactor

	// Workspace is the run's scratch store. It is attached to every tool
	// call's context (see workspace.NewContext), and each sub-agent receives
	// a read-only view of it. nil attaches nothing.
	Workspace workspace.Workspace

	// Budget caps what a run may spend. Checked on every usage report rather
	// than once per iteration, because one long-context call can cost more than
	// the whole allowance. nil means unlimited.
	Budget *types.Budget

	// ServerTools records the provider-executed tools (web search, code
	// execution, remote MCP) this agent is meant to use. It is informational:
	// the agent loop neither sends nor validates it. Server tools are enabled
	// and validated by the adapter's own options, anthropic.WithServerTools
	// or google.WithServerTools, which check them against the model's
	// declared capabilities.
	ServerTools []types.ServerTool

	// Logger for agent events. Defaults to slog.Default() if nil.
	Logger *slog.Logger

	// Metrics collector. Defaults to NoopMetrics if nil. A collector that
	// also implements types.AgentOutcomeRecorder receives how each run ended,
	// and one that implements types.CacheUsageRecorder receives prompt-cache
	// token counts.
	Metrics types.Metrics

	// RunTracer, when set, opens a span around each run. Provider and tool
	// spans of the run, and of the sub-agents it delegates to, become its
	// children. otel.WithTracing sets it.
	RunTracer RunTracer

	// Tokenizer measures input size for CompactConfig.MaxInputTokens before
	// the provider has reported any usage. nil uses types.EstimatingTokenizer.
	Tokenizer types.Tokenizer

	// OnMaxIter chooses what happens when a run reaches a step limit with
	// tool results still unanswered: the iteration cap, MaxConsecutiveErrors,
	// or MaxRepeatIterations. The default, MaxIterError, returns the limit's
	// error. MaxIterForceFinal asks the model for a final answer instead.
	OnMaxIter MaxIterPolicy
	// ForceFinalPrompt replaces DefaultForceFinalPrompt for MaxIterForceFinal.
	ForceFinalPrompt string

	// Dials are the agent's model-neutral generation intents, sent with every
	// call and compiled for the model that serves it (see types.ResolveDials).
	// A ConfigContent.Dials in the conversation applies on top of them.
	Dials types.Dials
	// DialPolicy sets how dials the serving model cannot honor are handled.
	// Nil uses each dial's class.
	DialPolicy *types.DialPolicy

	// StopAtTools ends the run as soon as one of these tools returns a
	// successful result. The result becomes the run's output (see
	// SubAgentResult.StopToolCallID); the model is not called again.
	StopAtTools []string

	// MaxConsecutiveErrors stops the run with ErrToolErrorLimit after this
	// many consecutive turns in which every tool call failed. 0 uses
	// DefaultMaxConsecutiveErrors; a negative value disables the check.
	MaxConsecutiveErrors int
	// MaxRepeatIterations stops the run with ErrRepeatedToolCalls when the
	// model requests the same tool calls with the same arguments more than
	// this many turns in a row. The repeated calls are not run. 0 disables it.
	MaxRepeatIterations int

	// ToolChoice constrains tool use. Auto and none apply to every turn. A
	// required or named choice applies to the first turn of each run and then
	// reverts to auto, so a forced call cannot loop. Any mode other than auto
	// or none needs a provider that implements types.OptionsProvider.
	ToolChoice *types.ToolChoice

	// AutoContinue lets a run resume up to this many times when the output
	// token limit cuts a text-only turn short (see WithAutoContinue). 0
	// disables it.
	AutoContinue int

	// InterruptTTL bounds how long a run waits for a decision: an approval,
	// a budget escalation, or a clarification. 0 means no deadline.
	// InterruptPolicy says what an unanswered decision becomes. See
	// WithInterruptExpiry.
	InterruptTTL    time.Duration
	InterruptPolicy types.InterruptPolicy
}

// AgentOption configures an AgentConfig using the functional options pattern.
type AgentOption func(*AgentConfig)

// WithCompactConfig sets the compaction strategy.
func WithCompactConfig(cfg *types.CompactConfig) AgentOption {
	return func(c *AgentConfig) { c.CompactCfg = cfg }
}

// WithSubAgents registers sub-agents for delegation.
func WithSubAgents(subs ...SubAgentDef) AgentOption {
	return func(c *AgentConfig) { c.SubAgents = append(c.SubAgents, subs...) }
}

// WithTree attaches a pre-existing conversation tree.
func WithTree(t *tree.Tree) AgentOption {
	return func(c *AgentConfig) { c.Tree = t }
}

// WithResolvers sets URI scheme resolvers for file content.
func WithResolvers(resolvers map[string]types.Resolver) AgentOption {
	return func(c *AgentConfig) { c.Resolvers = resolvers }
}

// WithExtractors sets media type extractors for non-native content.
func WithExtractors(extractors map[types.MediaType]types.Extractor) AgentOption {
	return func(c *AgentConfig) { c.Extractors = extractors }
}

// WithResponseSchema constrains the final answer to a JSON schema.
//
// Without tools every turn is sent with the schema. With tools, turns are
// sent with the tools and without the schema, because most providers cannot
// combine the two. When such a turn ends without tool calls and its text is
// not already a JSON object that satisfies the schema, the loop discards that
// draft and asks once more with the schema and no tools; the structured reply
// is the one recorded as the final answer. Consumers see the draft's deltas
// stream before the structured reply's.
//
// The run fails with types.ErrInvalidModelConfig when the provider cannot
// constrain output: its declared capabilities report no structured output
// for a known model, or it does not implement types.StructuredOutputProvider. A schema is never dropped silently.
func WithResponseSchema(schema *types.ParameterSchema) AgentOption {
	return func(c *AgentConfig) { c.ResponseSchema = schema }
}

// WithLogger sets the agent's logger.
func WithLogger(logger *slog.Logger) AgentOption {
	return func(c *AgentConfig) { c.Logger = logger }
}

// WithMetrics sets the metrics collector.
func WithMetrics(metrics types.Metrics) AgentOption {
	return func(c *AgentConfig) { c.Metrics = metrics }
}

// WithToolGate sets the pre-execution gate for tool calls. Compose several
// with types.Gates; the most restrictive verdict wins.
func WithToolGate(g types.ToolGate) AgentOption {
	return func(c *AgentConfig) { c.ToolGate = g }
}

// WithToolContext sets the knobs tools read at call time.
func WithToolContext(tc types.ToolContext) AgentOption {
	return func(c *AgentConfig) { c.ToolContext = tc }
}

// WithToolRedactor sets the redactor applied at the tool boundary.
func WithToolRedactor(r types.ToolRedactor) AgentOption {
	return func(c *AgentConfig) { c.ToolRedactor = r }
}

// WithWorkspace sets the scratch store attached to tool calls.
func WithWorkspace(ws workspace.Workspace) AgentOption {
	return func(c *AgentConfig) { c.Workspace = ws }
}

// WithBudget caps what the run may spend. Share one budget across an agent and
// its sub-agents to cap the whole run; give a sub-agent its own to cap that
// delegation separately.
func WithBudget(b *types.Budget) AgentOption {
	return func(c *AgentConfig) { c.Budget = b }
}

// WithServerTools records provider-executed tools for this agent. It does not
// enable them: configure server tools on the adapter (for example
// google.WithServerTools), which validates them against the model.
func WithServerTools(tools ...types.ServerTool) AgentOption {
	return func(c *AgentConfig) { c.ServerTools = append(c.ServerTools, tools...) }
}

// WithMaxIter overrides the maximum agent loop iterations.
func WithMaxIter(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxIter = n }
}

// WithStepRunner sets a durable step runner. The default NoopStepRunner runs
// steps inline (today's streaming behavior). A durable runner (see
// agent/durable/local and agent/durable/duraturo) memoizes LLM and tool calls
// so a crashed process resumes without repeating them.
func WithStepRunner(r types.StepRunner) AgentOption {
	return func(c *AgentConfig) { c.StepRunner = r }
}

// WithStore configures a types.Store so the conversation tree is persisted.
// With no Store (the default) the tree is in-memory only. See
// AgentConfig.Store for what is written. Loading a persisted tree is done
// explicitly via LoadTreeFromStore before NewAgent (pass the rebuilt tree,
// built with tree.WithStore, through WithTree).
func WithStore(s types.Store) AgentOption {
	return func(c *AgentConfig) { c.Store = s }
}

// WithPreset uses a declared preset: its provider chain becomes the agent's
// provider, and its tool choice, output mode and LLM timeout become the
// agent's defaults. A field already set, in the AgentConfig or by an earlier
// option, is kept; options applied after WithPreset override it. An unknown
// output mode is rejected when a run starts, as with WithOutputMode.
func WithPreset(p types.Preset) AgentOption {
	return func(c *AgentConfig) {
		if p == nil {
			return
		}
		c.Provider = p.Provider()
		d := p.Defaults()
		if c.ToolChoice == nil && d.ToolChoice != nil {
			tc := *d.ToolChoice
			c.ToolChoice = &tc
		}
		if c.OutputMode == OutputAuto && d.OutputMode != "auto" {
			c.OutputMode = OutputMode(d.OutputMode)
		}
		if c.LLMTimeout == 0 {
			c.LLMTimeout = d.LLMTimeout
		}
	}
}

// WithLLMTimeout bounds each provider (LLM) call with a child context deadline.
// A slow provider is cancelled and surfaces a timeout error instead of hanging.
// A non-positive duration disables the timeout.
func WithLLMTimeout(d time.Duration) AgentOption {
	return func(c *AgentConfig) { c.LLMTimeout = d }
}

// WithToolTimeout bounds each individual tool execution with a child context
// deadline. A slow tool is cancelled and surfaces a timeout error. A
// non-positive duration disables the timeout.
func WithToolTimeout(d time.Duration) AgentOption {
	return func(c *AgentConfig) { c.ToolTimeout = d }
}

// WithMaxParallelTools caps how many tool goroutines run concurrently when tools
// are fanned out. A non-positive value means unlimited (today's behavior). A
// value of 1 runs tools sequentially in request order; see WithSequentialTools.
func WithMaxParallelTools(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxParallelTools = n }
}

// WithSequentialTools runs tool calls one at a time, in the order the model
// requested them, in the caller's goroutine.
//
// Fanning tools out is the right default: independent lookups finish in the
// time of the slowest one. But tools that share mutable state, or whose
// contract is defined by their order, need a total order the caller can
// predict. A semaphore of one is not enough on its own: it serializes
// execution while leaving the winner of each slot to the scheduler, so the
// observable order still varies run to run.
//
// This is sugar for WithMaxParallelTools(1).
func WithSequentialTools() AgentOption {
	return WithMaxParallelTools(1)
}

// Agent runs an LLM agent loop with tool execution.
// All conversations are backed by a Tree.
//
// An Agent is safe for concurrent use by runs on different branches: Invoke,
// Submit, Continue, and RunDurable may be called from many goroutines at
// once. One run may hold a branch at a time, and a second run on a busy
// branch fails with ErrRunActive. Tool calls within one turn run
// concurrently unless WithSequentialTools is set, so every Tool, ToolGate,
// ToolPolicy, result policy, and result sink the agent uses must be safe for
// concurrent use. Two calls to the same tool in one turn run on the same
// instance at the same time.
type Agent struct {
	cfg      AgentConfig
	tools    *types.ToolRegistry
	handoffs *handoffGroup // nil unless WithHandoffs configured an entry group
	// citations numbers every source cited during this agent's life, so a
	// provider's server-side search and a local retrieval tool citing the same
	// page produce one footnote rather than two.
	citations *types.CitationRegistry
	// spawning is true when a sub-agent definition uses SubAgentSpawn, so
	// each run keeps a registry of the children it starts.
	spawning bool
	// admission is the budget reservation a spawned child holds from its
	// start until its first provider call. Nil for every other agent.
	admission *childAdmission
}

// NewAgent creates a new Agent. If no Tree is provided, one is created
// automatically from the SystemPrompt. Initial config is seeded into the
// tree so that serialise/restore round-trips include the full agent config.
// Options are applied after the base config, allowing incremental composition.
func NewAgent(cfg AgentConfig, opts ...AgentOption) *Agent {
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.MaxIter <= 0 {
		cfg.MaxIter = 10
	}
	if cfg.MaxHandoffs <= 0 {
		cfg.MaxHandoffs = 8
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = types.NoopMetrics{}
	}
	if cfg.StepRunner == nil {
		cfg.StepRunner = types.NoopStepRunner{}
	}
	if cfg.ToolGate == nil {
		cfg.ToolGate = types.AllowAllGate{}
	}
	tools := types.NewToolRegistry()
	if cfg.Tools != nil {
		tools = types.NewToolRegistry(cfg.Tools.All()...)
	}

	if cfg.Tree == nil {
		var opts []tree.Option
		if cfg.Store != nil {
			opts = append(opts, tree.WithStore(cfg.Store))
		}
		t, err := tree.New(types.NewSystemMessage(cfg.SystemPrompt), opts...)
		if err != nil {
			// The root could not be stored. Keep the agent usable in memory;
			// the root is written again below and on each run.
			cfg.Logger.Warn("store persist failed for the root node; the tree starts in memory", "agent", cfg.Name, "error", err)
			t, _ = tree.New(types.NewSystemMessage(cfg.SystemPrompt))
		}
		cfg.Tree = t
	}

	// Register sub-agents as delegate tools. They inherit this agent's
	// operational config, so the defaults applied above (MaxIter, Logger,
	// Metrics, StepRunner) are already in place and propagate downward.
	spawning := false
	for _, sa := range cfg.SubAgents {
		registerSubAgent(tools, sa, cfg)
		spawning = spawning || sa.Mode == SubAgentSpawn
	}
	if spawning {
		registerSubAgentControls(tools)
	}

	a := &Agent{cfg: cfg, tools: tools, citations: types.NewCitationRegistry(), spawning: spawning}

	// When a Store is configured, persist the tree's root node + main branch tip
	// up front so a later LoadTreeFromStore has an anchor even before the first
	// Invoke. Best-effort: a failure here is logged, never fatal (mirrors the
	// per-node persistence in runLoop).
	if cfg.Store != nil {
		if root := cfg.Tree.Root(); root != nil {
			a.persistNode(context.Background(), root)
		}
	}

	// Build the handoff group (if any). The entry agent shares this tree; each
	// member's handoff_to_<target> tools are wired into its registry.
	if len(cfg.Handoffs) > 0 {
		entry := &handoffMember{
			name:     cfg.Name,
			provider: cfg.Provider,
			tools:    tools,
			maxIter:  cfg.MaxIter,
		}
		grp, err := buildHandoffGroup(entry, cfg.Handoffs, cfg.LinkPolicy)
		if err != nil {
			panic(fmt.Sprintf("agent: invalid handoff configuration: %v", err))
		}
		a.handoffs = grp
	}

	return a
}

// registerSubAgent registers a SubAgentDef as a delegate tool. Each invocation
// constructs a fresh Agent: the sub-agent's conversation history is intentionally
// not reused between delegations. A result sink can retain completed traces.
//
// parent is the delegating agent's config; the child inherits its operational
// settings (see inheritConfig) so a delegated run carries the same timeouts,
// logging, metrics, compaction and file pipeline as the run that spawned it.
func registerSubAgent(registry *types.ToolRegistry, sa SubAgentDef, parent AgentConfig) {
	policy := sa.ResultPolicy
	if policy == nil && sa.ResponseSchema != nil {
		policy = SchemaResult{Schema: sa.ResponseSchema}
	}
	tool := &subAgentTool{
		name: sa.Name, policy: policy, sink: sa.ResultSink, timeout: sa.Timeout,
		context: sa.Context, filter: sa.ContextFilter, omitCaller: sa.OmitCallerBlock,
		def: types.ToolDef{
			Name:        "delegate_to_" + sa.Name,
			Description: sa.Description,
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{argTask},
				Properties: map[string]types.PropertyDef{
					argTask: {Type: types.SchemaString, Description: "The task to delegate"},
				},
			},
		},
		factory: func(runner types.StepRunner) *Agent {
			// The output mode is chosen last, from the provider the
			// options leave in place.
			opts := append(slices.Clone(sa.Options), resolveChildOutputMode)
			return NewAgent(inheritConfig(parent, sa, runner), opts...)
		},
	}
	if sa.Mode != SubAgentSpawn {
		registry.Register(tool)
		return
	}
	tool.def.Name = "spawn_" + sa.Name
	tool.def.Parameters.Properties = map[string]types.PropertyDef{
		argTask: {Type: types.SchemaString, Description: "The task for the background sub-agent"},
	}
	registry.Register(&spawnTool{subAgentTool: tool})
}

// AgentInfo describes an agent for display purposes (e.g. TUI headers).
type AgentInfo struct {
	Name      string
	Provider  string   // provider name, if available
	Tools     []string // registered tool names
	SubAgents []string // sub-agent names
}

// Info returns display metadata about the agent.
func (a *Agent) Info() AgentInfo {
	info := AgentInfo{Name: a.cfg.Name}

	if np, ok := a.cfg.Provider.(types.NamedProvider); ok {
		info.Provider = np.Name()
	}

	for _, td := range a.tools.Definitions() {
		// Skip internal delegate/handoff tools: they show as sub-agents/handoffs.
		if strings.HasPrefix(td.Name, "delegate_to_") || strings.HasPrefix(td.Name, "spawn_") ||
			strings.HasPrefix(td.Name, "handoff_to_") || isSubAgentControlTool(td.Name) {
			continue
		}
		info.Tools = append(info.Tools, td.Name)
	}

	for _, sa := range a.cfg.SubAgents {
		info.SubAgents = append(info.SubAgents, sa.Name)
	}

	return info
}

// Tree returns the agent's conversation tree.
func (a *Agent) Tree() *tree.Tree {
	return a.cfg.Tree
}

// Feedback records a rating and optional comment on a node in the conversation
// tree. The feedback is attached as a permanent leaf branching off the target
// node: it lives on its own dead-end branch, is never flattened into LLM
// messages, and cannot have children.
func (a *Agent) Feedback(ctx context.Context, targetNodeID types.NodeID, rating types.Rating, comment string) (*types.Node, error) {
	msg := types.UserMessage{Content: []types.UserContent{
		types.FeedbackContent{
			TargetNodeID: string(targetNodeID),
			Rating:       rating,
			Comment:      comment,
		},
	}}

	return a.cfg.Tree.AddFeedback(ctx, targetNodeID, msg)
}

// FeedbackEntry is a single piece of feedback extracted from the tree.
type FeedbackEntry struct {
	NodeID       types.NodeID // the feedback node itself
	TargetNodeID types.NodeID // the node being rated
	Rating       types.Rating
	Comment      string
}

// FeedbackSummary collects all feedback entries across the entire tree.
func (a *Agent) FeedbackSummary() []FeedbackEntry {
	nodes := a.cfg.Tree.Feedback()

	var entries []FeedbackEntry
	for _, n := range nodes {
		um, ok := n.Message.(types.UserMessage)
		if !ok {
			continue
		}
		for _, c := range um.Content {
			if fb, ok := c.(types.FeedbackContent); ok {
				entries = append(entries, FeedbackEntry{
					NodeID:       n.ID,
					TargetNodeID: types.NodeID(fb.TargetNodeID),
					Rating:       fb.Rating,
					Comment:      fb.Comment,
				})
			}
		}
	}
	return entries
}

// Invoke starts the agent loop on the active branch and returns a stream of deltas.
// Input messages are appended as child nodes and all responses are persisted to the tree.
//
// One run may be active per branch of a tree at a time, across every Agent
// that shares the tree. A second Invoke or RunDurable on a busy branch
// returns a stream that ends at once with ErrRunActive; use Agent.Submit to
// add a message to the active run instead. Runs on different branches of one
// tree may overlap.
//
// While the run is active, EventStream.Submit and Agent.Submit add messages
// to it without cancelling it.
func (a *Agent) Invoke(ctx context.Context, input []types.Message, branch ...types.BranchID) *EventStream {
	b := a.cfg.Tree.Active()
	if len(branch) > 0 {
		b = branch[0]
	}
	stream, err := a.start(ctx, input, b)
	if err != nil {
		return failedStream(ctx, err)
	}
	return stream
}

// start claims branch and runs the loop on a new stream. The claim is taken
// before start returns, so a claim error is returned directly. The stream
// accepts submitted messages unless the agent has a durable step runner:
// a submitted message is not part of the runner's recorded input, so a
// replay after a crash would rebuild a different transcript.
func (a *Agent) start(ctx context.Context, input []types.Message, branch types.BranchID) (*EventStream, error) {
	return a.startSubmission(ctx, input, branch, nil)
}

// startSubmission is start for a run that a submitted message begins. The
// stream reports sub with QueuedDelta and InjectedDelta once its input is
// on the branch, as it does for messages that join an active run.
func (a *Agent) startSubmission(ctx context.Context, input []types.Message, branch types.BranchID, sub *Submission) (*EventStream, error) {
	if a.cfg.optionErr != nil {
		return nil, a.cfg.optionErr
	}
	ctx, cancel := context.WithCancel(ctx)
	stream := newEventStream(ctx, cancel)
	stream.started = sub
	var registered *EventStream
	if _, inline := a.cfg.StepRunner.(types.NoopStepRunner); inline {
		stream.inbox = &inbox{}
		stream.released = make(chan struct{})
		registered = stream
	}

	claim, err := newRunClaim(a.cfg.Tree, branch, registered)
	if err != nil {
		cancel()
		return nil, err
	}
	stream.claim = claim

	go func() {
		defer claim.release()
		a.runLoop(ctx, stream, input, branch, claim.release)
	}()
	return stream, nil
}

// RunDurable runs the loop without a streaming consumer. The runner controls
// replay and uncertain outcomes. Use a fresh Agent, tree, and budget for each
// reconstructed run; copying the Agent does not isolate its mutable state.
// Approvals require an ApprovalRunner because no consumer can resolve events;
// that includes approvals inside delegated sub-agents. Like Invoke, it returns
// ErrRunActive when another run is active on the branch.
func (a *Agent) RunDurable(ctx context.Context, runner types.StepRunner, input []types.Message, branch types.BranchID) (*types.AssistantMessage, error) {
	if a.cfg.optionErr != nil {
		return nil, a.cfg.optionErr
	}
	if runner == nil {
		runner = types.NoopStepRunner{}
	}
	clone := *a
	clone.cfg.StepRunner = runner
	if _, durableApprovals := runner.(types.ApprovalRunner); durableApprovals && clone.cfg.CompactCfg != nil && clone.cfg.CompactCfg.ToCompactor() != nil {
		return nil, errors.New("durable approval replay requires compaction checkpoints; automatic compaction is unsupported")
	}

	if branch == "" {
		branch = clone.cfg.Tree.Active()
	}
	claim, err := newRunClaim(clone.cfg.Tree, branch, nil)
	if err != nil {
		return nil, err
	}
	release := claim.release
	defer release()

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := newEventStream(loopCtx, cancel)
	stream.nonStreaming = true
	stream.claim = claim

	// Drain deltas in a separate goroutine so the loop's RunStep calls execute
	// synchronously in THIS goroutine: important for durable engines that
	// correlate steps to the workflow's calling context.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range stream.Deltas() {
		}
	}()

	clone.runLoop(loopCtx, stream, input, branch, release) // synchronous; closes stream
	<-done

	// The stream's close error is the authoritative terminal signal: in-band
	// ErrorDeltas can be dropped when the context is already done (send races
	// against ctx.Done), so a cancelled run must not be reported as success.
	if runErr := stream.Wait(); runErr != nil {
		return nil, runErr
	}

	// Return the last assistant message on the branch.
	msgs, err := clone.cfg.Tree.FlattenBranch(branch)
	if err != nil {
		return nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if am, ok := msgs[i].(types.AssistantMessage); ok {
			m := am
			return &m, nil
		}
	}
	return nil, nil
}

// ── Config resolution ────────────────────────────────────────────────

// resolvedConfig holds the effective configuration for a single iteration,
// derived by walking all ConfigContent blocks in the tree.
type resolvedConfig struct {
	model       string
	maxIter     int
	maxIterSet  bool // true once a ConfigContent block explicitly set MaxIter
	compactor   types.Compactor
	compactCfg  *types.CompactConfig
	compactNow  bool   // set by ConfigContent and cleared by the next assistant turn
	activeAgent string // last HandoffContent.To seen on the branch ("" = entry)
	// toolChoice is the latest ConfigContent.ToolChoice. A forced choice is
	// cleared by the next assistant turn, so it applies to one call.
	toolChoice *types.ToolChoice
	// dials merges every ConfigContent.Dials on the branch, in order.
	dials types.Dials
	// loop is the open tool loop with signed reasoning, if any.
	loop signedLoop
}

// prepareMessages resolves config and strips metadata in a single pass over the
// message history. This avoids the cost of two separate O(n) walks per iteration.
func (a *Agent) prepareMessages(messages []types.Message) (resolvedConfig, []types.Message) {
	rc := resolvedConfig{maxIter: a.cfg.MaxIter}
	if a.cfg.CompactCfg != nil {
		rc.compactor = a.cfg.CompactCfg.ToCompactor()
		rc.compactCfg = a.cfg.CompactCfg
	}

	out := make([]types.Message, 0, len(messages))
	for _, msg := range messages {
		switch v := msg.(type) {
		case types.SystemMessage:
			filtered := make([]types.SystemContent, 0, len(v.Content))
			for _, c := range v.Content {
				switch cv := c.(type) {
				case types.ConfigContent:
					mergeConfig(&rc, cv)
				case types.HandoffContent:
					rc.activeAgent = cv.To // resolve active agent; strip like ConfigContent
				default:
					if !types.IsMetadataContent(c) {
						filtered = append(filtered, c)
					}
				}
			}
			if len(filtered) > 0 {
				out = append(out, types.SystemMessage{Content: filtered})
			}
		case types.UserMessage:
			filtered := make([]types.UserContent, 0, len(v.Content))
			for _, c := range v.Content {
				switch cv := c.(type) {
				case types.ConfigContent:
					mergeConfig(&rc, cv)
				case types.HandoffContent:
					rc.activeAgent = cv.To // human-forced handoff; strip from LLM stream
				default:
					if !types.IsMetadataContent(c) {
						filtered = append(filtered, c)
					}
				}
			}
			if len(filtered) > 0 {
				rc.loop.observeUser(filtered)
				out = append(out, types.UserMessage{Content: filtered})
			}
		case types.AssistantMessage:
			rc.loop.observeAssistant(v, rc.dials)
			// One-shot controls set before this turn have been used.
			rc.compactNow = false
			if rc.toolChoice != nil && rc.toolChoice.Forced() {
				rc.toolChoice = nil
			}
			filtered := make([]types.AssistantContent, 0, len(v.Content))
			for _, c := range v.Content {
				if !types.IsMetadataContent(c) {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				out = append(out, types.AssistantMessage{Content: filtered})
			}
		default:
			out = append(out, msg)
		}
	}
	return rc, out
}

func mergeConfig(rc *resolvedConfig, cc types.ConfigContent) {
	if cc.Model != "" {
		rc.model = cc.Model
	}
	if cc.MaxIter != 0 {
		rc.maxIter = cc.MaxIter
		rc.maxIterSet = true
	}
	if cc.Compact != nil {
		rc.compactor = cc.Compact.ToCompactor()
		rc.compactCfg = cc.Compact
	}
	if cc.CompactNow {
		rc.compactNow = true
	}
	if cc.ToolChoice != nil {
		rc.toolChoice = cc.ToolChoice
	}
	if cc.Dials != nil {
		rc.dials = rc.dials.Merge(*cc.Dials)
	}
}

// persistCompacted forks a new branch off the tree root and adds the compacted
// messages (skipping the first, which is the system message already on root).
// Returns the new branch ID.
func (a *Agent) persistCompacted(ctx context.Context, tr *tree.Tree, compacted []types.Message) (types.BranchID, error) {
	root := tr.Root()
	if len(compacted) < 2 {
		return "", fmt.Errorf("compacted history too short to branch")
	}

	// First compacted message is the system prompt (same as root): skip it.
	// Branch from root with the second message (the summary request); the rest
	// (assistant summary + preserved recent context) are appended below.
	branchID, _, err := tr.Branch(ctx, root.ID, "compact", compacted[1])
	if err != nil {
		return "", fmt.Errorf("branch from root: %w", err)
	}

	// Add remaining compacted messages (the preserved recent context).
	for _, msg := range compacted[2:] {
		if _, err := tr.AddChildOnBranch(ctx, branchID, msg); err != nil && !errors.Is(err, tree.ErrWALCommit) {
			return "", fmt.Errorf("add compacted child: %w", err)
		}
	}

	// Set the compacted branch as active so future Invoke calls use it.
	if err := tr.SetActiveContext(ctx, branchID); err != nil && !errors.Is(err, tree.ErrWALCommit) {
		return "", fmt.Errorf("set active: %w", err)
	}

	return branchID, nil
}

// callProvider invokes the given provider's LLM, using structured output when
// available. The provider is passed explicitly so the agent loop can swap it
// per-iteration during a handoff.
//
// opts carries per-request controls (a tool choice) and is non-nil only when
// the provider implements types.OptionsProvider.
//
// out is the response schema in force, resolved by the caller from the run
// context. A durable StepRunner may hand the step a context that does not
// descend from the run's, so the schema is not looked up again here.
func (a *Agent) callProvider(ctx context.Context, provider types.Provider, out runOutput, messages []types.Message, tools []types.ToolDef, opts *types.RequestOptions) (<-chan types.Delta, error) {
	if out.native() && len(tools) == 0 && opts != nil && opts.ToolChoice != nil && opts.ToolChoice.Mode == types.ToolChoiceNone {
		// A "none" tool choice with no tools changes nothing, and the
		// options path cannot carry the response schema.
		opts = nil
	}
	if op, ok := provider.(types.OptionsProvider); ok && opts != nil {
		return op.ChatStreamWithOptions(ctx, messages, tools, *opts)
	}
	if out.native() && len(tools) == 0 {
		if err := checkStructuredOutput(provider); err != nil {
			return nil, err
		}
		if sp, ok := provider.(types.StructuredOutputProvider); ok {
			return sp.ChatStreamWithSchema(ctx, messages, tools, out.schema)
		}
	}
	return provider.ChatStream(ctx, messages, tools)
}

// checkStructuredOutput rejects a response schema the provider cannot apply.
// A known model that declares no structured output, or whose adapter has
// withdrawn it for the current configuration, is refused. In every case
// the provider must implement types.StructuredOutputProvider, because that is
// the only path that sends the schema; a schema that would be dropped
// silently is a configuration error.
func checkStructuredOutput(provider types.Provider) error {
	if mc, ok := types.ProviderCapabilities(provider); ok && mc.Known &&
		(mc.StructuredOutput == types.StructuredOutputNone || !mc.Supports(types.CapStructuredOutput)) {
		return fmt.Errorf("%w: response schema: model %q declares no structured output",
			types.ErrInvalidModelConfig, types.ProviderModel(provider))
	}
	if _, ok := provider.(types.StructuredOutputProvider); !ok {
		return fmt.Errorf("%w: response schema: provider %q does not support structured output",
			types.ErrInvalidModelConfig, types.ProviderName(provider))
	}
	return nil
}

// satisfiesResponseSchema reports whether an assistant turn's text is already
// a JSON object that passes the shallow schema check, so no extra
// schema-constrained turn is needed.
func satisfiesResponseSchema(schema *types.ParameterSchema, msg *types.AssistantMessage) bool {
	var text strings.Builder
	for _, block := range msg.Content {
		if tc, ok := block.(types.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text.String())), &obj); err != nil {
		return false
	}
	return types.ValidateToolArgs(*schema, obj) == nil
}

// ── File resolution ──────────────────────────────────────────────────

// resolveFiles walks messages and resolves FileContent blocks with empty Data.
// For each FileContent, it resolves the URI via scheme-matched Resolver, then
// checks the provider's ContentNegotiator: if the media type is native, the
// FileContent is kept; otherwise, it is converted via an Extractor.
//
// A file that cannot be loaded is never passed on silently. A missing
// resolver (for a scheme other than http or https, which providers can fetch
// themselves), a resolver error, or an extractor error for a type the
// provider cannot read is logged and replaced with a text notice, so the
// model knows the file is missing and raw bytes never reach it as text.
func (a *Agent) resolveFiles(ctx context.Context, messages []types.Message) []types.Message {
	// Determine native content support from the provider. This prefers the
	// model-level capability declaration over the adapter-level negotiator: an
	// adapter knows how to encode an image, but only the model decides whether
	// it can read one.
	support := types.ProviderContentSupport(a.cfg.Provider)

	out := make([]types.Message, 0, len(messages))
	for _, msg := range messages {
		um, ok := msg.(types.UserMessage)
		if !ok {
			out = append(out, msg)
			continue
		}

		var replaced []types.UserContent
		for _, c := range um.Content {
			fc, ok := c.(types.FileContent)
			if !ok || len(fc.Data) > 0 {
				replaced = append(replaced, c)
				continue
			}

			// Extract URI scheme.
			scheme := uriScheme(fc.URI)
			resolver, found := a.cfg.Resolvers[scheme]
			if !found {
				if scheme == "http" || scheme == "https" {
					replaced = append(replaced, c) // providers can fetch web URLs
					continue
				}
				replaced = append(replaced, a.fileNotice(fc, fmt.Sprintf("no resolver for scheme %q", scheme)))
				continue
			}

			resolved, err := resolver.Resolve(ctx, fc.URI)
			if err != nil {
				replaced = append(replaced, a.fileNotice(fc, err.Error()))
				continue
			}

			fc.Data = resolved.Data
			if fc.MediaType == "" {
				fc.MediaType = resolved.MediaType
			}

			// Check if provider handles this type natively.
			if support.Supports(fc.MediaType) {
				replaced = append(replaced, fc)
				continue
			}

			// Try to extract to text content blocks.
			if ext, ok := a.cfg.Extractors[fc.MediaType]; ok {
				blocks, err := ext.Extract(ctx, fc.Data, fc.MediaType)
				if err == nil {
					replaced = append(replaced, blocks...)
					continue
				}
				replaced = append(replaced, a.fileNotice(fc, "extract "+string(fc.MediaType)+": "+err.Error()))
				continue
			}

			// Neither the provider nor an extractor can read this type. Raw
			// bytes are never forwarded as text, so the file becomes a notice.
			replaced = append(replaced, a.fileNotice(fc,
				"media type "+string(fc.MediaType)+" is not supported by the provider and no extractor is registered"))
		}

		out = append(out, types.UserMessage{Content: replaced})
	}
	return out
}

// fileNotice logs a file that could not be loaded and returns the text block
// that stands in for it.
func (a *Agent) fileNotice(fc types.FileContent, reason string) types.UserContent {
	a.cfg.Logger.Warn("file could not be loaded",
		"agent", a.cfg.Name, "uri", fc.URI, "media_type", fc.MediaType, "error", reason)
	return types.TextContent{Text: fmt.Sprintf("[file %s could not be loaded: %s]", fc.URI, reason)}
}

// uriScheme extracts the scheme from a URI (e.g. "file" from "file:///path").
func uriScheme(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" {
		return ""
	}
	return u.Scheme
}

// ── Store persistence ────────────────────────────────────────────────

// persistNode writes a freshly-added node and its branch tip to the configured
// Store. It is best-effort: a nil Store is a no-op, and any error is logged but
// never propagated, so persistence failures cannot break a live agent run. When
// the Store exposes a transaction, the node + branch tip are committed together
// so a reader never observes a tip pointing at an unsaved node.
func (a *Agent) persistNode(ctx context.Context, node *types.Node) {
	if a.cfg.Store == nil || node == nil {
		return
	}
	// The node already exists in the tree, so the write must land even when
	// the run was cancelled, within the same bound the tree uses.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tree.DefaultPersistTimeout)
	defer cancel()
	err := a.cfg.Store.Tx(ctx, func(tx types.StoreTx) error {
		if err := tx.SaveNode(ctx, node); err != nil {
			return err
		}
		return tx.SaveBranch(ctx, node.BranchID, node.ID)
	})
	if err != nil {
		a.cfg.Logger.Warn("store persist failed",
			"agent", a.cfg.Name, "node", node.ID, "branch", node.BranchID, "error", err)
	}
}

// Checkpoint creates a named checkpoint at the tip of branch (the active
// branch when empty) and persists it to the configured Store, so it survives
// LoadTreeFromStore round trips without WAL recovery. Callers that call
// Tree.Checkpoint directly get WAL coverage only: this is the durable path.
// The checkpoint always exists in the tree on return; a non-nil error means
// only the store write failed.
func (a *Agent) Checkpoint(ctx context.Context, branch types.BranchID, name string) (types.CheckpointID, error) {
	tr := a.cfg.Tree
	if branch == "" {
		branch = tr.Active()
	}
	cpID, err := tr.Checkpoint(branch, name)
	if err != nil {
		return "", err
	}
	if a.cfg.Store != nil {
		cp, ok := tr.Checkpoints()[cpID]
		if !ok {
			return cpID, fmt.Errorf("checkpoint %s missing after creation", cpID)
		}
		if err := a.cfg.Store.SaveCheckpoint(ctx, cp); err != nil {
			return cpID, fmt.Errorf("persist checkpoint: %w", err)
		}
	}
	return cpID, nil
}

// LoadTreeFromStore reconstructs a conversation tree from a Store by loading the
// subtree rooted at rootID (see tree.LoadFromStore). The returned tree can be
// passed to NewAgent via WithTree to resume a persisted session; pass
// tree.WithStore(store) in opts so later changes persist too. When active is
// empty, the store's saved active branch is used if it has one (so a session
// reloads onto its compacted branch), otherwise "main".
//
// This is the read counterpart to WithStore's write path. It is a free function
// (not a method) so a tree can be hydrated before an Agent exists.
func LoadTreeFromStore(ctx context.Context, store types.Store, rootID types.NodeID, active types.BranchID, opts ...tree.Option) (*tree.Tree, error) {
	if store == nil {
		return nil, fmt.Errorf("agent: nil store")
	}
	return tree.LoadFromStore(ctx, store, rootID, active, opts...)
}

// RecoverAndLoadTree heals the store from any write-ahead-log transactions
// that committed but were never applied (e.g. a crash between WAL commit and
// store write), then hydrates the tree. Call it instead of LoadTreeFromStore
// on startup when both a WAL and a Store are configured. The recovered tree
// keeps writing to the same WAL for subsequent mutations.
func RecoverAndLoadTree(ctx context.Context, wal types.WAL, store types.Store, rootID types.NodeID, active types.BranchID) (*tree.Tree, error) {
	var opts []tree.Option
	if wal != nil {
		if _, err := walrecover.RecoverWAL(ctx, wal, store); err != nil {
			return nil, fmt.Errorf("recover wal: %w", err)
		}
		opts = append(opts, tree.WithWAL(wal))
	}
	return LoadTreeFromStore(ctx, store, rootID, active, opts...)
}

// ── Run loop ─────────────────────────────────────────────────────────

// runLoop runs the agent loop and closes stream when it ends.
//
// release, when non-nil, frees the branch claim. It runs before the stream
// reports completion, so a caller that sees DoneDelta or returns from Wait
// can start the next run on the same branch at once.
func (a *Agent) runLoop(ctx context.Context, stream *EventStream, input []types.Message, branch types.BranchID, release func()) {
	log := a.cfg.Logger
	start := time.Now()
	log.Debug("agent loop started", "agent", a.cfg.Name, "branch", branch)
	endSpan := func(error) {}
	if a.cfg.RunTracer != nil {
		a.safely("run tracer start", func() {
			traced, end := a.cfg.RunTracer.StartAgent(ctx, a.cfg.Name)
			if traced != nil && end != nil {
				ctx, endSpan = traced, end
			}
		})
	}

	if a.spawning {
		stream.spawns = newSpawnRegistry()
	}
	var loopErr error
	defer func() {
		// Background children end with the run, before the stream closes,
		// so none of their deltas can reach a closed stream.
		stream.spawns.shutdown()
		// No safe point follows, so later submissions are refused rather
		// than lost; accepted ones remain in Undelivered.
		stream.inbox.close()
		stream.flushAcks()
		if release != nil {
			release()
		}
		// A terminal error is delivered on both channels on purpose: as an
		// ErrorDelta so channel consumers see it in-band, and as the stream's
		// close error so Wait() reports the same failure.
		if loopErr != nil {
			stream.send(types.ErrorDelta{Error: loopErr})
		}
		// The run's span and metrics are recorded before the stream reports
		// completion, so a caller that returns from Wait sees them.
		a.safely("run tracer end", func() { endSpan(loopErr) })
		a.safely("run metrics", func() {
			if r, ok := a.cfg.Metrics.(types.AgentOutcomeRecorder); ok {
				r.RecordAgentOutcome(ctx, a.cfg.Name, time.Since(start), loopErr)
			} else {
				a.cfg.Metrics.RecordAgentInvocation(ctx, a.cfg.Name, time.Since(start))
			}
		})
		stream.send(types.DoneDelta{})
		stream.close(loopErr)
		log.Debug("agent loop finished", "agent", a.cfg.Name, "elapsed", time.Since(start))
	}()

	stream.branch = branch
	loopErr = a.runRecovered(ctx, stream, input, branch)
}

// runRecovered runs the loop and turns a panic in it, such as one raised by
// a caller-supplied ToolPolicy, Compactor, OutcomePolicy, or tracer, into the
// run's error. The stream then closes with that error, background children
// shut down, and the branch claim is released, instead of the process
// crashing. Tool calls the panic left unanswered get error results, so the
// branch keeps every tool call paired with its result.
func (a *Agent) runRecovered(ctx context.Context, stream *EventStream, input []types.Message, branch types.BranchID) (err error) {
	defer func() {
		if p := recover(); p != nil {
			a.cfg.Logger.Error("agent run panic recovered",
				"agent", a.cfg.Name, "branch", stream.branch, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("agent run panicked: %v", p)
			a.answerOpenToolCalls(context.WithoutCancel(ctx), stream.branch, err.Error())
		}
	}()
	err = a.run(ctx, stream, input, branch)
	return a.captureSubAgent(ctx, stream, err)
}

// answerOpenToolCalls records reason as the error result of every tool call
// in the branch's last message when that message is an assistant turn whose
// calls have no results yet. It is best effort: a failure is logged.
func (a *Agent) answerOpenToolCalls(ctx context.Context, branch types.BranchID, reason string) {
	defer func() {
		if p := recover(); p != nil {
			a.cfg.Logger.Error("answering open tool calls panicked", "agent", a.cfg.Name, "panic", p)
		}
	}()
	msgs, err := a.cfg.Tree.FlattenBranch(branch)
	if err != nil || len(msgs) == 0 {
		return
	}
	last, ok := msgs[len(msgs)-1].(types.AssistantMessage)
	if !ok {
		return
	}
	calls := assistantToolCalls(&last)
	if len(calls) == 0 {
		return
	}
	results := make([]toolResult, len(calls))
	for i, c := range calls {
		results[i] = toolResult{toolCallID: c.ID, err: reason}
	}
	if err := a.persistToolResults(ctx, a.cfg.Tree, branch, results); err != nil {
		a.cfg.Logger.Error("recording results for open tool calls failed", "agent", a.cfg.Name, "branch", branch, "error", err)
	}
}

// safely runs fn, a call into caller-supplied code at the edge of a run, and
// logs a panic from it instead of letting it end the process.
func (a *Agent) safely(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			a.cfg.Logger.Error("panic recovered", "agent", a.cfg.Name, "in", what, "panic", p, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// RunTracer opens a span around one agent run. StartAgent returns the
// context the run uses, so the run's provider and tool spans become children,
// and end, which the agent calls once with the run's terminal error (nil on
// a clean finish).
type RunTracer interface {
	StartAgent(ctx context.Context, name string) (context.Context, func(err error))
}

// run executes the agent loop and returns its terminal error (nil on a clean
// finish). runLoop turns that error into the stream's ErrorDelta + close error.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Agent) run(ctx context.Context, stream *EventStream, input []types.Message, branch types.BranchID) error {
	if err := a.validateDurableConfiguration(); err != nil {
		return err
	}
	out := a.output(ctx)
	if err := a.checkOutput(a.cfg.Provider, out); err != nil {
		return err
	}
	log := a.cfg.Logger
	tr := a.cfg.Tree

	if err := a.appendInput(ctx, tr, branch, input); err != nil {
		return err
	}
	if err := a.reportStarted(stream, tr, branch); err != nil {
		return err
	}
	if a.cfg.ApprovalPolicy != nil {
		// The conversation's grants and counts are rebuilt from the records
		// on its branch, so a restored or replayed run decides alike.
		msgs, err := tr.FlattenBranch(branch)
		if err != nil {
			return err
		}
		stream.approvals = newApprovalState(msgs)
	}

	// scopeBranch keys per-conversation policy state. It stays the branch the
	// run started on even after compaction moves the run elsewhere.
	scopeBranch := branch
	// scopeConversation separates trees that share an agent name and a
	// branch name: every tree's root ID is random.
	var scopeConversation types.NodeID
	if root := tr.Root(); root != nil {
		scopeConversation = root.ID
	}

	var (
		handoffCount int
		iterCount    int // completed model turns; compactions and retries do not count
		overflow     overflowState
		guards       loopGuards
		// forcedSpent records that the configured forced tool choice has
		// been used, so it applies to one turn per run.
		forcedSpent bool
	)

	// pendingWork is true when the previous iteration executed tool calls and the
	// loop continued, meaning the assistant still owes a turn to consume those
	// results. If the iteration cap then fires, the run was truncated mid-task and
	// we emit ErrMaxIterations so consumers can tell "finished" from "cut off".
	pendingWork := false
	// autoContinues counts the continuations WithAutoContinue has used.
	autoContinues := 0
	// outputRepairs counts, per user turn, the final text answers sent back
	// because they did not match the response schema.
	outputRepairs := 0
	// turnStart is iterCount when the current user turn began. Step limits
	// count from it; step names keep using iterCount so they never repeat.
	turnStart := 0

	// finish runs where the run would end naturally. Submitted messages
	// waiting there start a new user turn with fresh step limits instead.
	finish := func() (bool, error) {
		resumed, err := a.resumeAtFinish(ctx, stream, tr, branch)
		if resumed && err == nil {
			turnStart, pendingWork, guards, outputRepairs = iterCount, false, loopGuards{}, 0
		}
		return resumed, err
	}
	// truncated decides what follows a turn the output limit cut short:
	// end the run with err, or resume it with WithAutoContinue.
	truncated := func(provider types.Provider, err error) (bool, error) {
		if err != nil || autoContinues >= a.cfg.AutoContinue {
			return false, err
		}
		autoContinues++
		pendingWork = true
		return true, a.appendContinuation(ctx, tr, branch, provider)
	}

	for {
		select {
		case <-ctx.Done():
			return types.ErrStreamCanceled
		default:
		}

		// Safe point: every tool_use on the branch has its result, so
		// steering and interrupting messages can be appended here.
		interrupted, err := a.injectAtSafePoint(ctx, stream, tr, branch)
		if err != nil {
			return err
		}
		if interrupted {
			turnStart, pendingWork, guards = iterCount, false, loopGuards{}
		}

		// Flatten the branch to get current message history.
		messages, err := tr.FlattenBranch(branch)
		if err != nil {
			return err
		}

		// Resolve config + active agent (handoff group only). With no group this
		// is byte-for-byte the non-handoff path.
		resolved, llmMessages := a.prepareMessages(messages)
		active := a.applyModel(a.resolveActive(&resolved, llmMessages), resolved.model)
		active, err = a.selectHandoffContext(ctx, active, messages)
		if err != nil {
			return err
		}
		// Tool policies and tools see which owner and conversation the
		// turn belongs to.
		scoped := WithRunScope(ctx, RunScope{Agent: active.name, Conversation: scopeConversation, Branch: scopeBranch})
		active, err = a.selectTools(scoped, active)
		if err != nil {
			return err
		}
		active = a.hideDenied(stream, active)
		// The final_answer tool and a prompted schema apply to every turn
		// and every member, after tool selection, so a policy cannot hide
		// the way the run ends.
		active = withOutputTool(active, out)
		active.messages = withSchemaInstruction(active.messages, out)
		active.dialLayers = dialLayers(active, resolved)

		// Check the step limits. If the cap fires while the last assistant turn
		// left tool calls pending (pendingWork), the run was truncated, not
		// finished: OnMaxIter decides between ErrMaxIterations and a forced
		// final answer. A clean natural finish (text-only turn, empty
		// response) clears pendingWork and does NOT emit it. The loop guards
		// trip only after a turn's tool results are recorded, so they also
		// leave work pending.
		limitErr := guards.tripped
		if iterCount-turnStart >= resolved.maxIter {
			if !pendingWork {
				if resumed, err := finish(); resumed || err != nil {
					if err != nil {
						return err
					}
					continue
				}
				break
			}
			limitErr = types.ErrMaxIterations
		}
		if limitErr != nil {
			return a.finishAtLimit(ctx, stream, tr, branch, active, limitErr)
		}

		if a.handoffs != nil && resolved.compactor != nil {
			return errors.New("handoff context compaction requires per-owner checkpoints; automatic compaction is unsupported")
		}
		// Resolve file URIs to data.
		active.messages = a.resolveFiles(ctx, active.messages)

		// Compact if configured: summarize or trim onto a new branch, then
		// re-flatten. A turn is compacted once before it is sent, or again
		// while it is still over MaxInputTokens and the last compaction shrank
		// it. Compaction does not count as an iteration.
		if overflow.canCompact() {
			newBranch, compacted, err := a.compactIfNeeded(ctx, stream, &overflow, resolved, active, tr, branch)
			if err != nil {
				return err
			}
			if compacted {
				// The run now writes newBranch, which the tree also makes
				// active. Claim it before writing, so a submission or run
				// addressed to it joins this run instead of starting another.
				if err := stream.claim.add(newBranch); err != nil {
					return err
				}
				branch = newBranch
				stream.branch = branch
				if err := a.carryApprovals(ctx, stream, tr, branch); err != nil {
					return err
				}
				continue
			}
		}

		if a.cfg.Budget != nil {
			if err := a.cfg.Budget.Err(); err != nil {
				return err
			}
		}
		choice := a.toolChoice(resolved, forcedSpent)
		toolDefs, opts, err := toolChoiceRequest(active.provider, choice, active.toolDefs)
		if err != nil {
			return err
		}
		if opts, err = a.attachDials(ctx, active, opts, toolDefs); err != nil {
			return err
		}
		// Get the assistant message as a durable step (provider call + aggregation).
		stepName := fmt.Sprintf("llm-%s-%d", branch, iterCount)
		// The call runs under its own cancel, so SubmitInterruptReplace can
		// stop it without ending the run.
		turnCtx, cancelTurn := context.WithCancelCause(ctx)
		stream.inbox.beginTurn(cancelTurn)
		msg, usage, llmErr := a.getAssistantMessage(turnCtx, stream, active.provider, active.messages, toolDefs, opts, stepName)
		interruptedBy := stream.inbox.endTurn()
		cancelTurn(nil)
		if errors.Is(llmErr, errInterruptRequested) && ctx.Err() == nil {
			// The interrupted call counts as a turn, so its step name is
			// not reused and a durable runner replays its partial result.
			iterCount++
			// The stopped call still used tokens: report them like any
			// turn's, with or without a budget.
			if err := a.reportUsage(ctx, stream, active.provider, usage); err != nil {
				return err
			}
			if err := a.commitInterrupted(ctx, stream, tr, branch, msg, interruptedBy); err != nil {
				return err
			}
			continue
		}
		if llmErr == nil && interruptedTurn(msg) {
			// A durable runner replayed a turn that was stopped when it
			// was recorded. It is committed the same way again.
			iterCount++
			if err := a.commitInterrupted(ctx, stream, tr, branch, msg, ""); err != nil {
				return err
			}
			continue
		}
		if llmErr != nil {
			// A context-length error is answered by compacting and retrying
			// the same turn on the compacted branch.
			newBranch, retry, err := a.recoverOverflow(ctx, stream, &overflow, llmErr, resolved, active, tr, branch)
			if retry {
				// The run now writes newBranch, which the tree also makes
				// active. Claim it before writing, so a submission or run
				// addressed to it joins this run instead of starting another.
				if err := stream.claim.add(newBranch); err != nil {
					return err
				}
				branch = newBranch
				stream.branch = branch
				if err := a.carryApprovals(ctx, stream, tr, branch); err != nil {
					return err
				}
				continue
			}
			log.Error("provider call failed", "error", err, "iteration", iterCount)
			return err
		}
		overflow.turnSucceeded(usage)
		iterCount++
		if choice != nil && choice == a.cfg.ToolChoice && choice.Forced() {
			forcedSpent = true
		}

		if err := a.reportUsage(ctx, stream, active.provider, usage); err != nil {
			return err
		}
		if done, err := a.handleTruncation(ctx, stream, tr, branch, msg, usage); done {
			if resume, err := truncated(active.provider, err); resume || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			if resumed, err := finish(); resumed || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			return nil
		}

		// A response schema that could not ride along with the tools is
		// applied to the final answer: a draft that ends the run without
		// tool calls is replaced by one schema-constrained, tool-free turn.
		if a.needsSchemaTurn(out, msg, toolDefs) {
			msg, usage, llmErr = a.getAssistantMessage(ctx, stream, active.provider, active.messages, nil, nil, stepName+"-schema")
			if llmErr != nil {
				log.Error("structured output call failed", "error", llmErr, "iteration", iterCount)
				return llmErr
			}
			if err := a.reportUsage(ctx, stream, active.provider, usage); err != nil {
				return err
			}
			if done, err := a.handleTruncation(ctx, stream, tr, branch, msg, usage); done {
				if resume, err := truncated(active.provider, err); resume || err != nil {
					if err != nil {
						return err
					}
					continue
				}
				if resumed, err := finish(); resumed || err != nil {
					if err != nil {
						return err
					}
					continue
				}
				return nil
			}
		}

		if msg == nil {
			// An empty response with a dead context is a truncated stream (the
			// provider's channel closed on cancellation), not a finished turn.
			if ctx.Err() != nil {
				return types.ErrStreamCanceled
			}
			// Empty response: a clean (if degenerate) finish.
			if resumed, err := finish(); resumed || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			break
		}

		// Persist assistant message to tree.
		if err := a.appendToBranch(ctx, tr, branch, *msg); err != nil {
			return err
		}

		toolCalls := assistantToolCalls(msg)
		if len(toolCalls) == 0 {
			// In tool and prompt mode the schema only asks for a shape, so
			// a text answer is checked here. A mismatch goes back to the
			// model with the error, a bounded number of times.
			if cause := out.textAnswerError(msg); cause != nil {
				if outputRepairs >= maxOutputRepairs {
					return fmt.Errorf("%w after %d attempts: %w", ErrSchemaInvalid, outputRepairs+1, cause)
				}
				outputRepairs++
				pendingWork = true
				if err := a.appendToBranch(ctx, tr, branch, repairMessage(out.mode, cause)); err != nil {
					return err
				}
				continue
			}
			// Text-only turn: the assistant finished naturally. Queued
			// messages are appended here and the same stream continues.
			if resumed, err := finish(); resumed || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			break
		}

		// A turn that repeats the previous calls exactly is answered without
		// running them, and the run stops at the next iteration.
		if guards.repeated(toolCalls, a.cfg.MaxRepeatIterations) {
			results := skipToolCalls(stream, toolCalls, "not run: "+guards.tripped.Error())
			pendingWork = true
			if err := a.persistToolResults(ctx, tr, branch, results); err != nil {
				return err
			}
			continue
		}

		// Execute all tool calls using the ACTIVE agent's tool registry. The
		// assistant now owes a turn to consume those results (pendingWork), so the
		// iteration cap can distinguish a truncated run from a clean finish.
		// Persist results BEFORE any handoff so every tool_use gets a matching
		// tool_result (provider contract) and rich Blocks are persisted.
		results := a.executeToolsConcurrently(scoped, stream, toolCalls, active.tools)
		if err := stream.runError(); err != nil {
			// Answer every tool_use before stopping so the branch stays a valid
			// transcript. A suspended durable run is the exception: its
			// pending calls are resumed later, not answered now.
			if !errors.Is(err, types.ErrSuspended) {
				if perr := a.persistToolResults(ctx, tr, branch, results); perr != nil {
					log.Warn("failed to record tool results for a stopped run", "error", perr)
				}
			}
			return err
		}
		pendingWork = true
		if err := a.persistToolResults(ctx, tr, branch, results); err != nil {
			return err
		}

		// A successful call to a stop tool, or to final_answer, ends the run
		// with its result.
		if id := a.stopToolCall(toolCalls, results, out); id != "" {
			stream.stopToolCallID = id
			return nil
		}
		// A failed delegation may move this agent to another model before
		// its next turn. The switch is recorded only now, after every
		// tool_use has its result.
		if err := a.observeSubAgentFailures(ctx, stream, tr, branch, active.provider, results); err != nil {
			return err
		}
		guards.recordResults(results, a.cfg.MaxConsecutiveErrors)

		// Handoff post-check: first signal wins; self-handoff is a no-op. On a
		// handoff the next iteration re-flattens and resolves the new active
		// agent; with no handoff it re-flattens to process the tool results: so
		// both simply fall through to the next iteration.
		if err := a.applyHandoff(ctx, tr, stream, branch, results, active.name, &handoffCount); err != nil {
			return err
		}
	}
	return nil
}

// appendInput appends input messages as child nodes on the branch. Returns the
// first tree-write error, which terminates the run.
func (a *Agent) appendInput(ctx context.Context, tr *tree.Tree, branch types.BranchID, input []types.Message) error {
	for _, msg := range input {
		if err := a.appendToBranch(ctx, tr, branch, msg); err != nil {
			return err
		}
	}
	return nil
}

// appendToBranch adds msg at the tip of branch and persists the new node.
// Every append in the run loop goes through here. Reading the tip and adding
// the child is one tree operation, so the message always extends the run's
// own branch, even when another branch (such as one rewound to a
// checkpoint) ends at the same node.
func (a *Agent) appendToBranch(ctx context.Context, tr *tree.Tree, branch types.BranchID, msg types.Message) error {
	_, err := a.appendNode(ctx, tr, branch, msg)
	return err
}

// appendNode is appendToBranch for callers that need the new node.
//
// A node returned with an error wrapping tree.ErrWALCommit is held by the
// tree and the store; only the write-ahead log missed it. That is logged and
// the run continues.
func (a *Agent) appendNode(ctx context.Context, tr *tree.Tree, branch types.BranchID, msg types.Message) (*types.Node, error) {
	node, err := tr.AddChildOnBranch(ctx, branch, msg)
	if err != nil {
		if node == nil || !errors.Is(err, tree.ErrWALCommit) {
			return nil, err
		}
		a.cfg.Logger.Warn("tree WAL commit failed after the store write; continuing",
			"agent", a.cfg.Name, "node", node.ID, "branch", branch, "error", err)
	}
	a.persistNode(ctx, node)
	return node, nil
}

// reportUsage streams a turn's usage and charges it to the budget. The budget
// is enforced on every usage report, not once per iteration: a single
// long-context call can cost more than the whole allowance.
func (a *Agent) reportUsage(ctx context.Context, stream *EventStream, provider types.Provider, usage *types.UsageDelta) error {
	// Emit enriched usage delta (carries CacheHit + response metadata + latency).
	enriched := types.UsageDelta{}
	if usage != nil {
		enriched = *usage
	}
	stream.send(enriched)
	return a.chargeBudget(ctx, stream, provider, enriched)
}

// needsSchemaTurn reports whether a turn sent with tools (and therefore
// without the response schema) ended the run with an answer that does not
// already satisfy the schema.
func (a *Agent) needsSchemaTurn(out runOutput, msg *types.AssistantMessage, toolDefs []types.ToolDef) bool {
	if !out.native() || len(toolDefs) == 0 || msg == nil {
		return false
	}
	if len(assistantToolCalls(msg)) > 0 {
		return false
	}
	return !satisfiesResponseSchema(out.schema, msg)
}

// activeContext is the per-iteration agent selection (provider, tools, persona).
type activeContext struct {
	provider types.Provider
	toolDefs []types.ToolDef
	tools    *types.ToolRegistry
	name     string
	messages []types.Message
	// dials and dialScope are the active agent's own dials: the entry
	// agent's, or a handoff member's when it sets its own.
	dials     types.Dials
	dialScope string
	// dialLayers are every dial layer of the next call, turn included.
	dialLayers []types.DialLayer
}

// resolveActive selects the active agent for this iteration and overlays its
// persona. With no handoff group it returns the entry agent's config unchanged.
func (a *Agent) resolveActive(resolved *resolvedConfig, llmMessages []types.Message) activeContext {
	ac := activeContext{
		provider: a.cfg.Provider,
		toolDefs: a.tools.Definitions(),
		tools:    a.tools,
		name:     a.cfg.Name,
		messages: llmMessages,

		dials:     a.cfg.Dials,
		dialScope: types.DialScopeAgent,
	}
	member := a.activeMember(resolved.activeAgent)
	if member == nil {
		return ac
	}
	if member.dials != nil {
		ac.dials, ac.dialScope = member.dials.Clone(), types.DialScopeMember
	}
	ac.provider = member.provider
	ac.tools = member.tools
	ac.toolDefs = member.tools.Definitions()
	ac.name = member.name
	if member.maxIter > 0 && !resolved.maxIterSet {
		resolved.maxIter = member.maxIter
	}
	if member.systemPrompt != "" {
		ac.messages = overlaySystem(llmMessages, member.systemPrompt)
	}
	return ac
}

// applyModel re-targets the active provider when a ConfigContent block set a
// model. A provider that cannot switch (no types.ModelSwitcher) is used
// unchanged, with a warning so a requested model is never dropped silently.
func (a *Agent) applyModel(ac activeContext, model string) activeContext {
	if model == "" {
		return ac
	}
	switched := types.ProviderWithModel(ac.provider, model)
	// A session provider (such as a router session) records a pin only when
	// WithModel is called, even when it already reports the requested model,
	// so the switch is never skipped for one.
	if _, session := ac.provider.(types.SessionProvider); session {
		if ms, ok := ac.provider.(types.ModelSwitcher); ok {
			switched = ms.WithModel(model)
		}
	}
	if switched == ac.provider && types.ProviderModel(ac.provider) != model {
		a.cfg.Logger.Warn("config requested a model but the provider cannot switch models",
			"agent", a.cfg.Name, "requested_model", model, "provider", types.ProviderName(ac.provider))
	}
	ac.provider = switched
	return ac
}

// assistantToolCalls extracts the tool-use blocks from an assistant message.
func assistantToolCalls(msg *types.AssistantMessage) []types.ToolUseContent {
	var calls []types.ToolUseContent
	for _, block := range msg.Content {
		if tc, ok := block.(types.ToolUseContent); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// persistToolResults builds and persists the combined tool-result message.
func (a *Agent) persistToolResults(ctx context.Context, tr *tree.Tree, branch types.BranchID, results []toolResult) error {
	contents := make([]types.ToolResultContent, len(results))
	for i, r := range results {
		trc := types.ToolResultContent{ToolCallID: r.toolCallID, Text: r.result, Blocks: r.blocks, ToolVersion: r.version}
		if r.err != "" {
			trc.IsError = true
			if trc.Text == "" {
				trc.Text = r.err
			}
		}
		contents[i] = trc
	}
	msg := types.NewToolResultMessage(contents...)
	for _, r := range results {
		for _, rec := range r.approvals {
			msg.Content = append(msg.Content, rec)
		}
	}
	return a.appendToBranch(ctx, tr, branch, msg)
}

// applyHandoff applies the first handoff signal in results, if any, appending a
// HandoffContent overlay on the same branch. It returns a non-nil error when the
// caller should terminate the run: the handoff limit was exceeded or a tree
// write failed.
func (a *Agent) applyHandoff(ctx context.Context, tr *tree.Tree, stream *EventStream, branch types.BranchID, results []toolResult, activeName string, handoffCount *int) error {
	handoffTo := ""
	reason, message, handoffCtx := "", "", ""
	for _, r := range results {
		if r.handoffTo != "" {
			handoffTo = r.handoffTo
			reason, message, handoffCtx = r.handoffReason, r.handoffMessage, r.handoffContext
			break
		}
	}
	if handoffTo == "" || handoffTo == activeName {
		return nil
	}
	// Enforce the bound BEFORE any side effect, so the over-limit transfer is
	// neither streamed nor persisted (which would poison the branch).
	if *handoffCount >= a.cfg.MaxHandoffs {
		return ErrHandoffLimitExceeded
	}
	*handoffCount++
	stream.send(types.HandoffDelta{From: activeName, To: handoffTo, Reason: reason})
	overlay := types.SystemMessage{Content: []types.SystemContent{
		types.HandoffContent{From: activeName, To: handoffTo, Reason: reason, Message: message, Context: handoffCtx},
	}}
	return a.appendToBranch(ctx, tr, branch, overlay)
}

// getAssistantMessage runs the provider call + aggregation as one durable step.
// Under the default NoopStepRunner the closure runs inline and deltas stream
// live to `stream`. Under a durable runner, on replay the recorded
// AssistantMessage is returned WITHOUT a provider call, and its blocks are
// re-emitted to the stream so consumers see a consistent event sequence.
//
//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (a *Agent) getAssistantMessage(
	ctx context.Context, stream *EventStream,
	provider types.Provider, llmMessages []types.Message, toolDefs []types.ToolDef, opts *types.RequestOptions, stepName string,
) (*types.AssistantMessage, *types.UsageDelta, error) {
	var (
		liveUsage *types.UsageDelta // captured if the provider emitted usage
		ran       bool              // true iff the closure actually executed (not replayed)
		// partial is the committed part of a call stopped by
		// SubmitInterruptReplace, kept in case the runner drops the result.
		partial *types.AssistantMessage
	)

	// Resolve the response schema from the run context before the step: a
	// durable runner may call fn with a context derived from its own, which
	// does not carry a schema scoped by Structured.
	out := a.output(ctx)
	start := time.Now()
	res, err := a.cfg.StepRunner.RunStep(ctx, stepName, func(stepCtx context.Context) (stepResult types.StepResult, stepError error) {
		ran = true
		if a.cfg.Budget != nil {
			reservation, pricing, err := a.reserveProviderCall(stepCtx, stream, provider, stepName)
			if err != nil {
				return types.StepResult{}, err
			}
			defer func() {
				usage, failed := liveUsage, stepError != nil
				if errors.Is(stepError, errInterruptRequested) {
					// An interrupted call is a known partial result, not a
					// lost one: charge what it used, estimated where the
					// provider had not reported it yet, rather than the
					// whole reservation.
					usage, failed = interruptedUsage(liveUsage, llmMessages, partial), false
				}
				receipt, usage, settleErr := a.settleProviderCall(reservation, pricing, provider, usage, failed)
				liveUsage = usage
				stepResult.Usage = usage
				stepResult.Receipt = &receipt
				switch {
				case settleErr == nil:
				case errors.Is(settleErr, types.ErrBudgetExceeded):
					// The call cost more than its reservation. The usage is
					// recorded; the turn was paid for and is kept, and the
					// budget policy (stop, approval, or warn) is applied when
					// the usage is charged after the step.
					a.cfg.Logger.Warn("provider call exceeded its budget reservation",
						"agent", a.cfg.Name, "step", stepName, "cost", receipt.Cost.String())
				case stepError == nil:
					stepError = settleErr
				}
			}()
		}
		defer func() {
			if value := recover(); value != nil {
				stepError = fmt.Errorf("provider panic: %v", value)
			}
		}()
		// Bound the provider call (and its stream aggregation) with a child
		// deadline so a slow provider is cancelled rather than hanging the loop.
		if a.cfg.LLMTimeout > 0 {
			var cancel context.CancelFunc
			stepCtx, cancel = context.WithTimeout(stepCtx, a.cfg.LLMTimeout)
			defer cancel()
		}
		llmStart := time.Now()
		agg := NewDefaultAggregator()
		partialOut := newPartialJSON(out, toolDefs)
		// stopped returns the partial turn of a call stopped by
		// SubmitInterruptReplace, with an error that still matches
		// context.Canceled for the step runner.
		stopped := func() (types.StepResult, error) {
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), errTurnInterrupted)
			partial = interruptedPartial(agg)
			return types.StepResult{Kind: types.StepKindLLM, Message: partial, Usage: liveUsage}, errTurnInterrupted
		}
		// A provider that reports no routes of its own, such as a single
		// adapter, gets its dial decisions reported here, so the turn and
		// evals record them as they do a router's.
		localRoute := localDialRoute(provider, opts, toolDefs)
		if localRoute != nil {
			stream.send(*localRoute)
		}
		rx, llmErr := a.callProvider(stepCtx, provider, out, llmMessages, toolDefs, opts)
		if llmErr != nil {
			if interruptRequested(stepCtx) {
				return stopped()
			}
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), llmErr)
			return types.StepResult{}, llmErr
		}
		var streamErr error
		lastRoute := localRoute
		for delta := range rx {
			switch d := delta.(type) {
			case types.UsageDelta:
				// Providers may emit usage in parts (e.g. Anthropic sends prompt
				// tokens at message_start and completion tokens at message_delta);
				// merge so token totals are complete.
				if liveUsage == nil {
					liveUsage = &types.UsageDelta{}
				}
				merged := liveUsage.Merge(d)
				liveUsage = &merged
			case types.ErrorDelta:
				// A mid-stream provider error (e.g. 529 overload after
				// message_start) must fail the turn, not be treated as success.
				// runLoop re-emits it as the turn error, so don't forward twice.
				streamErr = d.Error
			default:
				if rd, ok := delta.(types.RouteDelta); ok {
					// The last route of the call names the configuration
					// that produced the committed turn.
					lastRoute = &rd
				}
				stream.send(delta) // live streaming (no-op runner path)
				agg.Push(delta)
				if pj, ok := partialOut.push(delta); ok {
					stream.send(pj)
				}
			}
		}
		if interruptRequested(stepCtx) {
			return stopped()
		}
		if streamErr != nil {
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), streamErr)
			return types.StepResult{}, streamErr
		}
		// (timeouts) A well-behaved provider observes stepCtx and closes its
		// channel when the deadline fires. Surface that as a transient provider
		// error so the turn fails instead of treating a truncated stream as a
		// clean finish.
		if a.cfg.LLMTimeout > 0 && stepCtx.Err() == context.DeadlineExceeded {
			toErr := &types.ProviderError{
				Provider: types.ProviderName(provider),
				Model:    types.ProviderModel(provider),
				Kind:     types.ErrorKindTransient,
				Err:      fmt.Errorf("llm call exceeded timeout %s: %w", a.cfg.LLMTimeout, context.DeadlineExceeded),
			}
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), toErr)
			return types.StepResult{}, toErr
		}
		if stepCtx.Err() != nil {
			return types.StepResult{}, stepCtx.Err()
		}
		// The output token limit cut the turn short. The completed blocks are
		// kept; a call still streaming is recorded with an argument error so
		// the loop knows a call was cut off (handleTruncation never runs it).
		var cutOff []string
		if truncationReason(liveUsage) != "" {
			cutOff = agg.OpenToolCalls()
			agg.Flush()
		}
		// A stream that ends with a tool call still open was cut short. Its
		// arguments are incomplete, so the call must not run, and dropping it
		// would make the turn look like a clean text-only finish.
		if open := agg.OpenToolCalls(); len(open) > 0 {
			truncErr := fmt.Errorf("%w: stream ended with open tool calls %v", types.ErrResponseTruncated, open)
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), truncErr)
			return types.StepResult{}, truncErr
		}
		providerName := types.ProviderName(provider)
		a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", providerName, time.Since(llmStart), nil)
		// (token metric) Record token usage once per completed LLM call with the
		// merged prompt/completion counts. Skipped for cache hits (no new tokens
		// were produced) and when the provider reported no usage at all.
		if liveUsage != nil && !liveUsage.CacheHit &&
			(liveUsage.PromptTokens > 0 || liveUsage.CompletionTokens > 0) {
			a.cfg.Metrics.RecordTokenUsage(stepCtx, "chat", providerName,
				liveUsage.PromptTokens, liveUsage.CompletionTokens)
			if r, ok := a.cfg.Metrics.(types.CacheUsageRecorder); ok &&
				(liveUsage.CachedPromptTokens > 0 || liveUsage.CacheWriteTokens > 0) {
				r.RecordCacheTokenUsage(stepCtx, "chat", providerName,
					liveUsage.CachedPromptTokens, liveUsage.CacheWriteTokens)
			}
		}
		var msg *types.AssistantMessage
		m, ok := agg.Message().(types.AssistantMessage)
		for _, id := range cutOff {
			m.Content = append(m.Content, types.ToolUseContent{ID: id, ArgumentsError: errTruncatedToolCall})
			ok = true
		}
		if ok && lastRoute != nil {
			// Metadata: stripped before the next provider call.
			m.Content = append(m.Content, types.RouteContentFrom(*lastRoute))
		}
		if ok {
			msg = &m
		}
		return types.StepResult{Kind: types.StepKindLLM, Message: msg, Usage: liveUsage}, nil
	})
	if errors.Is(err, errInterruptRequested) {
		// A stopped call returns its partial turn with the error. A durable
		// runner hands back what it recorded; otherwise the in-process copy
		// is used.
		if res.Message != nil {
			partial = res.Message
		}
		usage := liveUsage
		if !ran {
			usage = res.Usage
		}
		if a.cfg.Budget == nil || usage == nil {
			// Settlement already estimated the usage under a budget.
			// Without one, the same estimate is reported, so a stopped
			// call is never invisible to usage accounting.
			usage = interruptedUsage(usage, llmMessages, partial)
		}
		usage.Latency = time.Since(start)
		return partial, usage, err
	}
	if err != nil {
		return nil, nil, err
	}

	latency := time.Since(start)

	// On REPLAY the closure never ran, so nothing streamed; re-emit the recorded
	// message's blocks so the consumer's view matches a live run.
	if !ran && res.Receipt != nil && a.cfg.Budget != nil {
		if err := a.cfg.Budget.Restore(*res.Receipt); err != nil {
			return nil, nil, err
		}
	}
	if !ran && res.Message != nil {
		replayAssistantBlocks(stream, *res.Message)
	}

	usage := liveUsage
	if !ran {
		usage = res.Usage
	}
	if usage == nil {
		usage = &types.UsageDelta{}
	}
	usage.Latency = latency
	return res.Message, usage, nil
}

// interruptedUsage returns the usage to charge for a provider call stopped
// by SubmitInterruptReplace. Counts the provider already reported are kept;
// a missing prompt count is estimated from the request, and a missing
// completion count from the partial turn.
func interruptedUsage(live *types.UsageDelta, messages []types.Message, partial *types.AssistantMessage) *types.UsageDelta {
	usage := types.UsageDelta{}
	if live != nil {
		usage = *live
	}
	if usage.PromptTokens == 0 {
		usage.PromptTokens = types.EstimateTokens(messages)
	}
	if usage.CompletionTokens == 0 && partial != nil {
		usage.CompletionTokens = types.EstimateTokens([]types.Message{*partial})
	}
	usage.TotalTokens = max(usage.TotalTokens, usage.PromptTokens+usage.CompletionTokens)
	return &usage
}

// interruptRequested reports whether ctx was cancelled by
// SubmitInterruptReplace.
func interruptRequested(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errInterruptRequested)
}

// toolResult collects the outcome of a single tool execution.
type toolResult struct {
	toolCallID     string
	result         string                  // text projection
	blocks         []types.ToolResultBlock // rich multi-modal output (nil for plain tools)
	err            string
	handoffTo      string // non-empty when a HandoffSignaler tool fired
	handoffReason  string
	handoffMessage string
	handoffContext string
	// refused marks a call that a gate or a human declined before it ran.
	// It carries err like a failure but is not counted as a tool fault.
	refused bool
	// subAgent names the child of a delegation, so a failed one can be
	// reported to the OutcomePolicy.
	subAgent string
	// version is the version the tool reported, recorded with its result.
	version string
	// approvals records the approval policy's decisions about the call.
	approvals []types.ApprovalContent
}

// executeToolsConcurrently runs all tool calls, streaming deltas as they arrive.
// Results are returned in the same order as toolCalls. Tools are looked up in the
// active registry (which differs per agent during a handoff).
//
// Noop and explicitly concurrent runners execute independent calls in parallel.
// Other durable engines keep steps on their workflow goroutine. A limit of one
// preserves model order. Only regular tool execution holds a semaphore slot;
// approval waits and delegated children do not. Children apply their own limit.
func (a *Agent) executeToolsConcurrently(ctx context.Context, stream *EventStream, toolCalls []types.ToolUseContent, tools *types.ToolRegistry) []toolResult {
	results := make([]toolResult, len(toolCalls))
	transfers := 0
	for _, call := range toolCalls {
		tool, _ := tools.Get(call.Name)
		if marked, ok := types.As[*types.MarkedTool](tool); ok {
			tool = marked.Inner
		}
		if _, ok := types.As[HandoffSignaler](tool); ok {
			transfers++
		}
	}
	if transfers > 1 {
		for i, call := range toolCalls {
			results[i] = failedTool(stream, call.ID, call.Name, "ambiguous handoff: request one control transfer per turn")
		}
		return results
	}

	_, isNoop := a.cfg.StepRunner.(types.NoopStepRunner)
	concurrent, _ := a.cfg.StepRunner.(types.ConcurrentStepRunner)
	if (!isNoop && (concurrent == nil || !concurrent.ConcurrentSteps())) || a.cfg.MaxParallelTools == 1 {
		for i, tc := range toolCalls {
			results[i] = a.executeOneTool(ctx, stream, tc, tools)
		}
		return results
	}

	// Optional concurrency cap: a buffered channel acts as a counting semaphore.
	// A slot is acquired before each tool runs and released after, so at most
	// MaxParallelTools goroutines execute a tool simultaneously. 0 = unlimited.
	// A cap of 1 never reaches here; it took the sequential path above.
	var sem chan struct{}
	if a.cfg.MaxParallelTools > 0 {
		sem = make(chan struct{}, a.cfg.MaxParallelTools)
	}

	ctx = context.WithValue(ctx, toolSlotsKey{}, sem)
	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		wg.Add(1)
		go func(idx int, tc types.ToolUseContent) {
			defer wg.Done()

			results[idx] = a.executeOneTool(ctx, stream, tc, tools)
		}(i, tc)
	}
	wg.Wait()
	return results
}

// chargeBudget records this call's usage and enforces the policy.
//
// Under BudgetStop the run ends with ErrBudgetExceeded. Under
// BudgetRequireApproval it escalates to the same human-in-the-loop path tool
// approvals use, so a deployment implements one resolution protocol rather than
// two; approving buys a bounded grant rather than removing the ceiling.
func (a *Agent) chargeBudget(ctx context.Context, stream *EventStream, provider types.Provider, usage types.UsageDelta) error {
	if a.cfg.Budget == nil {
		return nil
	}

	// A response cache replays the recorded token counts so observability keeps
	// the original numbers, but no provider call was made and nothing was
	// billed. Charging it would make a cached run report spend it did not
	// incur, which is the opposite of what the budget is for.
	if usage.CacheHit {
		return nil
	}

	// Attribute to the model that actually answered, not the one configured.
	// For a fallback chain ProviderModel reports the primary's name even when a
	// secondary served the request, which would credit every fallback dollar to
	// the primary and make Breakdown point at the wrong model. The pricing
	// still comes from the chain's worst-case card: over-counting the total is
	// the safe direction, mis-attributing it is not.
	model := usage.ResponseModel
	if model == "" {
		model = types.ProviderModel(provider)
	}
	caps, _ := types.ProviderCapabilities(provider)

	var status types.BudgetStatus
	var err error
	if usage.AccountingID != "" {
		status, err = a.cfg.Budget.RecordOnce(usage.AccountingID, model, caps.Pricing, types.UsageFromDelta(usage))
	} else {
		status, err = a.cfg.Budget.Record(model, caps.Pricing, types.UsageFromDelta(usage))
	}
	if err != nil {
		return err // unpriced model under an enforcing policy
	}

	switch status {
	case types.BudgetStatusWarn:
		a.cfg.Logger.Warn("budget nearing its limit",
			"spent", a.cfg.Budget.Spent().String(),
			"remaining", a.cfg.Budget.Remaining().String())
		return nil
	case types.BudgetStatusExceeded:
		if a.cfg.Budget.Policy().OnExceed == types.BudgetRequireApproval {
			marker := a.cfg.Budget.ApprovalMarker()
			pending := types.ToolUseContent{ID: "budget-" + usage.AccountingID, Name: budgetToolName}
			msg, _, approved := a.awaitApproval(ctx, stream, pending, []types.Marker{marker})
			if !approved {
				if err := stream.runError(); err != nil {
					return err
				}
				return fmt.Errorf("%w: %s", types.ErrBudgetExceeded, msg)
			}
			a.cfg.Budget.Grant(0)
			return nil
		}
		return a.cfg.Budget.Err()
	}
	return nil
}

// Citations returns the run's citation registry: every source the provider or
// a tool attributed a claim to, numbered once across the whole conversation.
func (a *Agent) Citations() *types.CitationRegistry { return a.citations }

// Budget returns the configured budget, or nil.
func (a *Agent) Budget() *types.Budget { return a.cfg.Budget }

// awaitApproval emits a MarkerDelta and blocks for a human decision. It is the
// single human-in-the-loop primitive: the pre-gate, the pre-existing
// MarkedTool mechanism, and the budget escalation all route through it, so a
// consumer only ever implements one resolution protocol.
//
// It returns (message, modifiedArgs, approved). On refusal or cancellation the
// message is the tool error to report.
func (a *Agent) awaitApproval(ctx context.Context, stream *EventStream, tc types.ToolUseContent, markers []types.Marker) (string, map[string]any, bool) {
	d, ok := a.awaitApprovalPhase(ctx, stream, tc, markers, "gate")
	return d.message, d.args, ok
}

// awaitApprovalPhase asks for one decision about tc. On refusal the
// decision's message is the tool error to report.
func (a *Agent) awaitApprovalPhase(ctx context.Context, stream *EventStream, tc types.ToolUseContent, markers []types.Marker, phase string) (decision, bool) {
	d, ok := a.awaitInterrupt(ctx, stream, interruptRequest{kind: interruptKind(tc, phase), phase: phase, call: tc, markers: markers})
	if !ok {
		return d, false
	}
	return decision{approved: true, args: d.args, approver: d.approver, grant: d.grant}, true
}

// errNonStreamingApproval stops a run that needs a human decision but has
// no consumer to ask and no ApprovalRunner to record the request.
var errNonStreamingApproval = errors.New("non-streaming approvals require an ApprovalRunner")

// executeOneTool runs a single tool call to completion, emitting the start/end
// deltas and returning its result. Every exit path sets the result's ToolCallID
// and emits a terminal ToolExecEndDelta, so a cancelled or rejected call never
// leaves a zero-valued result that would persist a tool_result with no matching
// tool_use ID.
//
// A panic in the gate, a marker check, a handoff tool, or anything else on
// this path becomes an error result for this call. One faulty tool must not
// take down the process and every other run in it.
func (a *Agent) executeOneTool(ctx context.Context, stream *EventStream, tc types.ToolUseContent, tools *types.ToolRegistry) (res toolResult) {
	stream.send(types.ToolExecStartDelta{ToolCallID: tc.ID, Name: tc.Name})
	var ca callApproval
	// Registered first so it runs last, after a recovered panic set res:
	// the decisions made before a failure are recorded too.
	defer func() { res.approvals = ca.records }()
	defer func() {
		if p := recover(); p != nil {
			err := a.toolPanic(ctx, tc, p)
			res = failedTool(stream, tc.ID, tc.Name, err.Error())
		}
	}()

	tool, found := tools.Get(tc.Name)
	if !found {
		return failedTool(stream, tc.ID, tc.Name, fmt.Sprintf("tool not found: %s", tc.Name))
	}

	// Arguments the model sent malformed, or that do not fit the declared
	// parameters, never reach the gate or the tool. The model gets the reason
	// back as a tool error and can correct the call.
	def := tool.Definition()
	if tc.ArgumentsError != "" {
		return failedTool(stream, tc.ID, tc.Name, fmt.Sprintf("%s: %s", types.ErrInvalidToolArguments, tc.ArgumentsError))
	}
	if err := types.ValidateToolArgs(def.Parameters, tc.Arguments); err != nil {
		return failedTool(stream, tc.ID, tc.Name, err.Error())
	}

	if res, done := a.gateTool(ctx, stream, &tc, def, &ca); done {
		return res
	}

	// Attach the deployment's tool knobs so a Configurable tool can read them
	// without its signature changing.
	if a.cfg.ToolContext.Len() > 0 {
		ctx = types.WithToolContext(ctx, a.cfg.ToolContext)
	}
	ctx = types.WithToolCallInfo(ctx, types.ToolCallInfo{
		ID: tc.ID, Name: tc.Name, Agent: a.toolOwner(ctx), RunID: stream.runID, Branch: stream.branch,
	})
	if a.cfg.Workspace != nil {
		ctx = workspace.NewContext(ctx, a.cfg.Workspace)
	}
	if a.cfg.Deps != nil {
		ctx = context.WithValue(ctx, agentDepsKey{}, a.cfg.Deps)
	}

	tool, res, done := a.resolveMarkers(ctx, stream, &tc, tool, &ca)
	if done {
		return res
	}
	ctx = types.WithCallApproval(ctx, ca.approval)

	// A per-tool quota is charged at dispatch, once the call is cleared to
	// run, so a denied call never uses up the allowance. An exhausted quota
	// is a tool error, so a model that keeps calling the tool trips
	// MaxConsecutiveErrors instead of looping.
	if a.cfg.Budget != nil {
		if err := a.cfg.Budget.ReserveToolCall(tc.Name); err != nil {
			return failedTool(stream, tc.ID, tc.Name, err.Error())
		}
	}

	// Handoff signal: a control transfer, not a normal result. Checked before
	// SubAgentInvoker and never wrapped in a durable step.
	if h, ok := types.As[HandoffSignaler](tool); ok {
		target := h.HandoffTarget()
		out, _ := tool.Execute(ctx, tc.Arguments)
		stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Name: tc.Name, Result: out})
		return toolResult{
			toolCallID: tc.ID, result: out, handoffTo: target,
			handoffReason:  stringArg(tc.Arguments, "reason"),
			handoffMessage: stringArg(tc.Arguments, argMessage),
			handoffContext: stringArg(tc.Arguments, "context"),
		}
	}

	// A clarification waits for a person, like an approval, so it is not a
	// durable step and its wait is not bounded by ToolTimeout.
	if _, ok := tool.(*clarificationTool); ok {
		return a.askClarification(ctx, stream, tc)
	}
	if spawn, ok := tool.(*spawnTool); ok {
		return a.spawnSubAgent(ctx, stream, tc, spawn)
	}
	if invoker, ok := types.As[SubAgentInvoker](tool); ok {
		return a.delegateToSubAgent(ctx, stream, tc, tool, invoker)
	}
	if stream.spawns != nil {
		ctx = context.WithValue(ctx, spawnRegistryKey{}, stream.spawns)
	}

	if sem, ok := ctx.Value(toolSlotsKey{}).(chan struct{}); ok && sem != nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return failedTool(stream, tc.ID, tc.Name, ctx.Err().Error())
		}
		defer func() { <-sem }()
	}

	res = a.runToolStep(ctx, stream, tc, tool)
	res.version = types.ToolVersion(tool)
	return res
}

// failedTool emits the terminal ToolExecEndDelta for a call that failed before
// (or instead of) execution and returns the matching result. Every early exit
// from executeOneTool goes through here so a tool_use ID is never left without
// a tool_result.
// The tool name is reported on the delta, so a consumer can label a refused
// or failed call the same way as one that ran.
func failedTool(stream *EventStream, toolCallID, name, errMsg string) toolResult {
	res := toolResult{toolCallID: toolCallID, err: errMsg}
	stream.send(types.ToolExecEndDelta{ToolCallID: toolCallID, Name: name, Error: res.err})
	return res
}

// refusedTool answers a call that a gate or a human declined. The model sees
// the same error result as a failure.
func refusedTool(stream *EventStream, toolCallID, name, errMsg string) toolResult {
	res := failedTool(stream, toolCallID, name, errMsg)
	res.refused = true
	return res
}

// toolGate returns the agent's gate, with the capability defaults of its
// ApprovalPolicy added when it asks for them.
func (a *Agent) toolGate() types.ToolGate {
	if p := a.cfg.ApprovalPolicy; p != nil && p.RiskDefaults {
		return types.Gates(a.cfg.ToolGate, riskGate)
	}
	return a.cfg.ToolGate
}

// gateTool applies the pre-execution gate to the resolved definition and the
// model's actual arguments, before anything runs. It may rewrite tc.Arguments
// in place. A denial is reported to the model as a tool error so it can adapt,
// rather than failing the turn; done reports whether the call is finished.
func (a *Agent) gateTool(ctx context.Context, stream *EventStream, tc *types.ToolUseContent, def types.ToolDef, ca *callApproval) (toolResult, bool) {
	decision := a.toolGate().Check(ctx, def, tc.Arguments)
	if decision.Outcome == types.GateAllow {
		if decision.ModifiedArgs != nil {
			// An allowing gate may still narrow the call, e.g. clamping a limit.
			tc.Arguments = decision.ModifiedArgs
		}
		return toolResult{}, false
	}

	if decision.ModifiedArgs != nil {
		tc.Arguments = decision.ModifiedArgs
	}
	switch decision.Outcome {
	case types.GateDeny:
		reason := decision.Reason
		if reason == "" {
			reason = "denied by policy"
		}
		return refusedTool(stream, tc.ID, tc.Name, "refused: "+reason), true
	case types.GateRequireApproval:
		marker := types.Marker{Kind: "gate_approval", Message: decision.Reason}
		if decision.Marker != nil {
			marker = *decision.Marker
		}
		d, ok := a.requestApproval(ctx, stream, *tc, def, []types.Marker{marker}, "gate", ca)
		if !ok {
			return refusedTool(stream, tc.ID, tc.Name, d.message), true
		}
		if d.args != nil {
			return a.recheckEditedArgs(ctx, stream, tc, def, d.args)
		}
	}
	return toolResult{}, false
}

// recheckEditedArgs applies arguments a human edited while approving a call.
// Approval covers the call as presented, not whatever the edit turns it into,
// so the edited arguments are validated and gated again: a denial still
// refuses the call and a gate rewrite still applies. A second approval
// request is not raised, because a human already decided on these arguments.
func (a *Agent) recheckEditedArgs(ctx context.Context, stream *EventStream, tc *types.ToolUseContent, def types.ToolDef, args map[string]any) (toolResult, bool) {
	tc.Arguments = args
	if err := types.ValidateToolArgs(def.Parameters, args); err != nil {
		return failedTool(stream, tc.ID, tc.Name, err.Error()), true
	}
	decision := a.toolGate().Check(ctx, def, args)
	if decision.ModifiedArgs != nil {
		tc.Arguments = decision.ModifiedArgs
	}
	if decision.Outcome == types.GateDeny {
		reason := decision.Reason
		if reason == "" {
			reason = "denied by policy"
		}
		return refusedTool(stream, tc.ID, tc.Name, "refused: "+reason), true
	}
	return toolResult{}, false
}

// resolveMarkers emits a MarkerDelta for a marked tool and waits for human
// resolution, returning the unwrapped inner tool once approved. It may rewrite
// tc.Arguments in place; done reports whether the call is finished.
//
// The marked tool is found through decorators (types.As), so a decorator that
// wraps a marked tool without re-marking it still prompts. The decorator is
// then kept and run as is; its MarkedTool runs the inner tool without asking
// again.
func (a *Agent) resolveMarkers(ctx context.Context, stream *EventStream, tc *types.ToolUseContent, tool types.Tool, ca *callApproval) (types.Tool, toolResult, bool) {
	mt, ok := types.As[*types.MarkedTool](tool)
	if !ok || len(mt.Markers) == 0 {
		return tool, toolResult{}, false
	}

	d, approved := a.requestApproval(ctx, stream, *tc, tool.Definition(), mt.Markers, "marker", ca)
	if !approved {
		return tool, refusedTool(stream, tc.ID, tc.Name, d.message), true
	}
	if d.args != nil {
		if res, done := a.recheckEditedArgs(ctx, stream, tc, tool.Definition(), d.args); done {
			return tool, res, true
		}
	}

	if types.Tool(mt) != tool {
		return tool, toolResult{}, false
	}
	return mt.Inner, toolResult{}, false
}

// delegateToSubAgent runs a sub-agent tool, forwarding child deltas. The
// delegation itself is not a durable step: the child inherits the parent's
// StepRunner so its own LLM/tool steps are the durable units.
//
// The parent's ToolTimeout does not bound the delegation: the child applies
// it to each of its own tool calls. SubAgentDef.Timeout bounds the whole
// child run, with the clock paused while a child approval waits for a human.
func (a *Agent) delegateToSubAgent(ctx context.Context, stream *EventStream, tc types.ToolUseContent, tool types.Tool, invoker SubAgentInvoker) toolResult {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var clock *pausableDeadline
	if st, ok := tool.(*subAgentTool); ok && st.timeout > 0 {
		timeoutErr := fmt.Errorf("sub-agent %s exceeded timeout %s: %w", st.name, st.timeout, context.DeadlineExceeded)
		clock = newPausableDeadline(st.timeout, func() { cancel(timeoutErr) })
		defer clock.stop()
	}
	task, _ := tc.Arguments[argTask].(string)
	var childStream *EventStream
	frame := a.childFrame(ctx, stream, tc.ID)
	if st, ok := tool.(*subAgentTool); ok {
		if err := a.checkAncestor(ctx, st.name); err != nil {
			return refusedTool(stream, tc.ID, tc.Name, "refused: "+err.Error())
		}
		history, err := a.childHistory(ctx, stream, st.context, st.filter)
		if err != nil {
			return failedTool(stream, tc.ID, tc.Name, err.Error())
		}
		childStream, err = st.start(ctx, childRun{
			task: task, history: history, runner: a.childStepRunner(tc.ID), id: tc.ID,
			nonStreaming: stream.nonStreaming, frame: frame,
		})
		if err != nil {
			return failedTool(stream, tc.ID, tc.Name, err.Error())
		}
	} else {
		childStream = invoker.InvokeAgent(withFrame(ctx, frame), task)
	}

	// A child failure must fail the delegation: partial text plus success
	// would let the parent LLM treat a crashed child as a completed task.
	// The ErrorDelta scan covers custom SubAgentInvoker streams that may
	// not close with the error they emitted.
	var childErr error
	var resultBuf strings.Builder
	// pending tracks forwarded child decisions still waiting for a reply.
	var pending sync.WaitGroup
	for d := range childStream.Deltas() {
		if marker, ok := d.(types.MarkerDelta); ok {
			if stream.nonStreaming {
				// No consumer can answer this marker. A custom invoker's
				// child would otherwise wait for it forever.
				childErr = errNonStreamingApproval
				childStream.Cancel()
				continue
			}
			a.forwardChildMarker(ctx, stream, tc.ID, childStream, marker, clock, &pending)
			continue
		}
		stream.send(types.ToolExecDelta{ToolCallID: tc.ID, Inner: d})
		switch v := d.(type) {
		case types.TextContentDelta:
			resultBuf.WriteString(v.Content)
		case types.CitationDelta:
			// A source the child cited is a source the whole answer rests
			// on, so it is registered here too and gets one number across
			// parent and child rather than one per agent.
			a.citations.Add(v.Citation)
		case types.ErrorDelta:
			childErr = v.Error
		}
	}
	pending.Wait()
	if err := childStream.Wait(); err != nil && !errors.Is(childErr, errNonStreamingApproval) {
		childErr = err
		if _, durable := a.cfg.StepRunner.(types.ApprovalRunner); durable {
			stream.stopRun(err)
		}
	}
	res := toolResult{toolCallID: tc.ID, result: resultBuf.String()}
	if st, native := tool.(*subAgentTool); native {
		result, err := childStream.SubAgentResult()
		res.result = result.Output
		if err != nil && !errors.Is(childErr, errNonStreamingApproval) {
			childErr = err
		}
		// The transcript tools can search a finished delegation too.
		stream.spawns.recordDelegation(tc.ID, st.name, result, err)
	}
	// Report the delegation timeout itself rather than the cancellation it
	// caused inside the child.
	if cause := context.Cause(ctx); childErr != nil && clock != nil && errors.Is(cause, context.DeadlineExceeded) {
		childErr = cause
	}
	// An approval nobody can answer stops the parent run too, as it would
	// for the parent's own tools, instead of handing the model a tool error.
	if stream.nonStreaming && errors.Is(childErr, errNonStreamingApproval) {
		stream.stopRun(errNonStreamingApproval)
	}
	if _, native := tool.(*subAgentTool); !native && a.cfg.ToolRedactor != nil {
		// A custom invoker's output, such as a remote agent's, is tool
		// output like any other, so it is tokenized before the model, the
		// tree, or the consumer sees it.
		// The partial text and the error are tokenized separately, so a
		// failed delegation still reports what the child produced.
		def := tool.Definition()
		text, _, err := a.tokenizeToolOutput(ctx, def, res.result, nil, nil)
		if err != nil {
			text, childErr = "", err
		} else if childErr != nil {
			_, _, childErr = a.tokenizeToolOutput(ctx, def, "", nil, childErr)
		}
		res.result = text
	}
	if childErr != nil {
		res.err = childErr.Error()
	}
	res.subAgent = tc.Name
	if st, ok := tool.(*subAgentTool); ok {
		res.subAgent = st.name
	}
	stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Name: tc.Name, Result: res.result, Error: res.err})
	return res
}

// runToolStep executes a regular tool, wrapped in a durable step. A RichTool
// yields multi-modal Blocks; a plain Tool yields text only.
func (a *Agent) runToolStep(ctx context.Context, stream *EventStream, tc types.ToolUseContent, tool types.Tool) toolResult {
	stepName := "tool-" + tc.ID
	if types.IsIdempotent(tool) {
		ctx = types.WithIdempotentStep(ctx)
	}
	sr, stepErr := a.cfg.StepRunner.RunStep(ctx, stepName, func(stepCtx context.Context) (result types.StepResult, panicErr error) {
		// A panic is returned as a Go error, not as a recorded tool error:
		// a durable runner must see the step fail so it is marked
		// indeterminate rather than saved as a completed result.
		defer func() {
			if p := recover(); p != nil {
				result, panicErr = types.StepResult{}, a.toolPanic(stepCtx, tc, p)
			}
		}()
		// Bound this tool call with a child deadline so a slow tool is cancelled
		// rather than hanging the loop. Tools that respect stepCtx return the
		// deadline error; tools that ignore it still run to completion (we cannot
		// preempt a blocking call), but the deadline is observable below.
		if a.cfg.ToolTimeout > 0 {
			var cancel context.CancelFunc
			stepCtx, cancel = context.WithTimeout(stepCtx, a.cfg.ToolTimeout)
			defer cancel()
		}
		toolStart := time.Now()
		var (
			text    string
			blocks  []types.ToolResultBlock
			execErr error
		)
		// Placeholders become real values only here, after the gate and any
		// approval, and only for the tool itself.
		args := tc.Arguments
		if a.cfg.ToolRedactor != nil {
			args = a.cfg.ToolRedactor.RestoreArgs(stepCtx, tool.Definition(), args)
		}
		if rt, ok := tool.(types.RichTool); ok {
			var tr types.ToolResult
			tr, execErr = rt.ExecuteRich(stepCtx, args)
			text, blocks = tr.Text, tr.Blocks
			// Register the tool's sources in the run-wide registry so a page
			// found by a local search tool and the same page found by the
			// provider's server-side search share one footnote number. A
			// citation is tool output too, so it is tokenized first.
			cites := tr.Citations
			if a.cfg.ToolRedactor != nil {
				cites = a.tokenizeCitations(stepCtx, tool.Definition(), cites)
			}
			for _, c := range a.citations.AddAll(cites) {
				stream.send(types.CitationDelta{Citation: c, ToolCallID: tc.ID})
			}
			if execErr == nil && tr.IsError {
				execErr = errors.New(tr.Text) // tool-signalled error without a Go error
			}
		} else {
			text, execErr = tool.Execute(stepCtx, args)
		}
		// Surface a deadline overrun as a tool error even when the tool ignored
		// stepCtx and returned no error of its own, so the turn can distinguish a
		// timed-out tool from a successful one.
		if execErr == nil && a.cfg.ToolTimeout > 0 && stepCtx.Err() == context.DeadlineExceeded {
			execErr = fmt.Errorf("tool %s exceeded timeout %s: %w", tc.Name, a.cfg.ToolTimeout, context.DeadlineExceeded)
			text, blocks = "", nil
		}
		a.cfg.Metrics.RecordToolCall(stepCtx, tc.Name, time.Since(toolStart), execErr)
		if a.cfg.ToolRedactor != nil {
			// Tokenize inside the step, so a durable runner records only
			// placeholders. An error message can quote its input, so it is
			// tokenized too.
			text, blocks, execErr = a.tokenizeToolOutput(stepCtx, tool.Definition(), text, blocks, execErr)
		}
		out := types.StepResult{Kind: types.StepKindTool, ToolCallID: tc.ID, ToolResult: text, ToolBlocks: blocks}
		if execErr != nil {
			out.ToolError = execErr.Error() // error-in-payload: recorded once, not retried
		}
		return out, nil
	})

	var res toolResult
	var panicked *toolPanicError
	_, inline := a.cfg.StepRunner.(types.NoopStepRunner)
	if stepErr != nil && inline && errors.As(stepErr, &panicked) {
		// Inline execution records nothing, so the panic is just this call's
		// error and the run continues.
		res = toolResult{toolCallID: tc.ID, err: stepErr.Error()}
	} else if stepErr != nil {
		stream.stopRun(stepErr)
		// Infrastructure failure from the runner itself (e.g. durable engine
		// error): surface it as a tool error rather than dropping it.
		res = toolResult{toolCallID: tc.ID, err: stepErr.Error()}
	} else {
		res = toolResult{toolCallID: tc.ID, result: sr.ToolResult, blocks: sr.ToolBlocks, err: sr.ToolError}
	}
	stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Name: tc.Name, Result: res.result, Blocks: res.blocks, Error: res.err, Version: types.ToolVersion(tool)})
	return res
}

// tokenizeToolOutput applies the ToolRedactor to a tool's text, blocks, and
// error. A redactor that withholds a successful result turns it into an error.
func (a *Agent) tokenizeToolOutput(ctx context.Context, def types.ToolDef, text string, blocks []types.ToolResultBlock, execErr error) (string, []types.ToolResultBlock, error) {
	if execErr != nil {
		r := a.cfg.ToolRedactor.TokenizeResult(ctx, def, types.ToolResult{Text: execErr.Error(), IsError: true})
		return "", nil, errors.New(r.Text)
	}
	r := a.cfg.ToolRedactor.TokenizeResult(ctx, def, types.ToolResult{Text: text, Blocks: blocks})
	if r.IsError {
		return "", nil, errors.New(r.Text)
	}
	return r.Text, r.Blocks, nil
}

// tokenizeCitations applies the ToolRedactor to the text fields of each
// citation: URI, title, quote, and string values in Meta. A citation with a
// field the redactor withholds is dropped, since its source cannot be shown.
func (a *Agent) tokenizeCitations(ctx context.Context, def types.ToolDef, cites []types.Citation) []types.Citation {
	out := make([]types.Citation, 0, len(cites))
	tokenize := func(s string) (string, bool) {
		if s == "" {
			return s, true
		}
		r := a.cfg.ToolRedactor.TokenizeResult(ctx, def, types.ToolResult{Text: s})
		return r.Text, !r.IsError
	}
	for _, c := range cites {
		ok := true
		for _, field := range []*string{&c.URI, &c.Title, &c.Quote} {
			if *field, ok = tokenize(*field); !ok {
				break
			}
		}
		if ok && len(c.Meta) > 0 {
			meta := make(map[string]any, len(c.Meta))
			for k, v := range c.Meta {
				if str, isString := v.(string); isString {
					if v, ok = tokenize(str); !ok {
						break
					}
				}
				meta[k] = v
			}
			c.Meta = meta
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// toolOwner names the agent that owns the turn: the active handoff member
// when the run scope says so, otherwise this agent.
func (a *Agent) toolOwner(ctx context.Context) string {
	if scope, ok := RunScopeFromContext(ctx); ok && scope.Agent != "" {
		return scope.Agent
	}
	return a.cfg.Name
}

// toolPanicError reports a panic raised while a tool call was handled.
type toolPanicError struct {
	tool  string
	value any
}

func (e *toolPanicError) Error() string {
	return fmt.Sprintf("tool %s panicked: %v", e.tool, e.value)
}

// ToolPanic marks the error as a recovered tool panic, so metrics and traces
// can classify it without depending on this package.
func (e *toolPanicError) ToolPanic() bool { return true }

// toolPanic logs a recovered panic with its stack, records it as a failed
// tool call, and returns the error that replaces the call's result.
func (a *Agent) toolPanic(ctx context.Context, tc types.ToolUseContent, value any) error {
	err := &toolPanicError{tool: tc.Name, value: value}
	a.cfg.Logger.Error("tool panic recovered",
		"agent", a.cfg.Name, "tool", tc.Name, "tool_call_id", tc.ID, "panic", value, "stack", string(debug.Stack()))
	a.cfg.Metrics.RecordToolCall(ctx, tc.Name, 0, err)
	return err
}

func stringArg(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}

type toolSlotsKey struct{}
