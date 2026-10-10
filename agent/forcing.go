package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// MaxIterPolicy chooses what a run does when it reaches a step limit with
// work still pending: the iteration cap, MaxConsecutiveErrors, or
// MaxRepeatIterations.
type MaxIterPolicy int

const (
	// MaxIterError ends the run with the limit's error, such as
	// types.ErrMaxIterations. It is the default.
	MaxIterError MaxIterPolicy = iota
	// MaxIterForceFinal makes one more model call with tools disabled and an
	// instruction to answer now, and records that reply as the final answer.
	// The run then ends without an error. If the call fails or produces no
	// text, the limit's error is returned.
	MaxIterForceFinal
)

// DefaultForceFinalPrompt is the instruction sent with the forced final call.
// It is sent with that request only and is not added to the tree.
const DefaultForceFinalPrompt = "You have reached the step limit for this task. Do not call any tools. " +
	"Summarize the progress so far and give your best final answer now."

// DefaultMaxConsecutiveErrors is the number of consecutive turns in which
// every tool call fails before the run stops. A call that a gate or a person
// refused is not a failure.
const DefaultMaxConsecutiveErrors = 2

var (
	// ErrToolErrorLimit means every tool call failed for
	// MaxConsecutiveErrors consecutive turns.
	ErrToolErrorLimit = errors.New("too many consecutive failed tool turns")
	// ErrRepeatedToolCalls means the model requested the same tool calls with
	// the same arguments more often in a row than MaxRepeatIterations allows.
	ErrRepeatedToolCalls = errors.New("repeated identical tool calls")
)

// WithOnMaxIter sets what happens when a run reaches a step limit.
func WithOnMaxIter(p MaxIterPolicy) AgentOption {
	return func(c *AgentConfig) { c.OnMaxIter = p }
}

// WithStopAtTools ends the run as soon as one of the named tools succeeds.
// The tool's result becomes the run's output.
func WithStopAtTools(names ...string) AgentOption {
	return func(c *AgentConfig) { c.StopAtTools = append(c.StopAtTools, names...) }
}

// WithMaxConsecutiveErrors stops a run after n consecutive turns in which
// every tool call failed. A negative n disables the check.
func WithMaxConsecutiveErrors(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxConsecutiveErrors = n }
}

// WithMaxRepeatIterations stops a run when the model requests the same tool
// calls with the same arguments more than n turns in a row. 0 disables it.
func WithMaxRepeatIterations(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxRepeatIterations = n }
}

// WithToolChoice sets the tool choice for the agent's turns. A required or
// named choice applies to the first turn of each run; auto and none apply to
// every turn. A ConfigContent.ToolChoice in the conversation takes precedence.
func WithToolChoice(choice types.ToolChoice) AgentOption {
	return func(c *AgentConfig) { c.ToolChoice = &choice }
}

// WithTokenizer sets the tokenizer used for CompactConfig.MaxInputTokens.
func WithTokenizer(t types.Tokenizer) AgentOption {
	return func(c *AgentConfig) { c.Tokenizer = t }
}

// ── Tool choice ──────────────────────────────────────────────────────

// toolChoice returns the choice for the next turn. A choice set in the
// conversation wins. The configured choice applies to every turn when it is
// auto or none, and to the first turn of the run when it forces a call.
func (a *Agent) toolChoice(resolved resolvedConfig, forcedSpent bool) *types.ToolChoice {
	if resolved.toolChoice != nil {
		return resolved.toolChoice
	}
	c := a.cfg.ToolChoice
	if c == nil || (c.Forced() && forcedSpent) {
		return nil
	}
	return c
}

// toolChoiceRequest turns a tool choice into the tools and options for one
// call. Auto needs nothing. None is sent as an option when the provider
// accepts one and declares tool choice; otherwise the tools are withheld,
// which has the same effect. A forced choice must reach the provider, so a
// provider that cannot receive it is a configuration error.
func toolChoiceRequest(provider types.Provider, choice *types.ToolChoice, tools []types.ToolDef) ([]types.ToolDef, *types.RequestOptions, error) {
	if choice == nil || choice.Mode == "" || choice.Mode == types.ToolChoiceAuto {
		if choice != nil {
			if err := choice.Validate(tools); err != nil {
				return nil, nil, err
			}
		}
		return tools, nil, nil
	}
	caps, known := types.ProviderCapabilities(provider)
	_, accepts := provider.(types.OptionsProvider)
	opts := &types.RequestOptions{ToolChoice: choice}
	if choice.Mode == types.ToolChoiceNone {
		if err := choice.Validate(tools); err != nil {
			return nil, nil, err
		}
		// With no tools there is nothing to disable, so no option is sent.
		// That keeps the request on the plain path, which also carries a
		// response schema.
		if len(tools) > 0 && accepts && known && caps.Supports(types.CapToolChoice) {
			return tools, opts, nil
		}
		return nil, nil, nil
	}
	if known {
		if err := caps.ValidateToolChoice(choice, tools); err != nil {
			return nil, nil, err
		}
	} else if err := choice.Validate(tools); err != nil {
		return nil, nil, err
	}
	if !accepts {
		return nil, nil, fmt.Errorf("%w: tool_choice %q: provider %q does not accept request options",
			types.ErrInvalidModelConfig, choice.Mode, types.ProviderName(provider))
	}
	return tools, opts, nil
}

// ── Stop at tool ─────────────────────────────────────────────────────

// stopToolCall returns the ID of the first call, in request order, to a tool
// listed in StopAtTools that succeeded. A failed call does not stop the run,
// so the model can recover from it.
func (a *Agent) stopToolCall(calls []types.ToolUseContent, results []toolResult, out runOutput) string {
	if len(a.cfg.StopAtTools) == 0 && !out.tool() {
		return ""
	}
	for i, call := range calls {
		stops := slices.Contains(a.cfg.StopAtTools, call.Name) || (out.tool() && call.Name == FinalAnswerToolName)
		if stops && i < len(results) && results[i].err == "" {
			return call.ID
		}
	}
	return ""
}

// ── Loop guards ──────────────────────────────────────────────────────

// loopGuards detects runs that make no progress: turns in which every tool
// call fails, and turns that repeat the previous turn's calls exactly.
type loopGuards struct {
	failedTurns int
	repeats     int
	lastCalls   string
	tripped     error
}

// repeated records a turn's tool calls and reports whether they exceed limit
// identical turns in a row. limit 0 disables the check.
func (g *loopGuards) repeated(calls []types.ToolUseContent, limit int) bool {
	sig := callSignature(calls)
	if sig == g.lastCalls {
		g.repeats++
	} else {
		g.lastCalls, g.repeats = sig, 1
	}
	if limit > 0 && g.repeats > limit {
		g.tripped = fmt.Errorf("%w: %d turns in a row", ErrRepeatedToolCalls, g.repeats)
		return true
	}
	return false
}

// recordResults counts turns in which every call failed. A call refused by a
// gate, a denied approval, or a rejected marker is a policy outcome, not a
// tool fault: a turn of only refusals leaves the count unchanged. limit 0 uses
// DefaultMaxConsecutiveErrors and a negative limit disables the check.
func (g *loopGuards) recordResults(results []toolResult, limit int) {
	if limit == 0 {
		limit = DefaultMaxConsecutiveErrors
	}
	failed, refused := 0, 0
	for _, r := range results {
		switch {
		case r.refused:
			refused++
		case r.err != "":
			failed++
		}
	}
	if refused == len(results) {
		return
	}
	if failed+refused < len(results) {
		g.failedTurns = 0
		return
	}
	g.failedTurns++
	if limit > 0 && g.failedTurns >= limit {
		g.tripped = fmt.Errorf("%w: %d turns", ErrToolErrorLimit, g.failedTurns)
	}
}

// callSignature identifies a turn's tool calls by name and arguments.
// encoding/json sorts map keys, so equal arguments encode equally.
func callSignature(calls []types.ToolUseContent) string {
	var b strings.Builder
	for _, c := range calls {
		args, _ := json.Marshal(c.Arguments)
		b.WriteString(c.Name)
		b.WriteByte(0)
		b.Write(args)
		b.WriteByte('\n')
	}
	return b.String()
}

// skipToolCalls answers every call with an error result without running it,
// so the turn's tool calls stay paired with results.
func skipToolCalls(stream *EventStream, calls []types.ToolUseContent, reason string) []toolResult {
	results := make([]toolResult, len(calls))
	for i, c := range calls {
		stream.send(types.ToolExecStartDelta{ToolCallID: c.ID, Name: c.Name})
		results[i] = failedTool(stream, c.ID, c.Name, reason)
	}
	return results
}

// ── Limits and the forced final answer ───────────────────────────────

// finishAtLimit ends a run that reached a step limit with work pending.
func (a *Agent) finishAtLimit(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, active activeContext, limitErr error) error {
	if a.cfg.OnMaxIter != MaxIterForceFinal {
		return limitErr
	}
	a.cfg.Logger.Info("step limit reached, forcing a final answer", "agent", a.cfg.Name, "reason", limitErr)
	if a.cfg.Budget != nil {
		if err := a.cfg.Budget.Err(); err != nil {
			return err
		}
	}
	prompt := a.cfg.ForceFinalPrompt
	if prompt == "" {
		prompt = DefaultForceFinalPrompt
	}
	messages := append(slices.Clone(active.messages), types.NewUserMessage(prompt))

	// A response schema is applied by the tool-free request path, so the
	// tools are withheld rather than disabled by option.
	tools, opts := []types.ToolDef(nil), (*types.RequestOptions)(nil)
	if !a.output(ctx).native() {
		var err error
		tools, opts, err = toolChoiceRequest(active.provider, &types.ToolChoice{Mode: types.ToolChoiceNone}, active.toolDefs)
		if err != nil {
			return err
		}
	}
	opts, err := a.attachDials(ctx, active, opts, tools)
	if err != nil {
		return err
	}
	msg, usage, err := a.getAssistantMessage(ctx, stream, active.provider, messages, tools, opts, fmt.Sprintf("llm-%s-final", branch))
	if err != nil {
		return err
	}
	if err := a.reportUsage(ctx, stream, active.provider, usage); err != nil {
		return err
	}
	if done, err := a.handleTruncation(ctx, stream, tr, branch, msg, usage); done {
		return err
	}
	if msg == nil {
		return limitErr
	}
	// The model was told not to call tools. A call it makes anyway is not
	// run, and is dropped so the recorded answer has no unanswered call.
	final := withoutToolCalls(*msg)
	if len(final.Content) == 0 {
		return limitErr
	}
	if _, err := a.appendNode(ctx, tr, branch, final); err != nil {
		return err
	}
	stream.forced, stream.forcedReason = true, limitErr.Error()
	return nil
}

// withoutToolCalls returns msg without its tool-use blocks.
func withoutToolCalls(msg types.AssistantMessage) types.AssistantMessage {
	content := make([]types.AssistantContent, 0, len(msg.Content))
	for _, c := range msg.Content {
		if _, ok := c.(types.ToolUseContent); !ok {
			content = append(content, c)
		}
	}
	return types.AssistantMessage{Content: content}
}

// ── Output truncation ────────────────────────────────────────────────

// truncationReason returns types.FinishReasonMaxTokens when the output token
// limit cut the turn short, whatever the provider called it, or "".
func truncationReason(usage *types.UsageDelta) string {
	if usage == nil {
		return ""
	}
	for _, r := range usage.FinishReasons {
		if types.IsTruncationFinishReason(r) {
			return types.FinishReasonMaxTokens
		}
	}
	return ""
}

// errTruncatedToolCall is the argument error recorded for a tool call that
// was still streaming when the output limit was reached.
const errTruncatedToolCall = "arguments cut off by the output token limit"

// handleTruncation commits a turn that the output token limit cut short. The
// completed text is kept and a TruncatedDelta names its node and the finish
// reason. Tool calls in the turn are never run: their arguments may be
// incomplete. When the turn had tool calls the run ends with a
// *types.ResponseTruncatedError; a text-only turn ends the run cleanly. done
// reports whether the turn was truncated.
//
// The committed node carries a TruncationContent marker with the reason. The
// marker is stripped before the next provider call.
func (a *Agent) handleTruncation(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, msg *types.AssistantMessage, usage *types.UsageDelta) (bool, error) {
	reason := truncationReason(usage)
	if reason == "" {
		return false, nil
	}
	var partial types.AssistantMessage
	dropped := 0
	if msg != nil {
		partial = withoutToolCalls(*msg)
		dropped = len(msg.Content) - len(partial.Content)
	}
	nodeID := ""
	if len(partial.Content) > 0 {
		node, err := a.appendNode(ctx, tr, branch, withTruncationMarker(partial, reason))
		if err != nil {
			return true, err
		}
		nodeID = string(node.ID)
	}
	stream.send(types.TruncatedDelta{NodeID: nodeID, Reason: reason})
	a.cfg.Logger.Warn("response truncated by the output token limit",
		"agent", a.cfg.Name, "finish_reason", reason, "dropped_tool_calls", dropped)
	if dropped == 0 {
		return true, nil
	}
	return true, &types.ResponseTruncatedError{FinishReason: reason, OutputTokens: usage.CompletionTokens}
}
