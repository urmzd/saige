package agent

import (
	"context"
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

	// File pipeline configuration.
	Resolvers  map[string]types.Resolver            // URI scheme → Resolver (e.g. "file", "https", "s3")
	Extractors map[types.MediaType]types.Extractor    // MediaType → Extractor for non-native types

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

// Agent runs an LLM agent loop with tool execution.
// All conversations are backed by a Tree.
type Agent struct {
	cfg   AgentConfig
	tools *types.ToolRegistry
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = types.NoopMetrics{}
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

	return &Agent{cfg: cfg, tools: tools}
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
		// Skip internal delegate tools from the tool list — they show as sub-agents.
		if !strings.HasPrefix(td.Name, "delegate_to_") {
			info.Tools = append(info.Tools, td.Name)
		}
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

// ── Config resolution ────────────────────────────────────────────────

// resolvedConfig holds the effective configuration for a single iteration,
// derived by walking all ConfigContent blocks in the tree.
type resolvedConfig struct {
	model      string
	maxIter    int
	compactor  types.Compactor
	compactNow bool
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
				if cc, ok := c.(types.ConfigContent); ok {
					mergeConfig(&rc, cc)
				} else {
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
	}
	if cc.Compact != nil {
		rc.compactor = cc.Compact.ToCompactor()
	}
	if cc.CompactNow {
		rc.compactNow = true
	}
}

// callProvider invokes the LLM, using structured output when available.
func (a *Agent) callProvider(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	if a.cfg.ResponseSchema != nil && len(tools) == 0 {
		if sp, ok := a.cfg.Provider.(types.StructuredOutputProvider); ok {
			return sp.ChatStreamWithSchema(ctx, messages, tools, a.cfg.ResponseSchema)
		}
	}
	return a.cfg.Provider.ChatStream(ctx, messages, tools)
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

	// Append input messages as child nodes on the branch.
	for _, msg := range input {
		tip, err := tr.Tip(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
		if _, err := tr.AddChild(ctx, tip.ID, msg); err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
	}

	toolDefs := a.tools.Definitions()

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

		// Resolve config and strip metadata in a single pass.
		resolved, llmMessages := a.prepareMessages(messages)

		// Check iteration cap.
		if iterCount >= resolved.maxIter {
			break
		}

		// Resolve file URIs to data.
		llmMessages = a.resolveFiles(ctx, llmMessages)

		// Compact if configured.
		if resolved.compactNow || resolved.compactor != nil {
			if resolved.compactor != nil {
				compacted, err := resolved.compactor.Compact(ctx, llmMessages, a.cfg.Provider)
				if err == nil {
					llmMessages = compacted
				}
			}
		}

		// Call LLM + timing.
		llmStart := time.Now()
		rx, llmErr := a.callProvider(ctx, llmMessages, toolDefs)
		if llmErr != nil {
			log.Error("provider call failed", "error", llmErr, "iteration", iterCount)
			a.cfg.Metrics.RecordProviderCall(ctx, types.ProviderName(a.cfg.Provider), time.Since(llmStart), llmErr)
			stream.send(types.ErrorDelta{Error: llmErr})
			return
		}

		// Accumulate response, capture UsageDelta from provider.
		agg := NewDefaultAggregator()
		var usage *types.UsageDelta
		for delta := range rx {
			if u, ok := delta.(types.UsageDelta); ok {
				usage = &u
				continue
			}
			stream.send(delta)
			agg.Push(delta)
		}

		// Emit enriched usage delta.
		latency := time.Since(llmStart)
		a.cfg.Metrics.RecordProviderCall(ctx, types.ProviderName(a.cfg.Provider), latency, nil)
		enriched := types.UsageDelta{Latency: latency}
		if usage != nil {
			enriched.PromptTokens = usage.PromptTokens
			enriched.CompletionTokens = usage.CompletionTokens
			enriched.TotalTokens = usage.TotalTokens
		}
		stream.send(enriched)

		msg := agg.Message()
		if msg == nil {
			break
		}

		// Persist assistant message to tree.
		tip, err := tr.Tip(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
		if _, err := tr.AddChild(ctx, tip.ID, msg); err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}

		// Check for tool calls.
		assistantMsg, ok := msg.(types.AssistantMessage)
		if !ok {
			break
		}

		var toolCalls []types.ToolUseContent
		for _, block := range assistantMsg.Content {
			if tc, ok := block.(types.ToolUseContent); ok {
				toolCalls = append(toolCalls, tc)
			}
		}

		if len(toolCalls) == 0 {
			break
		}

		// Execute all tool calls in parallel.
		results := a.executeToolsConcurrently(ctx, stream, toolCalls)

		// Build a single SystemMessage with all tool results and persist.
		toolResultContents := make([]types.ToolResultContent, len(results))
		for i, r := range results {
			trc := types.ToolResultContent{
				ToolCallID: r.toolCallID,
				Text:       r.result,
			}
			if r.err != "" {
				trc.IsError = true
				if trc.Text == "" {
					trc.Text = r.err
				}
			}
			toolResultContents[i] = trc
		}

		toolResultMsg := types.NewToolResultMessage(toolResultContents...)
		tip, err = tr.Tip(branch)
		if err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
		if _, err := tr.AddChild(ctx, tip.ID, toolResultMsg); err != nil {
			stream.send(types.ErrorDelta{Error: err})
			return
		}
	}
}

// toolResult collects the outcome of a single tool execution.
type toolResult struct {
	toolCallID string
	result     string
	err        string
}

// executeToolsConcurrently runs all tool calls in parallel, streaming deltas
// as they arrive. Results are returned in the same order as toolCalls.
func (a *Agent) executeToolsConcurrently(ctx context.Context, stream *EventStream, toolCalls []types.ToolUseContent) []toolResult {
	results := make([]toolResult, len(toolCalls))
	var wg sync.WaitGroup

	for i, tc := range toolCalls {
		wg.Add(1)
		go func(idx int, tc types.ToolUseContent) {
			defer wg.Done()

			stream.send(types.ToolExecStartDelta{ToolCallID: tc.ID, Name: tc.Name})

			tool, found := a.tools.Get(tc.Name)
			if !found {
				results[idx] = toolResult{
					toolCallID: tc.ID,
					err:        fmt.Sprintf("tool not found: %s", tc.Name),
				}
				stream.send(types.ToolExecEndDelta{
					ToolCallID: tc.ID,
					Error:      results[idx].err,
				})
				return
			}

			// Check for markers — if present, emit MarkerDelta and wait for resolution.
			if mt, ok := tool.(*types.MarkedTool); ok && len(mt.Markers) > 0 {
				stream.send(types.MarkerDelta{
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Arguments:  tc.Arguments,
					Markers:    mt.Markers,
				})

				resCh := stream.awaitResolution(tc.ID)
				select {
				case res := <-resCh:
					if !res.Approved {
						msg := "rejected"
						if res.Message != "" {
							msg = "rejected: " + res.Message
						}
						results[idx] = toolResult{
							toolCallID: tc.ID,
							err:        msg,
						}
						stream.send(types.ToolExecEndDelta{ToolCallID: tc.ID, Error: results[idx].err})
						return
					}
					if res.ModifiedArgs != nil {
						tc.Arguments = res.ModifiedArgs
					}
				case <-ctx.Done():
					return
				}

				// Use the inner tool for execution.
				tool = mt.Inner
			}

			// Check if this is a sub-agent — if so, forward child deltas.
			if invoker, ok := tool.(SubAgentInvoker); ok {
				task, _ := tc.Arguments["task"].(string)
				childStream := invoker.InvokeAgent(ctx, task)

				var resultBuf strings.Builder
				for d := range childStream.Deltas() {
					// Forward child deltas wrapped with attribution.
					stream.send(types.ToolExecDelta{
						ToolCallID: tc.ID,
						Inner:      d,
					})
					if tcd, ok := d.(types.TextContentDelta); ok {
						resultBuf.WriteString(tcd.Content)
					}
				}
				result := resultBuf.String()

				errStr := ""
				if err := childStream.Wait(); err != nil {
					errStr = err.Error()
				}
				results[idx] = toolResult{
					toolCallID: tc.ID,
					result:     result,
					err:        errStr,
				}
			} else {
				// Regular tool execution.
				toolStart := time.Now()
				result, execErr := tool.Execute(ctx, tc.Arguments)
				a.cfg.Metrics.RecordToolCall(ctx, tc.Name, time.Since(toolStart), execErr)
				errStr := ""
				if execErr != nil {
					errStr = execErr.Error()
				}
				results[idx] = toolResult{
					toolCallID: tc.ID,
					result:     result,
					err:        errStr,
				}
			}

			stream.send(types.ToolExecEndDelta{
				ToolCallID: results[idx].toolCallID,
				Result:     results[idx].result,
				Error:      results[idx].err,
			})
		}(i, tc)
	}

	wg.Wait()
	return results
}
