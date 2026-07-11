package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
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
	Handoffs    []HandoffDef
	MaxHandoffs int // max control transfers per run (default 8); ping-pong guard

	// StepRunner durably memoizes LLM and tool calls so a crashed process can
	// resume without repeating them. Defaults to types.NoopStepRunner (inline,
	// today's streaming behavior). A DBOS-backed runner lives in agent/durable/dbos.
	StepRunner types.StepRunner

	// Store persists the conversation tree. Defaults to nil, which preserves
	// today's in-memory-only behavior (the tree lives only for the agent's
	// lifetime). When set, each node added during a run is written to the Store
	// best-effort (errors are logged, never fatal), and NewAgent can hydrate the
	// tree from the Store via WithStore (see WithStore / LoadTreeFromStore).
	Store types.Store

	// LLMTimeout bounds each provider (LLM) call. A child context with this
	// deadline wraps the call in getAssistantMessage. 0 means no timeout.
	LLMTimeout time.Duration

	// ToolTimeout bounds each individual tool execution. A child context with
	// this deadline wraps each tool step in executeOneTool. 0 means no timeout.
	ToolTimeout time.Duration

	// MaxParallelTools caps how many tool goroutines run concurrently when tools
	// are fanned out (NoopStepRunner path). 0 means unlimited. Under a durable
	// StepRunner tools always run sequentially, so this has no effect there.
	MaxParallelTools int

	// File pipeline configuration.
	Resolvers  map[string]types.Resolver           // URI scheme → Resolver (e.g. "file", "https", "s3")
	Extractors map[types.MediaType]types.Extractor // MediaType → Extractor for non-native types

	// Structured output: if set, constrains final LLM output to this JSON schema.
	ResponseSchema *types.ParameterSchema

	// Logger for agent events. Defaults to slog.Default() if nil.
	Logger *slog.Logger

	// Metrics collector. Defaults to NoopMetrics if nil.
	Metrics types.Metrics
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

// WithResponseSchema constrains the final LLM output to a JSON schema.
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

// WithMaxIter overrides the maximum agent loop iterations.
func WithMaxIter(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxIter = n }
}

// WithStepRunner sets a durable step runner. The default NoopStepRunner runs
// steps inline (today's streaming behavior). A DBOS-backed runner (see
// agent/durable/dbos) memoizes LLM and tool calls so a crashed process resumes
// without repeating them.
func WithStepRunner(r types.StepRunner) AgentOption {
	return func(c *AgentConfig) { c.StepRunner = r }
}

// WithStore configures a types.Store so the conversation tree is persisted.
// With no Store (the default) the tree is in-memory only — fully backward
// compatible. When a Store is set, each node added during a run is written
// best-effort; loading a previously-persisted tree is done explicitly via
// LoadTreeFromStore before NewAgent (pass the rebuilt tree with WithTree).
func WithStore(s types.Store) AgentOption {
	return func(c *AgentConfig) { c.Store = s }
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
// are fanned out. A non-positive value means unlimited (today's behavior).
func WithMaxParallelTools(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxParallelTools = n }
}

// Agent runs an LLM agent loop with tool execution.
// All conversations are backed by a Tree.
type Agent struct {
	cfg      AgentConfig
	tools    *types.ToolRegistry
	handoffs *handoffGroup // nil unless WithHandoffs configured an entry group
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
	tools := cfg.Tools
	if tools == nil {
		tools = types.NewToolRegistry()
	}

	if cfg.Tree == nil {
		t, _ := tree.New(types.NewSystemMessage(cfg.SystemPrompt))
		cfg.Tree = t
	}

	// Register sub-agents as delegate tools.
	for _, sa := range cfg.SubAgents {
		registerSubAgent(tools, sa)
	}

	a := &Agent{cfg: cfg, tools: tools}

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
		grp, err := buildHandoffGroup(entry, cfg.Handoffs)
		if err != nil {
			panic(fmt.Sprintf("agent: invalid handoff configuration: %v", err))
		}
		a.handoffs = grp
	}

	return a
}

// registerSubAgent registers a SubAgentDef as a delegate tool. Each invocation
// constructs a fresh Agent — the sub-agent's conversation history is intentionally
// discarded between delegations, so sub-agents are stateless across calls.
func registerSubAgent(registry *types.ToolRegistry, sa SubAgentDef) {
	registry.Register(&subAgentTool{
		def: types.ToolDef{
			Name:        "delegate_to_" + sa.Name,
			Description: sa.Description,
			Parameters: types.ParameterSchema{
				Type:     "object",
				Required: []string{"task"},
				Properties: map[string]types.PropertyDef{
					"task": {Type: "string", Description: "The task to delegate"},
				},
			},
		},
		factory: func() *Agent {
			return NewAgent(AgentConfig{
				Name:         sa.Name,
				SystemPrompt: sa.SystemPrompt,
				Provider:     sa.Provider,
				Tools:        sa.Tools,
				SubAgents:    sa.SubAgents,
				MaxIter:      sa.MaxIter,
			})
		},
	})
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
		// Skip internal delegate/handoff tools — they show as sub-agents/handoffs.
		if strings.HasPrefix(td.Name, "delegate_to_") || strings.HasPrefix(td.Name, "handoff_to_") {
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
// node — it lives on its own dead-end branch, is never flattened into LLM
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
func (a *Agent) Invoke(ctx context.Context, input []types.Message, branch ...types.BranchID) *EventStream {
	b := a.cfg.Tree.Active()
	if len(branch) > 0 {
		b = branch[0]
	}

	ctx, cancel := context.WithCancel(ctx)
	stream := newEventStream(ctx, cancel)

	go a.runLoop(ctx, stream, input, b)

	return stream
}

// RunDurable runs the agent loop to completion (non-streaming) using the given
// durable StepRunner, returning the final assistant message. Each LLM call and
// tool execution is wrapped in runner.RunStep so a crashed process resumes
// without repeating them. It is intended to be called from inside a durable
// workflow (see agent/durable/dbos), which supplies a runner bound to the
// workflow's durable context.
//
// The runner is injected via a shallow copy of the Agent, so concurrent durable
// runs on the same Agent do not race on the step runner. Conversation state is
// persisted to the tree (Store/WAL) regardless of streaming, so it is correct
// after recovery; the runner only prevents re-spending on memoized steps.
func (a *Agent) RunDurable(ctx context.Context, runner types.StepRunner, input []types.Message, branch types.BranchID) (*types.AssistantMessage, error) {
	if runner == nil {
		runner = types.NoopStepRunner{}
	}
	clone := *a
	clone.cfg.StepRunner = runner

	if branch == "" {
		branch = clone.cfg.Tree.Active()
	}

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := newEventStream(loopCtx, cancel)

	// Drain deltas in a separate goroutine so the loop's RunStep calls execute
	// synchronously in THIS goroutine — important for durable engines that
	// correlate steps to the workflow's calling context.
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for d := range stream.Deltas() {
			if ed, ok := d.(types.ErrorDelta); ok {
				runErr = ed.Error
			}
		}
	}()

	clone.runLoop(loopCtx, stream, input, branch) // synchronous; closes stream
	<-done

	if runErr != nil {
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
	compactNow  bool
	activeAgent string // last HandoffContent.To seen on the branch ("" = entry)
}

// prepareMessages resolves config and strips metadata in a single pass over the
// message history. This avoids the cost of two separate O(n) walks per iteration.
func (a *Agent) prepareMessages(messages []types.Message) (resolvedConfig, []types.Message) {
	rc := resolvedConfig{maxIter: a.cfg.MaxIter}
	if a.cfg.CompactCfg != nil {
		rc.compactor = a.cfg.CompactCfg.ToCompactor()
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
					filtered = append(filtered, c)
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
				case types.FeedbackContent:
					continue
				default:
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				out = append(out, types.UserMessage{Content: filtered})
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
	}
	if cc.CompactNow {
		rc.compactNow = true
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

	// First compacted message is the system prompt (same as root) — skip it.
	// Branch from root with the second message (the summary request); the rest
	// (assistant summary + preserved recent context) are appended below.
	branchID, _, err := tr.Branch(ctx, root.ID, "compact", compacted[1])
	if err != nil {
		return "", fmt.Errorf("branch from root: %w", err)
	}

	// Add remaining compacted messages (the preserved recent context).
	for _, msg := range compacted[2:] {
		tip, err := tr.Tip(branchID)
		if err != nil {
			return "", fmt.Errorf("tip lookup: %w", err)
		}
		if _, err := tr.AddChild(ctx, tip.ID, msg); err != nil {
			return "", fmt.Errorf("add compacted child: %w", err)
		}
	}

	// Set the compacted branch as active so future Invoke calls use it.
	if err := tr.SetActive(branchID); err != nil {
		return "", fmt.Errorf("set active: %w", err)
	}

	return branchID, nil
}

// tryCompact attempts compaction if configured. Returns the new branch ID and
// true if compaction succeeded and the caller should re-flatten from the new branch.
func (a *Agent) tryCompact(ctx context.Context, log *slog.Logger, resolved resolvedConfig, llmMessages []types.Message, tr *tree.Tree) (types.BranchID, bool) {
	if resolved.compactor == nil {
		if resolved.compactNow {
			log.Warn("compactNow requested but no compactor configured, ignoring")
		}
		return "", false
	}

	compacted, err := resolved.compactor.Compact(ctx, llmMessages, a.cfg.Provider)
	if err != nil {
		log.Warn("compaction failed, continuing with full history", "error", err)
		return "", false
	}
	if len(compacted) >= len(llmMessages) {
		return "", false
	}

	newBranch, err := a.persistCompacted(ctx, tr, compacted)
	if err != nil {
		log.Warn("failed to persist compacted branch", "error", err)
		return "", false
	}
	log.Debug("compacted to new branch", "branch", newBranch)
	return newBranch, true
}

// callProvider invokes the given provider's LLM, using structured output when
// available. The provider is passed explicitly so the agent loop can swap it
// per-iteration during a handoff.
func (a *Agent) callProvider(ctx context.Context, provider types.Provider, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	if a.cfg.ResponseSchema != nil && len(tools) == 0 {
		if sp, ok := provider.(types.StructuredOutputProvider); ok {
			return sp.ChatStreamWithSchema(ctx, messages, tools, a.cfg.ResponseSchema)
		}
	}
	return provider.ChatStream(ctx, messages, tools)
}

// ── File resolution ──────────────────────────────────────────────────

// resolveFiles walks messages and resolves FileContent blocks with empty Data.
// For each FileContent, it resolves the URI via scheme-matched Resolver, then
// checks the provider's ContentNegotiator — if the media type is native, the
// FileContent is kept; otherwise, it is converted via an Extractor.
func (a *Agent) resolveFiles(ctx context.Context, messages []types.Message) []types.Message {
	if len(a.cfg.Resolvers) == 0 {
		return messages
	}

	// Determine native content support from the provider.
	var support types.ContentSupport
	if cn, ok := a.cfg.Provider.(types.ContentNegotiator); ok {
		support = cn.ContentSupport()
	}

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
				replaced = append(replaced, c) // keep unresolved
				continue
			}

			resolved, err := resolver.Resolve(ctx, fc.URI)
			if err != nil {
				replaced = append(replaced, c) // keep on error
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
			}

			// Fallback: keep the resolved FileContent.
			replaced = append(replaced, fc)
		}

		out = append(out, types.UserMessage{Content: replaced})
	}
	return out
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

// LoadTreeFromStore reconstructs a conversation tree from a Store by loading the
// subtree rooted at rootID and rebuilding it via tree.FromStore. The returned
// tree can be passed to NewAgent via WithTree to resume a persisted session. The
// active branch defaults to "main" unless overridden.
//
// This is the read counterpart to WithStore's write path. It is a free function
// (not a method) so a tree can be hydrated before an Agent exists.
func LoadTreeFromStore(ctx context.Context, store types.Store, rootID types.NodeID, active types.BranchID) (*tree.Tree, error) {
	if store == nil {
		return nil, fmt.Errorf("agent: nil store")
	}
	nodes, branches, err := store.LoadTree(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("load tree: %w", err)
	}
	if active == "" {
		active = types.BranchID("main")
	}
	return tree.FromStore(nodes, branches, nil, rootID, active)
}

// ── Run loop ─────────────────────────────────────────────────────────

func (a *Agent) runLoop(ctx context.Context, stream *EventStream, input []types.Message, branch types.BranchID) {
	log := a.cfg.Logger
	start := time.Now()
	log.Debug("agent loop started", "agent", a.cfg.Name, "branch", branch)

	defer func() {
		stream.send(types.DoneDelta{})
		stream.close(nil)
		a.cfg.Metrics.RecordAgentInvocation(ctx, a.cfg.Name, time.Since(start))
		log.Debug("agent loop finished", "agent", a.cfg.Name, "elapsed", time.Since(start))
	}()

	tr := a.cfg.Tree

	if !a.appendInput(ctx, tr, stream, branch, input) {
		return
	}

	var handoffCount int

	// pendingWork is true when the previous iteration executed tool calls and the
	// loop continued, meaning the assistant still owes a turn to consume those
	// results. If the iteration cap then fires, the run was truncated mid-task and
	// we emit ErrMaxIterations so consumers can tell "finished" from "cut off".
	pendingWork := false

	for iterCount := 0; ; iterCount++ {
		select {
		case <-ctx.Done():
			stream.send(types.ErrorDelta{Error: types.ErrStreamCanceled})
			return
		default:
		}

		// Flatten the branch to get current message history.
		messages, err := tr.FlattenBranch(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}

		// Resolve config + active agent (handoff group only). With no group this
		// is byte-for-byte the non-handoff path.
		resolved, llmMessages := a.prepareMessages(messages)
		active := a.resolveActive(&resolved, llmMessages)

		// Check iteration cap. If we break here while the last assistant turn left
		// tool calls pending (pendingWork), the run was truncated, not finished —
		// emit ErrMaxIterations so consumers can distinguish the two. A clean
		// natural finish (text-only turn, empty response) clears pendingWork and
		// does NOT emit it.
		if iterCount >= resolved.maxIter {
			if pendingWork {
				stream.send(types.ErrorDelta{Error: types.ErrMaxIterations})
			}
			break
		}

		// Resolve file URIs to data.
		active.messages = a.resolveFiles(ctx, active.messages)

		// Compact if configured: summarize, fork a new branch off root, continue.
		if newBranch, compacted := a.tryCompact(ctx, log, resolved, active.messages, tr); compacted {
			branch = newBranch
			continue // re-flatten from the new branch
		}

		// Get the assistant message as a durable step (provider call + aggregation).
		stepName := fmt.Sprintf("llm-%s-%d", branch, iterCount)
		msg, usage, llmErr := a.getAssistantMessage(ctx, stream, active.provider, active.messages, active.toolDefs, stepName)
		if llmErr != nil {
			log.Error("provider call failed", "error", llmErr, "iteration", iterCount)
			stream.send(types.ErrorDelta{Error: llmErr})
			return
		}

		// Emit enriched usage delta (carries CacheHit + response metadata + latency).
		enriched := types.UsageDelta{}
		if usage != nil {
			enriched = *usage
		}
		stream.send(enriched)

		if msg == nil {
			break // empty response: a clean (if degenerate) finish
		}

		// Persist assistant message to tree.
		tip, err := tr.Tip(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
		assistantNode, err := tr.AddChild(ctx, tip.ID, *msg)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
		a.persistNode(ctx, assistantNode)

		toolCalls := assistantToolCalls(msg)
		if len(toolCalls) == 0 {
			break // text-only turn: the assistant finished naturally
		}

		// Execute all tool calls using the ACTIVE agent's tool registry. The
		// assistant now owes a turn to consume those results (pendingWork), so the
		// iteration cap can distinguish a truncated run from a clean finish.
		// Persist results BEFORE any handoff so every tool_use gets a matching
		// tool_result (provider contract) and rich Blocks are persisted.
		results := a.executeToolsConcurrently(ctx, stream, toolCalls, active.tools)
		pendingWork = true
		if err := a.persistToolResults(ctx, tr, branch, results); err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}

		// Handoff post-check: first signal wins; self-handoff is a no-op. On a
		// handoff the next iteration re-flattens and resolves the new active
		// agent; with no handoff it re-flattens to process the tool results — so
		// both simply fall through to the next iteration.
		if a.applyHandoff(ctx, tr, stream, branch, results, active.name, &handoffCount) {
			return
		}
	}
}

// appendInput appends input messages as child nodes on the branch. Returns false
// (after streaming an ErrorDelta) if a tree write fails.
func (a *Agent) appendInput(ctx context.Context, tr *tree.Tree, stream *EventStream, branch types.BranchID, input []types.Message) bool {
	for _, msg := range input {
		tip, err := tr.Tip(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return false
		}
		node, err := tr.AddChild(ctx, tip.ID, msg)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return false
		}
		a.persistNode(ctx, node)
	}
	return true
}

// activeContext is the per-iteration agent selection (provider, tools, persona).
type activeContext struct {
	provider types.Provider
	toolDefs []types.ToolDef
	tools    *types.ToolRegistry
	name     string
	messages []types.Message
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
	}
	member := a.activeMember(resolved.activeAgent)
	if member == nil {
		return ac
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
		trc := types.ToolResultContent{ToolCallID: r.toolCallID, Text: r.result, Blocks: r.blocks}
		if r.err != "" {
			trc.IsError = true
			if trc.Text == "" {
				trc.Text = r.err
			}
		}
		contents[i] = trc
	}
	tip, err := tr.Tip(branch)
	if err != nil {
		return err
	}
	node, err := tr.AddChild(ctx, tip.ID, types.NewToolResultMessage(contents...))
	if err != nil {
		return err
	}
	a.persistNode(ctx, node)
	return nil
}

// applyHandoff applies the first handoff signal in results, if any, appending a
// HandoffContent overlay on the same branch. It returns fatal=true (after
// streaming an ErrorDelta) when the caller should return — the handoff limit was
// exceeded or a tree write failed.
func (a *Agent) applyHandoff(ctx context.Context, tr *tree.Tree, stream *EventStream, branch types.BranchID, results []toolResult, activeName string, handoffCount *int) (fatal bool) {
	handoffTo := ""
	for _, r := range results {
		if r.handoffTo != "" {
			handoffTo = r.handoffTo
			break
		}
	}
	if handoffTo == "" || handoffTo == activeName {
		return false
	}
	// Enforce the bound BEFORE any side effect, so the over-limit transfer is
	// neither streamed nor persisted (which would poison the branch).
	if *handoffCount >= a.cfg.MaxHandoffs {
		stream.send(types.ErrorDelta{Error: ErrHandoffLimitExceeded})
		return true
	}
	*handoffCount++
	stream.send(types.HandoffDelta{From: activeName, To: handoffTo})
	overlay := types.SystemMessage{Content: []types.SystemContent{
		types.HandoffContent{From: activeName, To: handoffTo},
	}}
	tip, err := tr.Tip(branch)
	if err != nil {
		stream.send(types.ErrorDelta{Error: err})
		return true
	}
	node, err := tr.AddChild(ctx, tip.ID, overlay)
	if err != nil {
		stream.send(types.ErrorDelta{Error: err})
		return true
	}
	a.persistNode(ctx, node)
	return false
}

// getAssistantMessage runs the provider call + aggregation as one durable step.
// Under the default NoopStepRunner the closure runs inline and deltas stream
// live to `stream`. Under a durable runner, on replay the recorded
// AssistantMessage is returned WITHOUT a provider call, and its blocks are
// re-emitted to the stream so consumers see a consistent event sequence.
func (a *Agent) getAssistantMessage(
	ctx context.Context, stream *EventStream,
	provider types.Provider, llmMessages []types.Message, toolDefs []types.ToolDef, stepName string,
) (*types.AssistantMessage, *types.UsageDelta, error) {
	var (
		liveUsage *types.UsageDelta // captured if the provider emitted usage
		ran       bool              // true iff the closure actually executed (not replayed)
	)

	start := time.Now()
	res, err := a.cfg.StepRunner.RunStep(ctx, stepName, func(stepCtx context.Context) (types.StepResult, error) {
		ran = true
		// Bound the provider call (and its stream aggregation) with a child
		// deadline so a slow provider is cancelled rather than hanging the loop.
		if a.cfg.LLMTimeout > 0 {
			var cancel context.CancelFunc
			stepCtx, cancel = context.WithTimeout(stepCtx, a.cfg.LLMTimeout)
			defer cancel()
		}
		llmStart := time.Now()
		rx, llmErr := a.callProvider(stepCtx, provider, llmMessages, toolDefs)
		if llmErr != nil {
			a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", types.ProviderName(provider), time.Since(llmStart), llmErr)
			return types.StepResult{}, llmErr
		}
		agg := NewDefaultAggregator()
		var streamErr error
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
				stream.send(delta) // live streaming (no-op runner path)
				agg.Push(delta)
			}
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
		providerName := types.ProviderName(provider)
		a.cfg.Metrics.RecordProviderCall(stepCtx, "chat", providerName, time.Since(llmStart), nil)
		// (token metric) Record token usage once per completed LLM call with the
		// merged prompt/completion counts. Skipped for cache hits (no new tokens
		// were produced) and when the provider reported no usage at all.
		if liveUsage != nil && !liveUsage.CacheHit &&
			(liveUsage.PromptTokens > 0 || liveUsage.CompletionTokens > 0) {
			a.cfg.Metrics.RecordTokenUsage(stepCtx, "chat", providerName,
				liveUsage.PromptTokens, liveUsage.CompletionTokens)
		}
		var msg *types.AssistantMessage
		if m, ok := agg.Message().(types.AssistantMessage); ok {
			msg = &m
		}
		return types.StepResult{Kind: types.StepKindLLM, Message: msg}, nil
	})
	if err != nil {
		return nil, nil, err
	}

	latency := time.Since(start)

	// On REPLAY the closure never ran, so nothing streamed; re-emit the recorded
	// message's blocks so the consumer's view matches a live run.
	if !ran && res.Message != nil {
		replayAssistantBlocks(stream, *res.Message)
	}

	usage := liveUsage
	if usage == nil {
		usage = &types.UsageDelta{}
	}
	usage.Latency = latency
	return res.Message, usage, nil
}

// toolResult collects the outcome of a single tool execution.
type toolResult struct {
	toolCallID string
	result     string                  // text projection
	blocks     []types.ToolResultBlock // rich multi-modal output (nil for plain tools)
	err        string
	handoffTo  string // non-empty when a HandoffSignaler tool fired
}

// executeToolsConcurrently runs all tool calls, streaming deltas as they arrive.
// Results are returned in the same order as toolCalls. Tools are looked up in the
// active registry (which differs per agent during a handoff).
//
// With the default NoopStepRunner tools run in parallel goroutines (today's
// behavior). With a durable StepRunner, tools run SEQUENTIALLY in the caller's
// goroutine: durable engines correlate steps to the workflow's calling context,
// so fanning out RunStep across goroutines would race on per-workflow step state.
func (a *Agent) executeToolsConcurrently(ctx context.Context, stream *EventStream, toolCalls []types.ToolUseContent, tools *types.ToolRegistry) []toolResult {
	results := make([]toolResult, len(toolCalls))

	if _, isNoop := a.cfg.StepRunner.(types.NoopStepRunner); !isNoop {
		for i, tc := range toolCalls {
			results[i] = a.executeOneTool(ctx, stream, tc, tools)
		}
		return results
	}

	// Optional concurrency cap: a buffered channel acts as a counting semaphore.
	// A slot is acquired before each tool runs and released after, so at most
	// MaxParallelTools goroutines execute a tool simultaneously. 0 = unlimited.
	var sem chan struct{}
	if a.cfg.MaxParallelTools > 0 {
		sem = make(chan struct{}, a.cfg.MaxParallelTools)
	}

	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		wg.Add(1)
		go func(idx int, tc types.ToolUseContent) {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}
			results[idx] = a.executeOneTool(ctx, stream, tc, tools)
		}(i, tc)
	}
	wg.Wait()
	return results
}

// executeOneTool runs a single tool call to completion, emitting the start/end
// deltas and returning its result. Every exit path sets the result's ToolCallID
// and emits a terminal ToolExecEndDelta, so a cancelled or rejected call never
// leaves a zero-valued result that would persist a tool_result with no matching
// tool_use ID.
func (a *Agent) executeOneTool(ctx context.Context, stream *EventStream, tc types.ToolUseContent, tools *types.ToolRegistry) toolResult {
	stream.send(types.ToolExecStartDelta{ToolCallID: tc.ID, Name: tc.Name})

	tool, found := tools.Get(tc.Name)
	if !found {
		res := toolResult{toolCallID: tc.ID, err: fmt.Sprintf("tool not found: %s", tc.Name)}
		stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Error: res.err})
		return res
	}

	// Markers — emit MarkerDelta and wait for human resolution.
	if mt, ok := tool.(*types.MarkedTool); ok && len(mt.Markers) > 0 {
		stream.send(types.MarkerDelta{
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Arguments:  tc.Arguments,
			Markers:    mt.Markers,
		})

		resCh := stream.awaitResolution(tc.ID)
		select {
		case r := <-resCh:
			if !r.Approved {
				msg := "rejected"
				if r.Message != "" {
					msg = "rejected: " + r.Message
				}
				res := toolResult{toolCallID: tc.ID, err: msg}
				stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Error: res.err})
				return res
			}
			if r.ModifiedArgs != nil {
				tc.Arguments = r.ModifiedArgs
			}
		case <-ctx.Done():
			// Must still produce a matching, well-formed tool_result.
			res := toolResult{toolCallID: tc.ID, err: "context cancelled"}
			stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Error: res.err})
			return res
		}
		tool = mt.Inner
	}

	// Handoff signal: a control transfer, not a normal result. Checked before
	// SubAgentInvoker and never wrapped in a durable step.
	if h, ok := tool.(HandoffSignaler); ok {
		target := h.HandoffTarget()
		out, _ := tool.Execute(ctx, tc.Arguments)
		stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Result: out})
		return toolResult{toolCallID: tc.ID, result: out, handoffTo: target}
	}

	// Sub-agent: forward child deltas (kept inline, not a durable step).
	if invoker, ok := tool.(SubAgentInvoker); ok {
		task, _ := tc.Arguments["task"].(string)
		childStream := invoker.InvokeAgent(ctx, task)

		var resultBuf strings.Builder
		for d := range childStream.Deltas() {
			stream.send(types.ToolExecDelta{ToolCallID: tc.ID, Inner: d})
			if tcd, ok := d.(types.TextContentDelta); ok {
				resultBuf.WriteString(tcd.Content)
			}
		}
		res := toolResult{toolCallID: tc.ID, result: resultBuf.String()}
		if err := childStream.Wait(); err != nil {
			res.err = err.Error()
		}
		stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Result: res.result, Error: res.err})
		return res
	}

	// Regular tool execution, wrapped in a durable step. A RichTool yields
	// multi-modal Blocks; a plain Tool yields text only.
	stepName := "tool-" + tc.ID
	sr, stepErr := a.cfg.StepRunner.RunStep(ctx, stepName, func(stepCtx context.Context) (types.StepResult, error) {
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
		if rt, ok := tool.(types.RichTool); ok {
			var tr types.ToolResult
			tr, execErr = rt.ExecuteRich(stepCtx, tc.Arguments)
			text, blocks = tr.Text, tr.Blocks
			if execErr == nil && tr.IsError {
				execErr = errors.New(tr.Text) // tool-signalled error without a Go error
			}
		} else {
			text, execErr = tool.Execute(stepCtx, tc.Arguments)
		}
		// Surface a deadline overrun as a tool error even when the tool ignored
		// stepCtx and returned no error of its own, so the turn can distinguish a
		// timed-out tool from a successful one.
		if execErr == nil && a.cfg.ToolTimeout > 0 && stepCtx.Err() == context.DeadlineExceeded {
			execErr = fmt.Errorf("tool %s exceeded timeout %s: %w", tc.Name, a.cfg.ToolTimeout, context.DeadlineExceeded)
			text, blocks = "", nil
		}
		a.cfg.Metrics.RecordToolCall(stepCtx, tc.Name, time.Since(toolStart), execErr)
		out := types.StepResult{Kind: types.StepKindTool, ToolCallID: tc.ID, ToolResult: text, ToolBlocks: blocks}
		if execErr != nil {
			out.ToolError = execErr.Error() // error-in-payload: recorded once, not retried
		}
		return out, nil
	})

	var res toolResult
	if stepErr != nil {
		// Infrastructure failure from the runner itself (e.g. durable engine
		// error) — surface it as a tool error rather than dropping it.
		res = toolResult{toolCallID: tc.ID, err: stepErr.Error()}
	} else {
		res = toolResult{toolCallID: tc.ID, result: sr.ToolResult, blocks: sr.ToolBlocks, err: sr.ToolError}
	}
	stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Result: res.result, Blocks: res.blocks, Error: res.err})
	return res
}
