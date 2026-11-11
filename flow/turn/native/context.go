package native

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	saige "github.com/urmzd/saige/agent"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// This file is the context policy of a turn: how the request is laid out so a
// provider's prompt cache can reuse it, and when the history is compacted.
// The mechanisms are saige's (its compaction strategies, its Anthropic cache
// breakpoints, its usage deltas). What lives here is the choice of which ones
// a flow turn uses and the reasons, because a wrong choice is invisible:
// the run still answers, it just pays full price for every token on every
// call.

// DefaultKeepTurns is how many of the most recent tool-calling turns a
// compaction leaves verbatim when Compaction.KeepTurns is unset. It matches
// saige's default.
const DefaultKeepTurns = saigetypes.DefaultKeepTurns

// Prompt cache lifetimes accepted by WithPromptCache.
const (
	// PromptCacheOff sends no cache breakpoints.
	PromptCacheOff = ""
	// PromptCacheShort keeps a cached prefix for five minutes, refreshed on
	// every read. It is the default: the calls of one tool loop are seconds
	// apart, so a short lifetime is enough to carry a run.
	PromptCacheShort = "5m"
	// PromptCacheLong keeps it for an hour, at a higher write price. It pays
	// off for an agent tagged a few times an hour with a large system prompt.
	PromptCacheLong = "1h"
)

// Compaction is the budget that decides when a long turn's history is
// summarized.
//
// The zero value disables compaction, which is the default: history only ever
// appends, and a turn that outgrows the model's context window fails. That
// default is deliberate. A compaction costs a model call and rewrites the
// request prefix, so it should happen because a deployment chose a budget,
// not because a library guessed one for a model it has never seen.
type Compaction struct {
	// MaxInputTokens is the input size, in tokens, above which the history
	// is compacted before the next model call. Zero or less disables
	// compaction.
	//
	// Size is the larger of the provider's reported input for the previous
	// call and a local estimate of the current history. The budget is a
	// trigger, not a hard cap: see WithCompaction for when a request may
	// exceed it.
	MaxInputTokens int
	// KeepTurns is how many of the most recent tool-calling turns survive
	// verbatim (default DefaultKeepTurns). It is also the minimum distance
	// between two compactions, so a larger value compacts less often and
	// keeps the cache warm for longer between rewrites.
	KeepTurns int
	// OnRequest lets a host ask a running turn to compact (Turn.Compact),
	// whatever its size. It is independent of MaxInputTokens: a deployment
	// may compact only when asked, only past a budget, or both. Without it
	// a request is ignored, and so is the rest of this paragraph. With it,
	// a turn whose request is rejected by the provider as too long is also
	// compacted and retried instead of failing, as it is under a budget.
	OnRequest bool
}

// enabled reports whether a turn is configured to compact at all.
func (c Compaction) enabled() bool { return c.MaxInputTokens > 0 || c.OnRequest }

// requestOnlyBudget is the budget a turn that compacts only on request runs
// under: large enough that no history reaches it, so the budget never fires
// and a request (which reports a size past it) is the only trigger.
const requestOnlyBudget = math.MaxInt32

// budget is the input size saige is told to compact above.
func (c Compaction) budget() int {
	if c.MaxInputTokens > 0 {
		return c.MaxInputTokens
	}
	return requestOnlyBudget
}

func (c Compaction) keepTurns() int {
	if c.KeepTurns <= 0 {
		return DefaultKeepTurns
	}
	return c.KeepTurns
}

// config is the saige compaction config for c, or nil when compaction is off.
//
// The strategy is saige's "summary": keep the system prompt and the original
// request, replace everything older than the last KeepTurns turns with one
// summary, keep those turns verbatim. Two properties make it the right one
// here. It keeps the original request, which carries the retrieved passages
// the turn's citations were published from, so the model never loses the
// material it was grounded in. And its cut is at a turn boundary counted from
// the end, so where it cuts depends on the shape of the history and nothing
// else.
//
// A turn that replays a thread (Turn.History) changes what "the original
// request" is. The message the strategy always keeps is the first one after
// the system prompt, which is then the oldest message of the thread; the
// turn's own request is the newest turn and survives only as one of the last
// KeepTurns. A turn that both replays history and runs more than KeepTurns
// tool calls past its budget can therefore have its request, retrieved
// passages included, folded into the summary.
//
// Threshold is left at zero so the strategy's own message-count rule never
// fires: the token budget is the only trigger.
func (c Compaction) config() *saigetypes.CompactConfig {
	if !c.enabled() {
		return nil
	}
	return &saigetypes.CompactConfig{
		Strategy:       saigetypes.CompactSummary,
		MaxInputTokens: c.budget(),
		KeepTurns:      c.keepTurns(),
	}
}

// WithCompaction sets the budget above which a turn's history is compacted.
// Without it a turn never compacts.
//
// # What a compaction does to the request
//
// Between compactions every model call sends the previous call's request plus
// the messages added since, byte for byte, so a prefix cache reads all of it.
// A compaction is the one exception. It replaces the turns older than the last
// KeepTurns with a single summary and leaves the system prompt, the tool
// definitions, and the original request untouched, so that much of the prefix
// survives even the rewrite. The summary is then part of the history like any
// other message and stays fixed until the next compaction.
//
// # Compactions are rare by construction
//
// After a compaction, another one is refused until KeepTurns further turns
// have been appended. Without that rule a turn whose last KeepTurns turns
// alone exceed the budget (a few very large tool results will do it) would be
// compacted before every single call, each time paying for a summary and
// missing the cache, which is the exact failure a budget exists to prevent.
// The cost of the rule is that such a turn sends requests over the budget
// until the distance is covered. If one of them is over the model's real
// context window the provider rejects it, and that rejection forces a
// compaction regardless of the rule. So does a host's request (Turn.Compact):
// a person asked for that compaction.
//
// # Across the runs of a thread
//
// History replayed from earlier runs (Turn.History) counts toward the budget
// like any other message. A compaction is not carried from one run to the
// next: each run is handed the thread verbatim, so once a thread is over the
// budget every run in it compacts at its first model call, pays for a summary,
// and shares only the tools, the system prompt, and the thread's oldest
// message with the run before. Set the budget with that in mind.
//
// # Determinism
//
// Whether to compact is a function of the turn's own history: the messages
// and the input size the provider reported for the previous call. Where to cut
// is a function of the messages alone. No clock, counter, or random value is
// consulted, so two executions that see the same history compact at the same
// call and at the same boundary. The summary text is model output and is not
// reproducible; Result.Compactions records that one was written.
//
// # The offline provider
//
// The summary is written by the turn's own provider. Under ProviderOffline
// that means a compaction consumes the next entry of the script as its
// summary. A script written for a turn that compacts has to include it.
func WithCompaction(c Compaction) Option {
	return func(cfg *config) { cfg.compaction = c }
}

// WithPromptCache sets how long the built-in Anthropic provider asks the API
// to keep a cached request prefix: PromptCacheShort (the default),
// PromptCacheLong, or PromptCacheOff. Any other value fails New.
//
// With caching on, three breakpoints are placed on every request: after the
// tool definitions, after the system prompt, and on the newest message. The
// first two cover what is identical for every run of a revision, so a second
// tag to the same agent reads its instructions from the cache. The third
// covers the conversation, so each call of a tool loop reads everything the
// previous call sent. A model that does not support cache breakpoints is sent
// none.
//
// It has no effect on other providers. Ollama has no prompt cache to address,
// and a provider registered with WithProvider owns its own configuration.
func WithPromptCache(ttl string) Option {
	return func(cfg *config) { cfg.promptCache = ttl }
}

func validPromptCache(ttl string) error {
	switch ttl {
	case PromptCacheOff, PromptCacheShort, PromptCacheLong:
		return nil
	}
	return fmt.Errorf("%w: prompt cache lifetime %q, want %q, %q, or %q to turn it off",
		ErrInvalid, ttl, PromptCacheShort, PromptCacheLong, PromptCacheOff)
}

// messagesPerTurn is how many messages one tool-calling turn adds to the
// history: the assistant message that makes the calls and the message that
// carries their results.
const messagesPerTurn = 2

// compactionGuard enforces the minimum distance between two compactions of
// one turn (see WithCompaction). It is per turn, and saige calls compaction
// hooks from the agent loop one at a time, so it needs no lock.
type compactionGuard struct {
	// minGrowth is how many messages must be appended after a compaction
	// before the next one.
	minGrowth int
	// compacted is true once any compaction has happened.
	compacted bool
	// settled is false between a compaction and the first measurement of
	// the history it left behind.
	settled bool
	// base is the size of the history right after the last compaction.
	base int
}

func newCompactionGuard(keepTurns int) *compactionGuard {
	return &compactionGuard{minGrowth: keepTurns * messagesPerTurn}
}

// before skips a compaction that comes too soon after the last one. A forced
// compaction is never skipped: the provider has already refused the request as
// too long, and skipping would fail the turn to protect a cache.
func (g *compactionGuard) before(_ context.Context, ev *saige.CompactionEvent) error {
	if ev.Forced || !g.compacted {
		return nil
	}
	if !g.settled {
		// The first look at the history after a compaction. saige comes
		// back at once to see whether the result fits the budget; what it
		// measures here is the baseline every later look is compared to.
		g.base, g.settled = ev.Messages, true
		ev.Skip = true
		return nil
	}
	if ev.Messages-g.base < g.minGrowth {
		ev.Skip = true
	}
	return nil
}

func (g *compactionGuard) after(_ context.Context, ev *saige.CompactionEvent) error {
	if ev.Compacted {
		g.compacted, g.settled = true, false
	}
	return nil
}

// CompactRequest asks a running turn to compact its history before its next
// model call. See Turn.Compact.
type CompactRequest = turn.CompactRequest

// TriggerRequested is event.Compaction.Trigger for a compaction a host
// asked for.
const TriggerRequested = string(saigetypes.CompactionTriggerRequested)

// compactRequests carries a host's requests to compact from the goroutine
// that receives them to the agent loop, and tells the compactions that
// answered them apart from the ones the budget made.
//
// saige has no call to compact a running turn. What it has is a budget it
// measures the history against before every model call, with a tokenizer
// the caller may supply. So a request is a pending flag the tokenizer reads:
// while one is pending it reports the history as larger than the budget, the
// loop compacts, and the BeforeCompaction hook takes the request. The
// compaction is the one saige would make under the budget, with the same
// strategy, cut and anti-thrash rule, only earlier. The count reported on
// the record is measured after the request was taken, so it is the real one.
//
// Requests that arrive while one is pending fold into it: two presses of a
// button before the next model call are one compaction.
//
// A request is received on the watcher goroutine and recorded a moment
// later, so a request that has arrived may not be pending yet. CountTokens
// catches up with the watcher before it reads the flag (see catchUp), so
// every request received before the loop measures is one it sees.
type compactRequests struct {
	base   saigetypes.Tokenizer
	budget int

	// sync is received by the watcher goroutine between requests, and gone
	// is closed when it stops watching. Both are nil until watch is called.
	sync chan struct{}
	gone <-chan struct{}

	mu      sync.Mutex
	pending bool
	by      string
	// attempts holds, oldest first, one entry per compaction saige has
	// started whose record the turn has not read yet: whether it answered
	// a request, and who asked. A compaction that changes nothing is
	// removed when it ends, since it sends no record.
	attempts []compactAttempt
}

type compactAttempt struct {
	requested bool
	by        string
}

func newCompactRequests(base saigetypes.Tokenizer, budget int) *compactRequests {
	return &compactRequests{base: base, budget: budget, sync: make(chan struct{})}
}

// watch names the channel closed when the watcher goroutine stops receiving
// requests. It is called before the agent loop starts.
func (q *compactRequests) watch(gone <-chan struct{}) { q.gone = gone }

// catchUp waits until the watcher goroutine has recorded every request it has
// received. The watcher receives from sync only between requests, so once
// the send completes, a request it took before is pending. A watcher that has
// stopped has nothing left to record.
func (q *compactRequests) catchUp(ctx context.Context) {
	select {
	case q.sync <- struct{}{}:
	case <-q.gone:
	case <-ctx.Done():
	}
}

// request records a host's request.
func (q *compactRequests) request(r CompactRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.pending {
		q.pending, q.by = true, strings.TrimSpace(r.By)
	}
}

// CountTokens implements saigetypes.Tokenizer.
func (q *compactRequests) CountTokens(ctx context.Context, messages []saigetypes.Message) (int, error) {
	n, err := q.base.CountTokens(ctx, messages)
	q.catchUp(ctx)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending {
		return max(n, q.budget+1), nil
	}
	return n, err
}

// started takes the pending request, if any, for a compaction saige is
// about to make, and reports whether there was one.
func (q *compactRequests) started() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	a := compactAttempt{requested: q.pending, by: q.by}
	q.pending, q.by = false, ""
	q.attempts = append(q.attempts, a)
	return a.requested
}

// ended forgets the last compaction started when it changed nothing.
func (q *compactRequests) ended(compacted bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !compacted && len(q.attempts) > 0 {
		q.attempts = q.attempts[:len(q.attempts)-1]
	}
}

// record attributes a compaction record to the attempt that made it: the
// oldest one not read yet, since compactions run one at a time and each
// sends its record before the next starts.
func (q *compactRequests) record(c event.Compaction) event.Compaction {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.attempts) == 0 {
		return c
	}
	a := q.attempts[0]
	q.attempts = q.attempts[1:]
	if a.requested {
		if c.Trigger == string(saigetypes.CompactionTriggerInputPressure) {
			c.Trigger = TriggerRequested
		}
		c.By = a.by
	}
	return c
}

// compactionHooks are the turn's compaction hooks: the anti-thrash guard
// when there is a budget, and the request bookkeeping when a host may ask.
// A request is never skipped by the guard; the host asked for this one.
func compactionHooks(guard *compactionGuard, reqs *compactRequests) saige.Hooks {
	return saige.Hooks{
		Name: "fiber-compaction",
		BeforeCompaction: func(ctx context.Context, ev *saige.CompactionEvent) error {
			requested := false
			if reqs != nil {
				requested = reqs.peek()
			}
			if !requested && guard != nil {
				if err := guard.before(ctx, ev); err != nil || ev.Skip {
					return err
				}
			}
			if reqs != nil {
				reqs.started()
			}
			return nil
		},
		AfterCompaction: func(ctx context.Context, ev *saige.CompactionEvent) error {
			if reqs != nil {
				reqs.ended(ev.Compacted)
			}
			if guard != nil {
				return guard.after(ctx, ev)
			}
			return nil
		},
	}
}

// peek reports whether a request is pending, without taking it.
func (q *compactRequests) peek() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

// compactionRecord reduces saige's compaction record to the facts a caller
// can act on. The node ids it drops name messages in a conversation tree that
// does not outlive the turn.
func compactionRecord(d saigetypes.CompactionDelta) event.Compaction {
	return event.Compaction{
		Trigger:      string(d.Record.Trigger),
		Strategy:     d.Record.Strategy,
		TokensBefore: d.Record.TokensBefore,
		TokensAfter:  d.Record.TokensAfter,
		Summarized:   len(d.Record.Summarized),
		Kept:         len(d.Record.Kept) + len(d.Record.Selected),
	}
}
