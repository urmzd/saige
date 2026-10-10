package types

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/selector/rank"
)

// CompactEntry is one message of a history under compaction, with where it
// came from, so a compaction can report what it kept, selected and
// summarized.
type CompactEntry struct {
	Message Message
	// Index is the position of the message in the history the compaction
	// started from. It is -1 for a message a strategy wrote, such as a
	// summary.
	Index int
	// Selected is true for an older message kept because relevance
	// selection chose it.
	Selected bool
	// Cleared is true when tool results in the message were replaced by
	// stubs.
	Cleared bool
}

// CompactRequest is the input to a CompactionStrategy.
type CompactRequest struct {
	Entries []CompactEntry
	// Query is the text relevance selection ranks older messages against:
	// the latest user turn, or the original task.
	Query string
	// Target is the input size, in tokens, a Chain stops at. Zero applies
	// every step.
	Target int
	// Force compacts even when a strategy's own message-count trigger has
	// not fired: the input is over its limit, or compaction was requested.
	Force bool
	// Tokenizer measures the history. Nil uses EstimatingTokenizer.
	Tokenizer Tokenizer
	// Provider writes summaries. The agent passes one that charges every
	// call to the run's budget.
	Provider Provider
}

// CompactResult is what a CompactionStrategy produced. Summarized and
// Dropped hold Index values of the original history.
type CompactResult struct {
	Entries    []CompactEntry
	Summarized []int
	Dropped    []int
	// Steps names the strategies that changed the history, in order.
	Steps []string
}

// Changed reports whether the strategy changed the history.
func (r CompactResult) Changed() bool { return len(r.Steps) > 0 }

// CompactionStrategy compacts a history and reports what it did with each
// message. Every strategy keeps the system prompt first and never separates
// a tool result from the tool call it answers. A strategy that has nothing
// to do returns the entries unchanged with no Steps.
type CompactionStrategy interface {
	Name() string
	CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error)
}

// DefaultKeepTurns is the number of recent turns kept when KeepTurns is not
// set.
const DefaultKeepTurns = 4

// DefaultSelectK is the number of older turns relevance selection keeps when
// SelectK is not set.
const DefaultSelectK = 3

// CompactionSummaryPrefix starts every summary a strategy writes. The
// summary is a system message, so providers read it as context rather than
// as something the user or the model said.
const CompactionSummaryPrefix = "Summary of the earlier conversation, written when it was compacted:\n\n"

// IsCompactionSummary reports whether m is a summary a strategy wrote.
func IsCompactionSummary(m Message) bool {
	sm, ok := m.(SystemMessage)
	if !ok || len(sm.Content) != 1 {
		return false
	}
	tc, ok := sm.Content[0].(TextContent)
	return ok && strings.HasPrefix(tc.Text, CompactionSummaryPrefix)
}

// NewCompactEntries numbers messages as the history a compaction starts from.
func NewCompactEntries(messages []Message) []CompactEntry {
	out := make([]CompactEntry, len(messages))
	for i, m := range messages {
		out[i] = CompactEntry{Message: m, Index: i}
	}
	return out
}

// EntryMessages returns the messages of entries.
func EntryMessages(entries []CompactEntry) []Message {
	out := make([]Message, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out
}

// AsStrategy returns c as a CompactionStrategy. A Compactor that is not one
// is adapted: its output is matched against its input to tell kept messages
// from summarized or dropped ones.
func AsStrategy(c Compactor) CompactionStrategy {
	switch v := c.(type) {
	case nil:
		return nil
	case CompactionStrategy:
		return v
	case *SlidingWindowCompactor:
		return compactorStrategy{name: string(CompactSlidingWindow), c: v}
	case *SummarizeCompactor:
		return compactorStrategy{name: string(CompactSummarize), c: v}
	case NoopCompactor:
		return compactorStrategy{name: string(CompactNone), c: v}
	default:
		return compactorStrategy{name: fmt.Sprintf("%T", c), c: c}
	}
}

// compactorStrategy adapts a message-level Compactor.
type compactorStrategy struct {
	name string
	c    Compactor
}

func (s compactorStrategy) Name() string { return s.name }

func (s compactorStrategy) CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error) {
	in := EntryMessages(req.Entries)
	out, err := s.c.Compact(ctx, in, req.Provider)
	if err != nil {
		return CompactResult{Entries: req.Entries}, err
	}
	if len(out) == len(in) && reflect.DeepEqual(in, out) {
		return CompactResult{Entries: req.Entries}, nil
	}
	// Match output messages to input messages in order. An output message
	// with no match was written by the compactor.
	res := CompactResult{Steps: []string{s.name}}
	used := make([]bool, len(in))
	wrote := false
	next := 0
	for _, m := range out {
		match := -1
		for i := next; i < len(in); i++ {
			if reflect.DeepEqual(in[i], m) {
				match = i
				break
			}
		}
		if match < 0 {
			res.Entries = append(res.Entries, CompactEntry{Message: m, Index: -1})
			wrote = true
			continue
		}
		used[match] = true
		next = match + 1
		res.Entries = append(res.Entries, req.Entries[match])
	}
	for i, ok := range used {
		if ok || req.Entries[i].Index < 0 {
			continue
		}
		if wrote {
			res.Summarized = append(res.Summarized, req.Entries[i].Index)
		} else {
			res.Dropped = append(res.Dropped, req.Entries[i].Index)
		}
	}
	return res, nil
}

// ── Turn structure ───────────────────────────────────────────────────

// layout splits a history into its head (the system prompt and the
// original task), summaries earlier compactions wrote, and turns. A turn is
// an assistant message with the tool results that answer it, together with
// the user or system messages directly before it. Tool results always stay
// in the turn of the call they answer.
type layout struct {
	head      []int
	summaries []int
	turns     [][]int
}

func layoutOf(entries []CompactEntry) layout {
	var l layout
	i := 0
	if i < len(entries) {
		if _, ok := entries[i].Message.(SystemMessage); ok && !IsCompactionSummary(entries[i].Message) {
			l.head = append(l.head, i)
			i++
		}
	}
	if i < len(entries) {
		if um, ok := entries[i].Message.(UserMessage); ok && !hasToolResult(um) {
			l.head = append(l.head, i)
			i++
		}
	}
	var cur []int
	hasAssistant := false
	for ; i < len(entries); i++ {
		m := entries[i].Message
		if IsCompactionSummary(m) {
			l.summaries = append(l.summaries, i)
			continue
		}
		switch {
		case hasToolResult(m):
			// Results belong to the call before them.
		case isAssistant(m):
			if hasAssistant {
				l.turns = append(l.turns, cur)
				cur = nil
			}
			hasAssistant = true
		default:
			if hasAssistant {
				l.turns = append(l.turns, cur)
				cur, hasAssistant = nil, false
			}
		}
		cur = append(cur, i)
	}
	if len(cur) > 0 {
		l.turns = append(l.turns, cur)
	}
	return l
}

func isAssistant(m Message) bool {
	_, ok := m.(AssistantMessage)
	return ok
}

// units splits the positions of older turns into the smallest spans that
// keep tool calls with their results: a message that carries no tool result
// starts a span, and tool results join the span before them.
func units(entries []CompactEntry, turns [][]int) [][]int {
	var out [][]int
	for _, t := range turns {
		for _, p := range t {
			if len(out) == 0 || !hasToolResult(entries[p].Message) {
				out = append(out, []int{p})
				continue
			}
			out[len(out)-1] = append(out[len(out)-1], p)
		}
	}
	return out
}

func flatten(spans [][]int) []int {
	var out []int
	for _, s := range spans {
		out = append(out, s...)
	}
	return out
}

func pick(entries []CompactEntry, positions []int) []CompactEntry {
	out := make([]CompactEntry, 0, len(positions))
	for _, p := range positions {
		out = append(out, entries[p])
	}
	return out
}

func indices(entries []CompactEntry, positions []int) []int {
	var out []int
	for _, p := range positions {
		if entries[p].Index >= 0 {
			out = append(out, entries[p].Index)
		}
	}
	return out
}

func keepTurns(n int) int {
	if n <= 0 {
		return DefaultKeepTurns
	}
	return n
}

// ── KeepRecent ───────────────────────────────────────────────────────

// KeepRecent keeps the system prompt, the original task, earlier summaries
// and the last Turns turns, and drops the rest. It makes no model call.
type KeepRecent struct {
	Turns int // recent turns kept (default 4)
}

// NewKeepRecent keeps the last n turns (default 4).
func NewKeepRecent(n int) *KeepRecent { return &KeepRecent{Turns: n} }

func (k *KeepRecent) Name() string { return string(CompactKeepRecent) }

func (k *KeepRecent) CompactEntries(_ context.Context, req CompactRequest) (CompactResult, error) {
	l := layoutOf(req.Entries)
	n := keepTurns(k.Turns)
	if len(l.turns) <= n {
		return CompactResult{Entries: req.Entries}, nil
	}
	old, recent := l.turns[:len(l.turns)-n], l.turns[len(l.turns)-n:]
	kept := slices.Concat(l.head, l.summaries, flatten(recent))
	return CompactResult{
		Entries: pick(req.Entries, kept),
		Dropped: indices(req.Entries, flatten(old)),
		Steps:   []string{k.Name()},
	}, nil
}

func (k *KeepRecent) Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error) {
	return compactMessages(ctx, k, messages, provider)
}

// ── Summary ──────────────────────────────────────────────────────────

// Summary keeps the system prompt, the original task and the last KeepTurns
// turns, and replaces everything between them, earlier summaries included,
// with one summary written by the request's provider. The summary is a
// system message.
type Summary struct {
	KeepTurns int // recent turns kept (default 4)
	// Threshold is the message count, not counting summaries, above which
	// the strategy runs on its own. Zero runs it only when forced.
	Threshold int
}

func (s *Summary) Name() string { return string(CompactSummary) }

func (s *Summary) CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error) {
	if !triggered(req, s.Threshold) {
		return CompactResult{Entries: req.Entries}, nil
	}
	l := layoutOf(req.Entries)
	n := keepTurns(s.KeepTurns)
	if len(l.turns) <= n {
		return CompactResult{Entries: req.Entries}, nil
	}
	old, recent := l.turns[:len(l.turns)-n], l.turns[len(l.turns)-n:]
	return summarizeSpan(ctx, req, s.Name(), l, nil, slices.Concat(l.summaries, flatten(old)), flatten(recent))
}

func (s *Summary) Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error) {
	return compactMessages(ctx, s, messages, provider)
}

// triggered reports whether a summarizing strategy runs: when forced, or
// when the history, not counting summaries, has more than threshold messages.
func triggered(req CompactRequest, threshold int) bool {
	if req.Force {
		return true
	}
	if threshold <= 0 {
		return false
	}
	n := 0
	for _, e := range req.Entries {
		if !IsCompactionSummary(e.Message) {
			n++
		}
	}
	return n > threshold
}

// summarizeSpan writes head, a summary of span, selected and recent, in that
// order. Selected positions are kept verbatim and marked.
func summarizeSpan(ctx context.Context, req CompactRequest, name string, l layout, selected, span, recent []int) (CompactResult, error) {
	unchanged := CompactResult{Entries: req.Entries}
	var summaryEntry []CompactEntry
	if len(span) > 0 {
		// One summary replaces the span, so a span of one message that is
		// not already a summary cannot shrink the history.
		if len(span) == 1 && !IsCompactionSummary(req.Entries[span[0]].Message) {
			return unchanged, nil
		}
		if req.Provider == nil {
			return unchanged, fmt.Errorf("%s: no provider to write the summary", name)
		}
		text, err := Summarize(ctx, req.Provider, EntryMessages(pick(req.Entries, span)))
		if err != nil {
			return unchanged, fmt.Errorf("%s: %w", name, err)
		}
		summaryEntry = []CompactEntry{{Message: NewSystemMessage(CompactionSummaryPrefix + text), Index: -1}}
	}
	sel := pick(req.Entries, selected)
	for i := range sel {
		sel[i].Selected = true
	}
	return CompactResult{
		Entries:    slices.Concat(pick(req.Entries, l.head), summaryEntry, sel, pick(req.Entries, recent)),
		Summarized: indices(req.Entries, span),
		Steps:      []string{name},
	}, nil
}

// SummaryInstruction is the system prompt of a summary call.
const SummaryInstruction = "Summarize the following conversation concisely, preserving key facts, names, numbers and decisions."

// Summarize asks provider to summarize msgs. It fails on a stream error or
// an empty summary, so a failed call never replaces real history.
func Summarize(ctx context.Context, provider Provider, msgs []Message) (string, error) {
	rx, err := provider.ChatStream(ctx, []Message{
		NewSystemMessage(SummaryInstruction),
		NewUserMessage(MessagesToText(msgs)),
	}, nil)
	if err != nil {
		return "", fmt.Errorf("summarization: %w", err)
	}
	var sb strings.Builder
	var streamErr error
	for d := range rx {
		switch v := d.(type) {
		case TextContentDelta:
			sb.WriteString(v.Content)
		case ErrorDelta:
			streamErr = v.Error
		}
	}
	if streamErr != nil {
		return "", fmt.Errorf("summarization: %w", streamErr)
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return "", errors.New("summarization produced an empty summary")
	}
	return text, nil
}

// ── RelevantPlusSummary ──────────────────────────────────────────────

// RelevantPlusSummary keeps the system prompt, the original task and the
// last KeepTurns turns. From the older turns it keeps, verbatim, the K spans
// most relevant to the request's Query by BM25, each a message with the tool
// results that answer it. It summarizes the rest.
type RelevantPlusSummary struct {
	KeepTurns int // recent turns kept (default 4)
	K         int // older spans selected (default 3)
	Threshold int // as Summary.Threshold
}

func (r *RelevantPlusSummary) Name() string { return string(CompactRelevantPlusSummary) }

func (r *RelevantPlusSummary) CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error) {
	if !triggered(req, r.Threshold) {
		return CompactResult{Entries: req.Entries}, nil
	}
	l := layoutOf(req.Entries)
	n := keepTurns(r.KeepTurns)
	if len(l.turns) <= n {
		return CompactResult{Entries: req.Entries}, nil
	}
	old, recent := l.turns[:len(l.turns)-n], l.turns[len(l.turns)-n:]
	spans := units(req.Entries, old)
	k := r.K
	if k <= 0 {
		k = DefaultSelectK
	}
	chosen := map[int]bool{}
	if req.Query != "" {
		ids := make([]int, len(spans))
		for i := range spans {
			ids[i] = i
		}
		bm := rank.NewBM25(func(i int) string { return MessagesToText(EntryMessages(pick(req.Entries, spans[i]))) })
		hits, err := bm.Select(ctx, req.Query, ids, k)
		if err != nil && ctx.Err() != nil {
			return CompactResult{Entries: req.Entries}, err
		}
		for _, h := range hits {
			chosen[h] = true
		}
	}
	var selected []int
	rest := slices.Clone(l.summaries)
	for i, s := range spans {
		if chosen[i] {
			selected = append(selected, s...)
		} else {
			rest = append(rest, s...)
		}
	}
	slices.Sort(rest)
	if len(rest) == 0 {
		return CompactResult{Entries: req.Entries}, nil
	}
	return summarizeSpan(ctx, req, r.Name(), l, selected, rest, flatten(recent))
}

func (r *RelevantPlusSummary) Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error) {
	return compactMessages(ctx, r, messages, provider)
}

// ── Chain ────────────────────────────────────────────────────────────

// Chain applies its strategies in order. With a Target it stops as soon as
// the history fits, and forces each later step, since the history is
// still over the target. Without one it applies every step.
type Chain []CompactionStrategy

func (c Chain) Name() string {
	names := make([]string, len(c))
	for i, s := range c {
		names[i] = s.Name()
	}
	return string(CompactChain) + "(" + strings.Join(names, ",") + ")"
}

func (c Chain) CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error) {
	tok := req.Tokenizer
	if tok == nil {
		tok = EstimatingTokenizer{}
	}
	res := CompactResult{Entries: req.Entries}
	for _, s := range c {
		step := req
		step.Entries = res.Entries
		if req.Target > 0 {
			n, err := tok.CountTokens(ctx, EntryMessages(res.Entries))
			if err != nil {
				return res, err
			}
			if n <= req.Target {
				break
			}
			step.Force = true
		}
		out, err := s.CompactEntries(ctx, step)
		if err != nil {
			// A failed step leaves the history as the previous steps left
			// it; the next step may still make it fit.
			if ctx.Err() != nil {
				return res, err
			}
			continue
		}
		if !out.Changed() {
			continue
		}
		res.Entries = out.Entries
		res.Summarized = append(res.Summarized, out.Summarized...)
		res.Dropped = append(res.Dropped, out.Dropped...)
		res.Steps = append(res.Steps, out.Steps...)
	}
	return res, nil
}

func (c Chain) Compact(ctx context.Context, messages []Message, provider Provider) ([]Message, error) {
	return compactMessages(ctx, c, messages, provider)
}

// ── ClearToolResults as a strategy ───────────────────────────────────

func (c *ClearToolResultsCompactor) Name() string { return string(CompactClearToolResults) }

func (c *ClearToolResultsCompactor) CompactEntries(ctx context.Context, req CompactRequest) (CompactResult, error) {
	in := EntryMessages(req.Entries)
	out, err := c.Compact(ctx, in, req.Provider)
	if err != nil {
		return CompactResult{Entries: req.Entries}, err
	}
	res := CompactResult{Entries: slices.Clone(req.Entries)}
	for i := range out {
		if !reflect.DeepEqual(in[i], out[i]) {
			res.Entries[i].Message = out[i]
			res.Entries[i].Cleared = true
		}
	}
	if !reflect.DeepEqual(in, out) {
		res.Steps = []string{c.Name()}
	}
	return res, nil
}

// compactMessages runs a strategy as a message-level Compactor, triggered
// by its own rules.
func compactMessages(ctx context.Context, s CompactionStrategy, messages []Message, provider Provider) ([]Message, error) {
	res, err := s.CompactEntries(ctx, CompactRequest{Entries: NewCompactEntries(messages), Provider: provider})
	if err != nil || !res.Changed() {
		return messages, err
	}
	return EntryMessages(res.Entries), nil
}

// ToolPairingError reports the first tool result in msgs that does not
// answer a tool call made earlier in msgs.
func ToolPairingError(msgs []Message) error {
	calls := map[string]bool{}
	for _, m := range msgs {
		if am, ok := m.(AssistantMessage); ok {
			for _, c := range am.Content {
				if tu, ok := c.(ToolUseContent); ok {
					calls[tu.ID] = true
				}
			}
			continue
		}
		for _, r := range toolResults(m) {
			if !calls[r.ToolCallID] {
				return fmt.Errorf("%w: %s", ErrSplitToolCall, r.ToolCallID)
			}
		}
	}
	return nil
}

// ErrSplitToolCall reports a history in which a tool result no longer
// follows the call that produced it.
var ErrSplitToolCall = errors.New("compaction separated a tool result from its tool call")
