package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// maxOverflowRetries bounds how many times one turn is compacted and retried
// after the provider reports that the input exceeded the context window.
const maxOverflowRetries = 3

// maxPressureCompactions bounds how many times one turn is compacted before
// it is sent because its input exceeds CompactConfig.MaxInputTokens.
const maxPressureCompactions = 5

// overflowState tracks context pressure across the turns of one run.
type overflowState struct {
	// lastPromptTokens is the input size the provider reported for the
	// previous turn. It is reset after a compaction, because the history it
	// measured no longer exists.
	lastPromptTokens int
	// retries counts compactions made for the current turn after a
	// context-length error; original is the first such error.
	retries  int
	original error
	// compacted is true once the current turn has been compacted, so a turn
	// that is still over the limit is sent rather than compacted again.
	compacted bool
	// pressure is true while the current turn is being compacted because its
	// input exceeds CompactConfig.MaxInputTokens and the last compaction
	// shrank it. pressureSize is the input size measured before that
	// compaction, and pressureRounds counts the compactions made.
	pressure       bool
	pressureSize   int
	pressureRounds int
	// discardedTip is the branch tip at which a summarizing compaction was
	// last rejected because it would split a tool call from its result. The
	// same history is not tried again until new nodes are added.
	discardedTip types.NodeID
}

// turnSucceeded records a completed provider call and starts a new turn.
func (s *overflowState) turnSucceeded(usage *types.UsageDelta) {
	s.retries, s.original, s.compacted = 0, nil, false
	s.pressure, s.pressureSize, s.pressureRounds = false, 0, 0
	if usage != nil && usage.PromptTokens > 0 {
		s.lastPromptTokens = usage.PromptTokens
	}
}

// canCompact reports whether the current turn may be compacted before it is
// sent: once, or again while input pressure compaction keeps making progress.
func (s *overflowState) canCompact() bool {
	return !s.compacted || s.pressure
}

// tokenizer returns the configured tokenizer or the local estimator.
func (a *Agent) tokenizer() types.Tokenizer {
	if a.cfg.Tokenizer != nil {
		return a.cfg.Tokenizer
	}
	return types.EstimatingTokenizer{}
}

// inputSize is the size of the next turn's input: the larger of the
// provider's last report and an estimate of messages. The report is exact but
// lags by the tool results added since, and the estimate covers the first
// turn, before any report exists.
func (a *Agent) inputSize(ctx context.Context, st *overflowState, messages []types.Message) int {
	size := st.lastPromptTokens
	if n, err := a.tokenizer().CountTokens(ctx, messages); err != nil {
		a.cfg.Logger.Warn("token count failed, using the last reported input size", "agent", a.cfg.Name, "error", err)
	} else {
		size = max(size, n)
	}
	return size
}

// compactIfNeeded compacts the branch before a turn when the configured
// trigger fires. With CompactConfig.MaxInputTokens set, the trigger is input
// pressure (or an explicit CompactNow); otherwise the configured compactor
// decides from the message count. Under input pressure the turn is compacted
// again until it fits, until a compaction no longer shrinks it, or until
// maxPressureCompactions. A non-nil error ends the run.
func (a *Agent) compactIfNeeded(ctx context.Context, stream *EventStream, st *overflowState, resolved resolvedConfig, active activeContext, tr *tree.Tree, branch types.BranchID) (types.BranchID, bool, error) {
	cfg := resolved.compactCfg
	if cfg == nil || cfg.MaxInputTokens <= 0 {
		if resolved.compactor == nil {
			if resolved.compactNow {
				a.cfg.Logger.Warn("compactNow requested but no compactor configured, ignoring")
			}
			return "", false, nil
		}
		return a.runCompaction(ctx, stream, st, resolved, active, tr, branch, false)
	}
	if st.compacted && !st.pressure {
		return "", false, nil
	}
	size := a.inputSize(ctx, st, active.messages)
	if st.pressure && (size >= st.pressureSize || st.pressureRounds >= maxPressureCompactions) {
		// The last compaction made no progress, or the rounds are spent:
		// send the turn as it is.
		st.pressure = false
		return "", false, nil
	}
	if size <= cfg.MaxInputTokens && (st.pressure || !resolved.compactNow) {
		st.pressure = false
		return "", false, nil
	}
	newBranch, ok, err := a.runCompaction(ctx, stream, st, resolved, active, tr, branch, resolved.compactNow && !st.pressure)
	if err != nil || !ok {
		st.pressure = false
		return newBranch, ok, err
	}
	st.pressure, st.pressureSize = true, size
	st.pressureRounds++
	return newBranch, true, nil
}

// recoverOverflow handles a provider error. For a context-length error with
// compaction configured, it compacts the branch and reports retry so the turn
// is sent again, up to maxOverflowRetries times per turn. Otherwise it returns
// the error to end the run with: the first context-length error of the turn,
// so a failed recovery reports what the provider originally said.
func (a *Agent) recoverOverflow(ctx context.Context, stream *EventStream, st *overflowState, llmErr error, resolved resolvedConfig, active activeContext, tr *tree.Tree, branch types.BranchID) (types.BranchID, bool, error) {
	if !types.IsContextLength(llmErr) {
		return "", false, llmErr
	}
	if st.original == nil {
		st.original = llmErr
	}
	if resolved.compactCfg == nil || resolved.compactCfg.Strategy == types.CompactNone ||
		a.handoffs != nil || st.retries >= maxOverflowRetries {
		return "", false, st.original
	}
	st.retries++
	newBranch, ok, err := a.runCompaction(ctx, stream, st, resolved, active, tr, branch, true)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, st.original
	}
	a.cfg.Logger.Info("context window exceeded, compacted and retrying",
		"agent", a.cfg.Name, "attempt", st.retries, "branch", newBranch)
	return newBranch, true, nil
}

// runCompaction compacts the branch with the active provider. Every summary
// call is admitted by the budget before it is sent and settled afterwards,
// like a turn. It reports the new branch when the history changed. A failed
// compaction leaves the branch as it was and is logged, not fatal; only a
// budget refusal or stop ends the run.
func (a *Agent) runCompaction(ctx context.Context, stream *EventStream, st *overflowState, resolved resolvedConfig, active activeContext, tr *tree.Tree, branch types.BranchID, force bool) (types.BranchID, bool, error) {
	if a.cfg.Budget != nil {
		if err := a.cfg.Budget.Err(); err != nil {
			return "", false, err
		}
	}
	meter := &meteredProvider{Provider: active.provider, agent: a, stream: stream, step: fmt.Sprintf("compact-%s", branch)}
	var (
		newBranch types.BranchID
		ok        bool
	)
	switch {
	case usesTreeCompaction(resolved.compactCfg, force):
		newBranch, ok = a.treeCompact(ctx, st, tr, branch, meter, resolved.compactCfg.MaxInputTokens, force)
	case resolved.compactor != nil:
		newBranch, ok = a.compactMessages(ctx, a.cfg.Logger, resolved.compactor, meter, active.messages, tr)
	}
	calls, budgetErr := meter.result()
	for i := range calls {
		if err := a.reportUsage(ctx, stream, active.provider, &calls[i]); err != nil {
			return "", false, err
		}
	}
	if budgetErr != nil {
		return "", false, budgetErr
	}
	if !ok {
		return "", false, nil
	}
	st.lastPromptTokens = 0
	st.compacted = true
	return newBranch, true, nil
}

// usesTreeCompaction reports whether the summarizing strategy runs through
// tree.Compact: under a token limit, or when a turn must shrink regardless of
// its size. An empty strategy summarizes; none never makes a summary call.
// Sliding window and tool-result clearing keep their own compactors.
func usesTreeCompaction(cfg *types.CompactConfig, force bool) bool {
	if cfg == nil {
		return false
	}
	switch cfg.Strategy {
	case "", types.CompactSummarize:
		return cfg.MaxInputTokens > 0 || force
	}
	return false
}

// treeCompact summarizes the older half of the branch onto a new active
// branch. A forced compaction ignores the token limit. The split is checked
// before the summary call, so a compaction that would separate a tool call
// from its result costs nothing and is not retried on the same history.
func (a *Agent) treeCompact(ctx context.Context, st *overflowState, tr *tree.Tree, branch types.BranchID, provider types.Provider, limit int, force bool) (types.BranchID, bool) {
	log := a.cfg.Logger
	tip, err := tr.Tip(branch)
	if err != nil {
		log.Warn("compaction skipped", "agent", a.cfg.Name, "error", err)
		return "", false
	}
	if tip.ID == st.discardedTip {
		return "", false
	}
	if err := checkCompactionSplit(tr, branch); err != nil {
		log.Warn("compaction skipped", "agent", a.cfg.Name, "error", err)
		st.discardedTip = tip.ID
		return "", false
	}
	observed := st.lastPromptTokens
	if force {
		limit, observed = 0, 0
	}
	tok := pressureTokenizer{base: a.tokenizer(), observed: observed}
	newBranch, err := tr.Compact(ctx, branch, provider, tok, tree.CompactOpts{MaxTokens: limit, PreserveShared: true})
	if err != nil {
		log.Warn("compaction failed, continuing with full history", "agent", a.cfg.Name, "error", err)
		return "", false
	}
	if newBranch == branch {
		return "", false
	}
	if err := checkToolPairing(tr, newBranch); err != nil {
		// The compacted branch would be rejected by the provider. Keep the
		// original branch active; the unused branch stays in the tree.
		log.Warn("compaction discarded", "agent", a.cfg.Name, "error", err)
		st.discardedTip = tip.ID
		if serr := tr.SetActiveContext(ctx, branch); serr != nil {
			log.Warn("failed to restore active branch after compaction", "agent", a.cfg.Name, "error", serr)
		}
		return "", false
	}
	a.persistBranch(ctx, tr, newBranch)
	log.Debug("compacted to new branch", "agent", a.cfg.Name, "branch", newBranch)
	return newBranch, true
}

// errSplitToolCall reports a compacted history in which a tool result no
// longer follows the call that produced it.
var errSplitToolCall = errors.New("compaction separated a tool result from its tool call")

// checkToolPairing verifies that every tool result on branch answers a tool
// call made earlier on the same branch.
func checkToolPairing(tr *tree.Tree, branch types.BranchID) error {
	msgs, err := tr.FlattenBranch(branch)
	if err != nil {
		return err
	}
	return toolPairingError(msgs)
}

// checkCompactionSplit predicts the history tree.Compact would leave on
// branch and verifies its tool pairing, without calling a provider. It mirrors
// the tree's choice of nodes: the active, non-root nodes that no other branch
// shares, cut where tree.CompactCount says. The summary that replaces them
// holds no tool calls, so only the kept messages are checked. Compact keeps
// tool pairs together, so this is a cheap check for histories it cannot
// split cleanly, such as a branch whose shared prefix ends inside a pair.
func checkCompactionSplit(tr *tree.Tree, branch types.BranchID) error {
	msgs, err := tr.FlattenBranchAnnotated(branch)
	if err != nil {
		return err
	}
	shared := map[types.NodeID]bool{}
	for id, tip := range tr.Branches() {
		if id == branch {
			continue
		}
		path, err := tr.Path(tip)
		if err != nil {
			continue
		}
		for _, n := range path {
			shared[n] = true
		}
	}
	var root types.NodeID
	if r := tr.Root(); r != nil {
		root = r.ID
	}
	var candidates []int
	var candidateMsgs []types.Message
	for i, m := range msgs {
		if m.NodeID == root || m.State != types.NodeActive || shared[m.NodeID] {
			continue
		}
		candidates = append(candidates, i)
		candidateMsgs = append(candidateMsgs, m.Message)
	}
	if len(candidates) == 0 {
		return nil
	}
	first, last := candidates[0], candidates[tree.CompactCount(candidateMsgs)-1]
	kept := make([]types.Message, 0, len(msgs))
	for i, m := range msgs {
		if i < first || i > last {
			kept = append(kept, m.Message)
		}
	}
	return toolPairingError(kept)
}

// toolPairingError reports the first tool result in msgs that does not answer
// a tool call made earlier in msgs.
func toolPairingError(msgs []types.Message) error {
	calls := map[string]bool{}
	for _, m := range msgs {
		switch v := m.(type) {
		case types.AssistantMessage:
			for _, c := range v.Content {
				if tu, ok := c.(types.ToolUseContent); ok {
					calls[tu.ID] = true
				}
			}
		case types.SystemMessage:
			for _, c := range v.Content {
				if r, ok := c.(types.ToolResultContent); ok && !calls[r.ToolCallID] {
					return fmt.Errorf("%w: %s", errSplitToolCall, r.ToolCallID)
				}
			}
		case types.UserMessage:
			for _, c := range v.Content {
				if r, ok := c.(types.ToolResultContent); ok && !calls[r.ToolCallID] {
					return fmt.Errorf("%w: %s", errSplitToolCall, r.ToolCallID)
				}
			}
		}
	}
	return nil
}

// persistBranch writes every node created for branch to the Store.
func (a *Agent) persistBranch(ctx context.Context, tr *tree.Tree, branch types.BranchID) {
	if a.cfg.Store == nil {
		return
	}
	tip, err := tr.Tip(branch)
	if err != nil {
		return
	}
	path, err := tr.Path(tip.ID)
	if err != nil {
		return
	}
	for i := 1; i < len(path); i++ {
		children, err := tr.Children(path[i-1])
		if err != nil {
			return
		}
		for _, n := range children {
			if n.ID == path[i] && n.BranchID == branch {
				a.persistNode(ctx, n)
			}
		}
	}
}

// compactMessages runs a message-level compactor and moves the run onto a
// new branch holding its output. It reports false when the compactor failed
// or left the history unchanged.
func (a *Agent) compactMessages(ctx context.Context, log *slog.Logger, compactor types.Compactor, provider types.Provider, llmMessages []types.Message, tr *tree.Tree) (types.BranchID, bool) {
	compacted, err := compactor.Compact(ctx, llmMessages, provider)
	if err != nil {
		log.Warn("compaction failed, continuing with full history", "error", err)
		return "", false
	}
	if !historyChanged(llmMessages, compacted) {
		return "", false
	}

	newBranch, err := a.persistCompacted(ctx, tr, compacted)
	if err != nil {
		log.Warn("failed to persist compacted branch", "error", err)
		return "", false
	}
	a.persistBranch(ctx, tr, newBranch)
	log.Debug("compacted to new branch", "branch", newBranch)
	return newBranch, true
}

// historyChanged reports whether a compactor changed the history. Strategies
// that shorten it change its length; strategies that rewrite messages in
// place, such as clearing tool results, keep the length.
func historyChanged(before, after []types.Message) bool {
	if len(after) != len(before) {
		return len(after) < len(before)
	}
	return !reflect.DeepEqual(before, after)
}

// pressureTokenizer counts at least the input size the provider last
// reported, so tree.Compact sees the same pressure that triggered it.
type pressureTokenizer struct {
	base     types.Tokenizer
	observed int
}

func (p pressureTokenizer) CountTokens(ctx context.Context, messages []types.Message) (int, error) {
	n, err := p.base.CountTokens(ctx, messages)
	if err != nil {
		if p.observed > 0 {
			return p.observed, nil
		}
		return 0, err
	}
	return max(n, p.observed), nil
}

// meteredProvider makes model calls on the run's behalf outside a turn
// (summaries) and accounts for them like turns. With a budget, each call is
// reserved before it is sent and settled with its usage when its stream ends,
// so concurrent agents sharing the budget cannot overspend while a summary
// runs. Without one, it only records usage for the stream.
type meteredProvider struct {
	types.Provider
	agent  *Agent
	stream *EventStream
	step   string

	mu    sync.Mutex
	n     int
	calls []types.UsageDelta
	err   error
}

func (m *meteredProvider) ChatStream(ctx context.Context, messages []types.Message, tools []types.ToolDef) (<-chan types.Delta, error) {
	settle := func(usage *types.UsageDelta, _ bool) (*types.UsageDelta, error) { return usage, nil }
	if m.agent != nil && m.agent.cfg.Budget != nil {
		m.mu.Lock()
		m.n++
		step := fmt.Sprintf("%s-%d", m.step, m.n)
		m.mu.Unlock()
		reservation, pricing, err := m.agent.reserveProviderCall(ctx, m.stream, m.Provider, step)
		if err != nil {
			m.fail(err)
			return nil, err
		}
		settle = func(usage *types.UsageDelta, failed bool) (*types.UsageDelta, error) {
			_, settled, err := m.agent.settleProviderCall(reservation, pricing, m.Provider, usage, failed)
			return settled, err
		}
	}
	rx, err := m.Provider.ChatStream(ctx, messages, tools)
	if err != nil {
		m.finish(settle(nil, true))
		return nil, err
	}
	out := make(chan types.Delta)
	go func() {
		defer close(out)
		var (
			call   types.UsageDelta
			seen   bool
			failed bool
		)
		for d := range rx {
			switch v := d.(type) {
			case types.UsageDelta:
				call, seen = call.Merge(v), true
			case types.ErrorDelta:
				failed = true
			}
			select {
			case out <- d:
			case <-ctx.Done():
				// Keep draining so the provider can finish and close rx.
			}
		}
		var usage *types.UsageDelta
		if seen {
			call.Cumulative = false
			call.AccountingID = ""
			usage = &call
		}
		m.finish(settle(usage, failed))
	}()
	return out, nil
}

// finish records a settled call. A call that reported no tokens is not
// streamed; its settlement already charged the budget.
func (m *meteredProvider) finish(usage *types.UsageDelta, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if usage != nil && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) {
		m.calls = append(m.calls, *usage)
	}
	if err != nil && m.err == nil {
		m.err = err
	}
}

func (m *meteredProvider) fail(err error) {
	m.finish(nil, err)
}

// result returns the usage of each settled call and the first budget error.
func (m *meteredProvider) result() ([]types.UsageDelta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls), m.err
}
