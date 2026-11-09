package event

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Run and control types. Each was an envelope.Kind before the union; the old
// kind is noted on each body.
const (
	TypeAccepted    = "run.accepted.v1"
	TypeStarted     = "run.started.v1"
	TypeParked      = "run.parked.v1"
	TypeResumed     = "run.resumed.v1"
	TypeCompleted   = "run.completed.v1"
	TypeFailed      = "run.failed.v1"
	TypeStopped     = "run.stopped.v1"
	TypeChildParked = "run.child.parked.v1"

	TypeText       = "run.delta.text.v1"
	TypeThinking   = "run.delta.thinking.v1"
	TypeToolCall   = "run.delta.tool.call.v1"
	TypeToolDone   = "run.delta.tool.done.v1"
	TypeCitation   = "run.delta.citation.v1"
	TypeToolMarker = "run.delta.tool.marker.v1"

	TypeAction = "run.action.taken.v1"

	TypeUsageCall  = "run.usage.v1"
	TypeUsageChild = "run.usage.child.v1"
	TypeCompacted  = "run.context.compacted.v1"

	TypeStop    = "control.stop.v1"
	TypeCompact = "control.compact.v1"
)

// Tool call phases reported in ToolCall.Phase.
const (
	// PhaseRequested is the model generating a tool call.
	PhaseRequested = "requested"
	// PhaseExecuting is the harness having begun executing it.
	PhaseExecuting = "executing"
)

// MaxToolArguments bounds the arguments a tool event carries, in bytes of
// compact JSON. Arguments over it are left out whole, never cut.
const MaxToolArguments = 8 << 10

// Question kinds: what a run is waiting on a person for.
const (
	QuestionApproval   = "approval"
	QuestionChoice     = "choice"
	QuestionReply      = "reply"
	QuestionDelegation = "delegation"
	// QuestionHold is a question the host put to a person about a run, not
	// one its agent asked: whether the run may go on (see
	// runtime.Runtime.Hold). It is answered approve, and the run continues,
	// or deny, and the run is stopped.
	QuestionHold = "hold"
)

// Decisions on an approval.
const (
	DecisionApprove = "approve"
	DecisionDeny    = "deny"
)

// Accepted is the body of run.accepted.v1 (was lifecycle.accepted).
type Accepted struct {
	Attempt int `json:"attempt,omitempty"`
}

// Started is the body of run.started.v1 (was lifecycle.started).
type Started struct {
	Attempt int `json:"attempt,omitempty"`
}

// Parked is the body of run.parked.v1 (was lifecycle.parked).
type Parked struct {
	Attempt int `json:"attempt,omitempty"`
	// Reason explains the park, in the words of whatever the run waits for.
	Reason string `json:"reason,omitempty"`
	// Question is what the run waits on a person for, when it parked on one.
	Question *Question `json:"question,omitempty"`
}

// Resumed is the body of run.resumed.v1 (was lifecycle.resumed).
type Resumed struct {
	Attempt int `json:"attempt,omitempty"`
	// Answer is the answer the run resumed with, when it parked on a question.
	Answer *Answer `json:"answer,omitempty"`
}

// Completed is the body of run.completed.v1 (was lifecycle.completed).
type Completed struct {
	Attempt int `json:"attempt,omitempty"`
	// Text is the whole final answer.
	Text string `json:"text,omitempty"`
	// Usage is what the run spent, everything it delegated included.
	Usage *RunUsage `json:"usage,omitempty"`
}

// Failed is the body of run.failed.v1 (was lifecycle.failed). Terminal
// reports that the run will not be retried; the ledger stays the authority.
type Failed struct {
	Attempt  int    `json:"attempt,omitempty"`
	Error    string `json:"error,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
}

// Stopped is the body of run.stopped.v1 (was lifecycle.stopped): a run asked
// to stop that wrapped its turn up.
type Stopped struct {
	Attempt int `json:"attempt,omitempty"`
	// Reason is why, in the words of whoever asked.
	Reason string `json:"reason,omitempty"`
	// By is who asked. Never shown to a model.
	By string `json:"by,omitempty"`
	// Text is what the agent had answered when it was stopped.
	Text  string    `json:"text,omitempty"`
	Usage *RunUsage `json:"usage,omitempty"`
}

// ChildParked is the body of run.child.parked.v1 (was lifecycle.child.parked):
// a child run this run waits on is parked on a question for a person.
type ChildParked struct {
	RunID    string   `json:"run_id"`
	Agent    string   `json:"agent,omitempty"`
	Question Question `json:"question"`
}

// Text is the body of run.delta.text.v1 (was delta.text): one fragment of the
// answer, in order.
type Text struct {
	Text string `json:"text"`
	// Parent is the path of tool call ids, from the run's own agent down,
	// of the nested agent that said this: an agent the run's agent runs as
	// a tool. Empty is the run's own agent. It is set on every delta body.
	Parent []string `json:"parent,omitempty"`
}

// Thinking is the body of run.delta.thinking.v1 (was delta.thinking).
type Thinking struct {
	Text   string   `json:"text"`
	Parent []string `json:"parent,omitempty"`
}

// ToolCall is the body of run.delta.tool.call.v1 (was delta.tool.call).
type ToolCall struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Phase          string          `json:"phase"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	ArgumentsBytes int             `json:"arguments_bytes,omitempty"`
	Parent         []string        `json:"parent,omitempty"`
}

// ToolDone is the body of run.delta.tool.done.v1 (was delta.tool.done).
type ToolDone struct {
	ID             string          `json:"id"`
	Name           string          `json:"name,omitempty"`
	Result         string          `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	ArgumentsBytes int             `json:"arguments_bytes,omitempty"`
	Parent         []string        `json:"parent,omitempty"`
}

// Provenance locates the source a citation came from. It mirrors saige's
// rag/types.Provenance.
type Provenance struct {
	DocumentUUID   string `json:"document_uuid,omitempty"`
	DocumentTitle  string `json:"document_title,omitempty"`
	SourceURI      string `json:"source_uri,omitempty"`
	SectionUUID    string `json:"section_uuid,omitempty"`
	SectionHeading string `json:"section_heading,omitempty"`
	SectionIndex   int    `json:"section_index,omitempty"`
}

// Citation is the body of run.delta.citation.v1 (was delta.citation).
type Citation struct {
	// Label is the marker the answer cites the source with, such as "[1]".
	Label string `json:"label"`
	// Text is the cited text: the retrieved passage, or the quote a tool
	// attributed to the source.
	Text   string     `json:"text"`
	Source Provenance `json:"source"`
	// Index is the passage's position in the retrieved context, or, for a
	// tool's citation, the source's number less one.
	Index int `json:"index"`
	// ToolCallID is the call whose result the citation came from, when a
	// tool's result rather than the model cited it.
	ToolCallID string   `json:"tool_call_id,omitempty"`
	Parent     []string `json:"parent,omitempty"`
}

// ToolMarker is the body of run.delta.tool.marker.v1: a tool call that
// waits for a decision before it runs, such as an approval or an answer to
// the model's question. The call's arguments are on its run.delta.tool.call.v1
// events, not here.
type ToolMarker struct {
	// ID is the call's id within the agent that made it; with Parent it
	// names the same call its ToolCall and ToolDone do.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Markers say why the call waits, in the tool's or the model's words.
	Markers []Marker `json:"markers,omitempty"`
	// Interrupt is the decision the call waits on, when one was posted.
	Interrupt *MarkerInterrupt `json:"interrupt,omitempty"`
	Parent    []string         `json:"parent,omitempty"`
}

// Marker is one reason a tool call waits.
type Marker struct {
	// Kind is the marker's kind, such as "human_approval" or "clarification".
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

// MarkerInterrupt is the pending decision a ToolMarker waits on.
type MarkerInterrupt struct {
	// ID names the decision: a reply can answer it by this id.
	ID string `json:"id"`
	// Kind is what the decision is: "approval", "clarification", "budget"
	// or "input".
	Kind      string    `json:"kind,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Action is the body of run.action.taken.v1 (was action.taken). Target and
// Address are address URIs.
type Action struct {
	Name    string          `json:"name"`
	Target  string          `json:"target,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Address string          `json:"address,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// Question is what a parked run waits on a person for.
type Question struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Agent        string        `json:"agent,omitempty"`
	Prompt       string        `json:"prompt"`
	Options      []string      `json:"options,omitempty"`
	Tool         *QuestionTool `json:"tool,omitempty"`
	AskedOf      string        `json:"asked_of,omitempty"`
	Approvers    []string      `json:"approvers,omitempty"`
	SelfApproval bool          `json:"self_approval,omitempty"`
	ExpiresAt    time.Time     `json:"expires_at,omitzero"`
	Default      *Answer       `json:"default,omitempty"`
	At           time.Time     `json:"at,omitzero"`
	ChildRunID   string        `json:"child_run_id,omitempty"`
	// Hold is set on a question of kind QuestionHold: which of the host's
	// holds it asks about.
	Hold *Hold `json:"hold,omitempty"`
}

// Hold is the host's hold a QuestionHold question asks about.
type Hold struct {
	// ID is the host's id for the hold, unique within the run.
	ID string `json:"id"`
	// Topic is the host's name for what the run is held on ("budget", say).
	Topic string `json:"topic,omitempty"`
	// Data is the host's own detail about the hold, opaque to flow.
	Data json.RawMessage `json:"data,omitempty"`
}

// QuestionTool is the tool call an approval asks about.
type QuestionTool struct {
	Name          string          `json:"name"`
	CallID        string          `json:"call_id"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Schema        json.RawMessage `json:"schema,omitempty"`
	ActsForPerson bool            `json:"acts_for_person,omitempty"`
}

// Answer is a person's answer to a Question, or the question's default.
type Answer struct {
	QuestionID string          `json:"question_id"`
	Decision   string          `json:"decision,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Choice     string          `json:"choice,omitempty"`
	Text       string          `json:"text,omitempty"`
	Note       string          `json:"note,omitempty"`
	By         string          `json:"by,omitempty"`
	At         time.Time       `json:"at,omitzero"`
	Expired    bool            `json:"expired,omitempty"`
	RunID      string          `json:"run_id,omitempty"`
}

// StopRequest is the body of control.stop.v1 (was control.stop).
type StopRequest struct {
	RunID  string    `json:"run_id"`
	Reason string    `json:"reason,omitempty"`
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at,omitzero"`
}

// CompactRequest is the body of control.compact.v1 (was control.compact).
type CompactRequest struct {
	RunID string    `json:"run_id"`
	By    string    `json:"by,omitempty"`
	At    time.Time `json:"at"`
}

// Cost is an amount of money in integer micro-units of Currency. One US
// dollar is Micros 1_000_000 with Currency "USD".
type Cost struct {
	Micros   int64  `json:"micros"`
	Currency string `json:"currency"`
	AsOf     string `json:"as_of,omitempty"`
}

// Usage is what some model calls cost, summed.
type Usage struct {
	ModelCalls         int    `json:"model_calls,omitempty"`
	Requests           int    `json:"requests,omitempty"`
	PromptTokens       int    `json:"prompt_tokens,omitempty"`
	CachedPromptTokens int    `json:"cached_prompt_tokens,omitempty"`
	CacheWriteTokens   int    `json:"cache_write_tokens,omitempty"`
	CompletionTokens   int    `json:"completion_tokens,omitempty"`
	Cost               *Cost  `json:"cost,omitempty"`
	OtherCosts         []Cost `json:"other_costs,omitempty"`
	UnpricedCalls      int    `json:"unpriced_calls,omitempty"`
}

// IsZero reports whether nothing was counted.
func (u Usage) IsZero() bool {
	return u.ModelCalls == 0 && u.Requests == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 &&
		u.CachedPromptTokens == 0 && u.CacheWriteTokens == 0 && u.Cost == nil && len(u.OtherCosts) == 0 &&
		u.UnpricedCalls == 0
}

// ContextUsage is how full a run's context was at its latest model call.
type ContextUsage struct {
	Model  string `json:"model,omitempty"`
	Window int    `json:"window,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
}

// RunUsage is what a run spent, split by who spent it.
type RunUsage struct {
	Own      Usage         `json:"own"`
	Children Usage         `json:"children,omitzero"`
	Total    Usage         `json:"total"`
	Context  *ContextUsage `json:"context,omitempty"`
}

// UsageCall is the body of run.usage.v1 (was usage.call): one model call.
// Readers that sum them key them by AccountingID.
type UsageCall struct {
	AccountingID       string `json:"accounting_id"`
	SaigeAccountingID  string `json:"saige_accounting_id,omitempty"`
	Agent              string `json:"agent,omitempty"`
	Depth              int    `json:"depth,omitempty"`
	Model              string `json:"model,omitempty"`
	Requests           int    `json:"requests,omitempty"`
	PromptTokens       int    `json:"prompt_tokens,omitempty"`
	CachedPromptTokens int    `json:"cached_prompt_tokens,omitempty"`
	CacheWriteTokens   int    `json:"cache_write_tokens,omitempty"`
	CompletionTokens   int    `json:"completion_tokens,omitempty"`
	ResponseCacheHit   bool   `json:"response_cache_hit,omitempty"`
	Cost               *Cost  `json:"cost,omitempty"`
	ContextWindow      int    `json:"context_window,omitempty"`
}

// UsageChild is the body of run.usage.child.v1 (was usage.child): a child
// run's whole usage, once it has ended.
type UsageChild struct {
	AccountingID string `json:"accounting_id"`
	RunID        string `json:"run_id"`
	Agent        string `json:"agent,omitempty"`
	Depth        int    `json:"depth,omitempty"`
	Usage        Usage  `json:"usage"`
}

// Compaction is the body of run.context.compacted.v1 (was context.compacted).
type Compaction struct {
	Trigger      string `json:"trigger,omitempty"`
	By           string `json:"by,omitempty"`
	Strategy     string `json:"strategy,omitempty"`
	TokensBefore int    `json:"tokens_before,omitempty"`
	TokensAfter  int    `json:"tokens_after,omitempty"`
	Summarized   int    `json:"summarized,omitempty"`
	Kept         int    `json:"kept,omitempty"`
}

func (Accepted) Type() string       { return TypeAccepted }
func (Started) Type() string        { return TypeStarted }
func (Parked) Type() string         { return TypeParked }
func (Resumed) Type() string        { return TypeResumed }
func (Completed) Type() string      { return TypeCompleted }
func (Failed) Type() string         { return TypeFailed }
func (Stopped) Type() string        { return TypeStopped }
func (ChildParked) Type() string    { return TypeChildParked }
func (Text) Type() string           { return TypeText }
func (Thinking) Type() string       { return TypeThinking }
func (ToolCall) Type() string       { return TypeToolCall }
func (ToolDone) Type() string       { return TypeToolDone }
func (Citation) Type() string       { return TypeCitation }
func (ToolMarker) Type() string     { return TypeToolMarker }
func (Action) Type() string         { return TypeAction }
func (UsageCall) Type() string      { return TypeUsageCall }
func (UsageChild) Type() string     { return TypeUsageChild }
func (Compaction) Type() string     { return TypeCompacted }
func (StopRequest) Type() string    { return TypeStop }
func (CompactRequest) Type() string { return TypeCompact }

func (Accepted) isBody()       {}
func (Started) isBody()        {}
func (Parked) isBody()         {}
func (Resumed) isBody()        {}
func (Completed) isBody()      {}
func (Failed) isBody()         {}
func (Stopped) isBody()        {}
func (ChildParked) isBody()    {}
func (Text) isBody()           {}
func (Thinking) isBody()       {}
func (ToolCall) isBody()       {}
func (ToolDone) isBody()       {}
func (Citation) isBody()       {}
func (ToolMarker) isBody()     {}
func (Action) isBody()         {}
func (UsageCall) isBody()      {}
func (UsageChild) isBody()     {}
func (Compaction) isBody()     {}
func (StopRequest) isBody()    {}
func (CompactRequest) isBody() {}

// render returns a Render function for body type T: it checks the envelope
// carries a T and formats it with f.
func render[T Body](f func(Envelope, T) string) func(Envelope) (string, error) {
	return func(e Envelope) (string, error) {
		b, ok := e.Data.(T)
		if !ok {
			var want T
			return "", fmt.Errorf("event: render %s: data is %T, want %T", e.Type, e.Data, want)
		}
		return f(e, b), nil
	}
}

// line joins the non-empty parts with a space.
func line(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func renderQuestion(q *Question) string {
	if q == nil {
		return ""
	}
	s := "question: " + q.Prompt
	if len(q.Options) > 0 {
		s += " (options: " + strings.Join(q.Options, ", ") + ")"
	}
	return s
}

func renderAnswer(a *Answer) string {
	if a == nil {
		return ""
	}
	switch {
	case a.Expired:
		return "answer: nobody answered in time"
	case a.Decision != "":
		return line("answer:", a.Decision, a.Note)
	case a.Choice != "":
		return "answer: " + a.Choice
	}
	return line("answer:", a.Text, a.Note)
}

func renderMarker(b ToolMarker) string {
	what := "a decision"
	if b.Interrupt != nil && b.Interrupt.Kind != "" {
		what = b.Interrupt.Kind
	}
	s := line("tool", b.Name, "waits for", what)
	var why []string
	for _, m := range b.Markers {
		if m.Message != "" {
			why = append(why, m.Message)
		}
	}
	if len(why) > 0 {
		s += ": " + strings.Join(why, "; ")
	}
	return s
}

func renderTokens(prompt, completion int) string {
	return fmt.Sprintf("%d prompt tokens, %d completion tokens", prompt, completion)
}

func init() {
	for _, e := range []Entry{
		{Type: TypeAccepted, New: func() Body { return new(Accepted) },
			Render: render(func(e Envelope, _ Accepted) string { return line("run", e.Run, "accepted") })},
		{Type: TypeStarted, New: func() Body { return new(Started) },
			Render: render(func(e Envelope, b Started) string {
				return line("run", e.Run, fmt.Sprintf("started, attempt %d", b.Attempt))
			})},
		{Type: TypeParked, New: func() Body { return new(Parked) },
			Render: render(func(e Envelope, b Parked) string {
				return line("run", e.Run, "parked:", b.Reason, renderQuestion(b.Question))
			})},
		// An answer is human-authored free text.
		{Type: TypeResumed, Sensitive: true, New: func() Body { return new(Resumed) },
			Render: render(func(e Envelope, b Resumed) string {
				return line("run", e.Run, fmt.Sprintf("resumed, attempt %d", b.Attempt), renderAnswer(b.Answer))
			})},
		{Type: TypeCompleted, New: func() Body { return new(Completed) },
			Render: render(func(_ Envelope, b Completed) string { return b.Text })},
		{Type: TypeFailed, New: func() Body { return new(Failed) },
			Render: render(func(e Envelope, b Failed) string { return line("run", e.Run, "failed:", b.Error) })},
		// The reason is in the asker's words.
		{Type: TypeStopped, Sensitive: true, New: func() Body { return new(Stopped) },
			Render: render(func(e Envelope, b Stopped) string {
				return line("run", e.Run, "stopped:", b.Reason, b.Text)
			})},
		{Type: TypeChildParked, New: func() Body { return new(ChildParked) },
			Render: render(func(_ Envelope, b ChildParked) string {
				return line("child run", b.RunID, "parked:", renderQuestion(&b.Question))
			})},
		{Type: TypeText, New: func() Body { return new(Text) },
			Render: render(func(_ Envelope, b Text) string { return b.Text })},
		{Type: TypeThinking, New: func() Body { return new(Thinking) },
			Render: render(func(_ Envelope, b Thinking) string { return b.Text })},
		{Type: TypeToolCall, New: func() Body { return new(ToolCall) },
			Render: render(func(_ Envelope, b ToolCall) string {
				return line("tool call", b.Name, "("+b.Phase+")", string(b.Arguments))
			})},
		{Type: TypeToolDone, New: func() Body { return new(ToolDone) },
			Render: render(func(_ Envelope, b ToolDone) string {
				if b.Error != "" {
					return line("tool", b.Name, "failed:", b.Error)
				}
				return line("tool", b.Name, "returned:", b.Result)
			})},
		{Type: TypeCitation, New: func() Body { return new(Citation) },
			Render: render(func(_ Envelope, b Citation) string {
				src := b.Source.SourceURI
				if src == "" {
					src = line(b.Source.DocumentTitle, b.Source.SectionHeading)
				}
				return line(b.Label, b.Text, "("+src+")")
			})},
		{Type: TypeToolMarker, New: func() Body { return new(ToolMarker) },
			Render: render(func(_ Envelope, b ToolMarker) string { return renderMarker(b) })},
		{Type: TypeAction, New: func() Body { return new(Action) },
			Render: render(func(_ Envelope, b Action) string {
				if b.Error != "" {
					return line("action", b.Name, "failed:", b.Error)
				}
				return line("action", b.Name, "on", b.Target+":", b.Summary, b.Address)
			})},
		{Type: TypeUsageCall, OptIn: true, New: func() Body { return new(UsageCall) },
			Render: render(func(_ Envelope, b UsageCall) string {
				return line("model call", b.Model+":", renderTokens(b.PromptTokens, b.CompletionTokens))
			})},
		{Type: TypeUsageChild, OptIn: true, New: func() Body { return new(UsageChild) },
			Render: render(func(_ Envelope, b UsageChild) string {
				return line("child run", b.RunID, "used", renderTokens(b.Usage.PromptTokens, b.Usage.CompletionTokens))
			})},
		{Type: TypeCompacted, OptIn: true, New: func() Body { return new(Compaction) },
			Render: render(func(_ Envelope, b Compaction) string {
				return fmt.Sprintf("context compacted: %d messages summarized, %d kept", b.Summarized, b.Kept)
			})},
		// The reason is in the asker's words.
		{Type: TypeStop, Sensitive: true, New: func() Body { return new(StopRequest) },
			Render: render(func(_ Envelope, b StopRequest) string { return line("stop requested:", b.Reason) })},
		{Type: TypeCompact, New: func() Body { return new(CompactRequest) },
			Render: render(func(Envelope, CompactRequest) string { return "compaction requested" })},
	} {
		Register(e)
	}
}
