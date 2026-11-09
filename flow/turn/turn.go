// Package turn is the seam between a run and whatever executes one turn of
// it. A Runtime takes a pinned definition and the event that triggered the
// turn, and streams back catalog event bodies.
//
// The native Runtime (pkg/turn/native) wraps saige. Any other implementation
// is welcome: the run's durability stays with durable and its isolation with
// sandbox (pkg/turn/sandbox wraps any Runtime). This package imports
// neither saige nor anything that does.
//
// What a turn may read beyond its arguments travels on its context: the stop
// and compact signals (StopSignal, CompactSignal), the host's holds
// (HoldsFrom) and the actor the turn acts for (event.ActorFrom).
package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
)

// ErrInvalid reports a pinned definition or input a Runtime cannot run.
// Callers match with errors.Is.
var ErrInvalid = errors.New("turn: invalid")

// Runtime runs one turn.
//
// emit is called on the caller's goroutine or the Runtime's own, in order,
// and never concurrently. An error from it aborts the turn. Every body
// emitted is a registered catalog type, and Result.Text is the concatenation
// of the run.delta.text.v1 bodies emitted.
//
// A turn asked to stop (StopSignal) returns Result.Stopped and no error. A
// failed turn returns an error and what it had produced so far.
type Runtime interface {
	Turn(ctx context.Context, def Pinned, in Input, emit func(context.Context, event.Body) error) (Result, error)
}

// Func adapts a function to Runtime.
type Func func(ctx context.Context, def Pinned, in Input, emit func(context.Context, event.Body) error) (Result, error)

// Turn calls f.
func (f Func) Turn(ctx context.Context, def Pinned, in Input, emit func(context.Context, event.Body) error) (Result, error) {
	return f(ctx, def, in, emit)
}

// Pinned is the exact definition a turn runs: a name, a version, and the
// digest of its content at that version. A Runtime never resolves a newer
// one: a durable run replays from the top, and a definition that moved
// between attempts would diverge from its own ledger.
type Pinned struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Digest is the content hash of Spec at Version, or empty where the
	// source of definitions computes none.
	Digest string `json:"digest,omitempty"`
	// Spec is the definition's content.
	Spec Spec `json:"spec"`
}

// Validate checks the fields every Runtime needs.
func (p Pinned) Validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("%w: the pinned definition has no name", ErrInvalid)
	case p.Version == "":
		return fmt.Errorf("%w: definition %q is not pinned to a version", ErrInvalid, p.Name)
	case p.Spec.Name != p.Name:
		return fmt.Errorf("%w: definition %q carries the spec of %q", ErrInvalid, p.Name, p.Spec.Name)
	}
	return nil
}

// Spec is what an agent is: a model behind a provider, a system prompt, the
// tools it may call, the sources it may retrieve from, and what it may do
// beyond talking. It is immutable: editing an agent makes a new version.
type Spec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
	// Effort is the reasoning effort the primary model is asked for, such
	// as "low" or "high". Empty is the model's default. A host sets it on
	// its copy of a pinned spec for one turn, as it may set Provider and
	// Model; it is never applied to a fallback, which may not take it.
	Effort       string   `json:"effort,omitempty"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	Sources      []Source `json:"sources,omitempty"`
	Access       Policy   `json:"access,omitempty"`
	// Output, when set, constrains the agent's final answer to a JSON
	// schema: the answer is a JSON object that matches Output.Schema. Nil
	// means free text. An agent with tools is sent the schema only once its
	// last reply calls none and is not already such an object; that reply
	// was streamed too, so the answer's text is then the draft followed by
	// the JSON.
	Output *Output `json:"output,omitempty"`
	// Fallbacks are the models tried, in order, when a call to the primary
	// fails with an error another model could fix.
	Fallbacks []ModelRef `json:"fallbacks,omitempty"`
	// FallbackOn is when a call moves on to the next fallback. Empty means
	// any error another model could fix.
	FallbackOn []string `json:"fallback_on,omitempty"`
	// Approvals names the tools whose calls wait for a person.
	Approvals []string `json:"approvals,omitempty"`
	// Approvers, SelfApproval and Answerers say who may answer the questions
	// the agent asks.
	Approvers    []string `json:"approvers,omitempty"`
	SelfApproval bool     `json:"self_approval,omitempty"`
	Answerers    []string `json:"answerers,omitempty"`
	// TaggedOnly runs the agent only when a message names it.
	TaggedOnly bool `json:"tagged_only,omitempty"`
}

// Output is the shape an agent's final answer takes.
type Output struct {
	// Schema is the JSON Schema the answer is constrained to. Its top-level
	// type is "object". Empty or null constrains nothing.
	Schema json.RawMessage `json:"schema"`
}

// ModelRef names one model behind one provider.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Source is one retrieval corpus the agent may ground answers in.
type Source struct {
	Name    string            `json:"name"`
	URI     string            `json:"uri"`
	Options map[string]string `json:"options,omitempty"`
}

// Policy is what a definition may do beyond talking, as two closed
// allowlists: everything not listed is denied. A zero Policy grants nothing.
type Policy struct {
	// Spawn names the agents it may delegate to.
	Spawn []string `json:"spawn,omitempty"`
	// WorkspaceAreas are the shared-workspace key prefixes it may use.
	WorkspaceAreas []string `json:"workspace_areas,omitempty"`
}

// Input is one turn's input. It must be the same on every attempt: nothing
// derived from the moment of execution belongs here.
type Budget struct {
	CostMicros int64 `json:"cost_micros"`
	Tokens     int64 `json:"tokens"`
	Requests   int64 `json:"requests"`
}

type Input struct {
	Budget *Budget `json:"budget,omitempty"`
	// Trigger is the event the turn answers. The prompt is its catalog
	// rendering, and so is the retrieval query.
	Trigger event.Envelope `json:"trigger"`
	// History is the conversation so far, oldest first, replayed verbatim
	// ahead of the request.
	History []Message `json:"history,omitempty"`
	// Instructions is a trusted host-generated return contract. It
	// supplements the pinned system prompt; callers never pass user text.
	Instructions string `json:"instructions,omitempty"`
	// Tenant is the authorization scope, set by the host from the principal.
	Tenant string `json:"tenant,omitempty"`
	// Accounting is the prefix the turn's model calls are keyed under.
	Accounting string `json:"accounting,omitempty"`
	// Park lets the turn end at a question for a person (Result.Parked).
	Park bool `json:"park,omitempty"`
	// QuestionTTL is how long a question waits before its default applies.
	QuestionTTL time.Duration `json:"question_ttl,omitempty"`
	// Resume continues a turn that parked, with Resume.Answers filled in.
	Resume *Parked `json:"resume,omitempty"`
	// Compaction, when set, is this turn's history budget in place of the
	// runtime's own: a host's per-run choice, such as a run's
	// compaction_tokens. It is resolved before the turn, the same on every
	// attempt. A runtime that cannot compact ignores it.
	Compaction *Compaction `json:"compaction,omitempty"`
}

// Compaction is the budget that decides when a turn's history is summarized.
// Its fields mean what native.Compaction's do.
type Compaction struct {
	// MaxInputTokens is the input size, in tokens, above which the history
	// is compacted. Zero or less compacts only when asked, if OnRequest.
	MaxInputTokens int `json:"max_input_tokens,omitempty"`
	// KeepTurns is how many recent tool-calling turns survive verbatim.
	KeepTurns int `json:"keep_turns,omitempty"`
	// OnRequest lets a host ask the running turn to compact.
	OnRequest bool `json:"on_request,omitempty"`
}

// Role says who a history message is from.
type Role string

const (
	// RoleUser is a message said to the agent.
	RoleUser Role = "user"
	// RoleAssistant is an answer the agent gave.
	RoleAssistant Role = "assistant"
)

// Message is one earlier message of the conversation a turn continues. It
// has nowhere to put a timestamp or a run id, so none can reach the model and
// differ between two runs.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
	// Attachments are the files a user message arrived with.
	Attachments []attachment.Attachment `json:"attachments,omitempty"`
}

// Stop asks a turn to wrap up before it has finished.
type Stop struct {
	// Reason is why, in the words of whoever asked.
	Reason string `json:"reason,omitempty"`
	// By is who asked. It is never sent to the model.
	By string `json:"by,omitempty"`
}

// CompactRequest asks a turn to compact its history before its next model
// call.
type CompactRequest struct {
	// By is who asked. It is never sent to the model.
	By string `json:"by,omitempty"`
}

// Parked is where a turn that ended at a question stopped. Question and
// Prompt are for the host. State is the Runtime's own record of where it
// stopped, opaque to everyone else. The host fills Answers and passes the
// whole value back as Input.Resume.
type Parked struct {
	Question event.Question `json:"question"`
	// Prompt is the request message the parked turn sent.
	Prompt string `json:"prompt"`
	// Answers are the answers the turn resumes with, by question id.
	Answers map[string]event.Answer `json:"answers,omitempty"`
	// State is the Runtime's continuation.
	State json.RawMessage `json:"state,omitempty"`
}

// Result is what a turn produced.
type Result struct {
	// Text is the final answer: the concatenation of the run.delta.text.v1
	// bodies emitted.
	Text string `json:"text"`
	// Prompt is the request message exactly as the model was sent it. A host
	// that keeps history records it as what the user said.
	Prompt string `json:"prompt,omitempty"`
	// Citations counts the retrieved passages the turn was grounded in.
	Citations int `json:"citations,omitempty"`
	// Usage is what the turn's own model calls spent.
	Usage event.Usage `json:"usage,omitzero"`
	// Compactions lists each time the turn's history was summarized.
	Compactions []event.Compaction `json:"compactions,omitempty"`
	// Stopped is set when the turn ended because it was asked to stop.
	Stopped *Stop `json:"stopped,omitempty"`
	// Parked is set when the turn ended at a question for a person.
	Parked *Parked `json:"parked,omitempty"`
}

// Hold is a host's request that a run wait for a person before it does
// anything more: a run at a spending limit, say. A turn that may park ends at
// its next batch of tool calls with a question of kind event.QuestionHold,
// and runs nothing in that batch until the question is answered. An approval
// continues the turn; a denial, or nobody answering in time, stops the run.
type Hold struct {
	// ID is the host's id for the hold, unique within the run. A hold whose
	// question was answered is not asked again.
	ID string `json:"id"`
	// Topic is the host's name for what the run is held on.
	Topic string `json:"topic,omitempty"`
	// Prompt is the question, for a person.
	Prompt string `json:"prompt"`
	// Approvers, when set, are the only people who may answer, named as a
	// spec's Approvers are. Empty leaves who may answer to the host.
	Approvers []string `json:"approvers,omitempty"`
	// Data is the host's own detail, carried on the question unread.
	Data json.RawMessage `json:"data,omitempty"`
	// ExpiresIn is how long the question waits before it is denied. Zero
	// keeps the turn's question lifetime.
	ExpiresIn time.Duration `json:"expires_in,omitempty"`
}

// Holds reports the holds a run has that no answer has settled yet, oldest
// first. It is called from inside the turn, between model calls, so it must
// be cheap and safe for concurrent use.
type Holds func(ctx context.Context) []Hold

type stopKey struct{}

type compactKey struct{}

type holdsKey struct{}

// WithHolds returns a context on which a turn reads its run's holds through
// fn.
func WithHolds(ctx context.Context, fn Holds) context.Context {
	return context.WithValue(ctx, holdsKey{}, fn)
}

// HoldsFrom returns how a turn under ctx reads its run's holds, or nil when
// nobody can hold it.
func HoldsFrom(ctx context.Context) Holds {
	fn, _ := ctx.Value(holdsKey{}).(Holds)
	return fn
}

// WithStopSignal returns a context on which a turn is asked to stop through
// ch. The first value received stops the turn.
func WithStopSignal(ctx context.Context, ch <-chan Stop) context.Context {
	return context.WithValue(ctx, stopKey{}, ch)
}

// StopSignal returns the channel a turn under ctx is asked to stop on, or
// nil when it cannot be.
func StopSignal(ctx context.Context) <-chan Stop {
	ch, _ := ctx.Value(stopKey{}).(<-chan Stop)
	return ch
}

// WithCompactSignal returns a context on which a turn is asked to compact its
// history through ch.
func WithCompactSignal(ctx context.Context, ch <-chan CompactRequest) context.Context {
	return context.WithValue(ctx, compactKey{}, ch)
}

// CompactSignal returns the channel a turn under ctx is asked to compact on,
// or nil when nobody can ask.
func CompactSignal(ctx context.Context) <-chan CompactRequest {
	ch, _ := ctx.Value(compactKey{}).(<-chan CompactRequest)
	return ch
}
