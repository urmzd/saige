// Package thread is the memory of a conversation: an append-only journal of
// what was said in one thread, kept so that the next run in that thread can be
// shown it.
//
// Without it every tag starts a fresh conversation. An agent in a long Slack
// thread remembers nothing, and the only request prefix two runs share is the
// tool definitions and the system prompt, so a provider's prompt cache has
// almost nothing to reuse. A journal fixes both at once, and the two fixes are
// the same property seen from two sides: history that only ever appends is
// both what an agent should remember and what a prefix cache can read.
//
// # What a thread is
//
// A thread is identified by a Key: the tenant, the agent, and the surface the
// tag came from. See KeyOf for the exact rule. Two consequences are deliberate.
// Different agents tagged in one Slack thread keep separate journals, because
// each journal is a record of one agent's conversation and not a transcript of
// the room. And nothing is shared across tenants, because the tenant is part
// of the key and never a filter applied afterwards.
//
// # Append-only, and why that is not negotiable
//
// An entry is never edited and never reordered, and a sequence number is never
// reused. Two things depend on it.
//
// A durable run replays from the top on every attempt, so the history it reads
// has to be the same on attempt three as it was on attempt one. pkg/runtime
// pins "history up to sequence N" in the run's input when the tag is accepted
// and loads exactly that on every attempt. The pin only means something if the
// entries at or below N can never change.
//
// A provider's prompt cache reuses a request up to the first byte that differs
// from an earlier one. A journal that rewrote an old entry would move that
// byte to the middle of the conversation and make every later run pay full
// price for everything after it.
//
// # Idempotent appends
//
// Durable runs retry, so a run that appended its exchange and then crashed
// will append it again. Append therefore recognizes an entry it already holds,
// by (run id, role) or by surface message id, and returns the stored entry
// instead of writing a second one. An entry carrying neither identity is
// refused, because a retry of it could not be told from a new message.
//
// # The surface is the real record
//
// People post in a thread without tagging the agent, and edit or delete what
// they posted. Lister is the seam a connector implements to say what its
// surface holds, and Reconcile appends whatever the journal has not seen. It
// only appends: an edit or a delete on the surface does not rewrite the
// journal, for the two reasons above.
//
// # Boundary
//
// This package stores and orders messages. It does not know what a run is
// beyond an id, does not render anything for a model (pkg/turn/native does), and
// does not decide when history is pinned or appended (pkg/runtime does). It
// imports only the leaves that name its inputs.
package thread

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/event"
)

// Sentinel errors. Callers match with errors.Is.
var (
	// ErrInvalid reports a key or entry the journal cannot hold.
	ErrInvalid = errors.New("thread: invalid")
	// ErrGap reports a journal that does not hold every entry up to the
	// sequence it was asked for. Sequences are assigned without holes and
	// never removed, so a gap means the journal lost data, and a run pinned
	// to that history cannot be shown it.
	ErrGap = errors.New("thread: journal is missing entries")
)

// Role says who a message is from, in the model's terms.
type Role string

const (
	// RoleUser is anything said to the agent: the tagging message, and every
	// other message people posted in the thread.
	RoleUser Role = "user"
	// RoleAssistant is the agent's own final answer.
	RoleAssistant Role = "assistant"
)

// Valid reports whether r is a role the journal records.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAssistant }

// ParamThread is the one address parameter that is part of a thread's
// identity. Every other parameter is positioning that changes while the
// thread stays the same thread.
const ParamThread = "thread"

// oneShot prefixes the surface of a thread that belongs to a single tag. It
// cannot collide with a canonical address, which always contains "://".
const oneShot = "tag:"

// Key identifies one thread.
type Key struct {
	// Tenant is the authorization scope. It is required.
	Tenant string `json:"tenant"`
	// Agent is the agent whose conversation this is, normalized.
	Agent string `json:"agent"`
	// Surface is the canonical form of where the conversation happens. See
	// KeyOf.
	Surface string `json:"surface"`
}

// KeyOf returns the thread a run of agent answering e belongs to.
//
// The surface is e's Source, read as an address, reduced to what identifies
// a conversation:
//
//	connector://workspace/path[?thread=<value>]
//
// The connector, workspace and path are kept as they are. Of the parameters
// only "thread" is kept, because pkg/address defines parameters as positioning
// rather than identity and "thread" is the one exception a surface needs: two
// Slack threads in one channel share a path and differ only there. Everything
// else is dropped. A GitHub pull request address carries head_sha, which
// changes with every push, and keeping it would start a new conversation each
// time someone committed.
//
//	slack://T01/C02?thread=1699.001                  -> slack://T01/C02?thread=1699.001
//	github://acme/handbook/pull/7?head_sha=abc123     -> github://acme/handbook/pull/7
//	jira://acme/PROJ-5                                -> jira://acme/PROJ-5
//
// An event whose Source is not an address (a schedule's tick, a delegation)
// has no surface a second event could share, so it is its own one-shot
// thread, keyed by the event's id. A redelivery carries the same id and lands
// on the same thread.
func KeyOf(e event.Envelope, agent string) Key {
	k := Key{Tenant: e.Tenant, Agent: strings.ToLower(strings.TrimSpace(agent))}
	src, err := address.Parse(e.Source)
	if err != nil {
		k.Surface = oneShot + e.ID
		return k
	}
	k.Surface = Surface(src)
	return k
}

// Surface returns the canonical thread surface of an address: the address with
// every parameter but ParamThread removed. It returns "" for the zero address.
func Surface(a address.Address) string {
	canonical := address.Address{Connector: a.Connector, Workspace: a.Workspace, Path: a.Path}
	if v, ok := a.Param(ParamThread); ok && v != "" {
		canonical.Params = map[string]string{ParamThread: v}
	}
	return canonical.String()
}

// OneShot reports whether the thread belongs to a single tag with no surface,
// so that no later tag can ever continue it.
func (k Key) OneShot() bool { return strings.HasPrefix(k.Surface, oneShot) }

// Validate checks that the key names exactly one thread.
//
// An empty tenant is refused for the reason every store refuses it: the
// tenant comes from the caller's credential, so a missing one means the
// authorization step was skipped, and a shared bucket for unscoped callers is
// how one customer reads another's conversation.
func (k Key) Validate() error {
	switch {
	case k.Tenant == "":
		return fmt.Errorf("%w: no tenant; every thread is scoped to one", ErrInvalid)
	case k.Agent == "":
		return fmt.Errorf("%w: thread names no agent", ErrInvalid)
	case k.Surface == "" || k.Surface == oneShot:
		return fmt.Errorf("%w: thread names no surface", ErrInvalid)
	}
	return nil
}

// String renders the key for logs and errors.
func (k Key) String() string { return k.Tenant + "/" + k.Agent + "@" + k.Surface }

// Entry is one message in a thread's journal.
type Entry struct {
	// Seq orders entries within their thread, from 1, without holes. The
	// store assigns it; a value passed to Append is ignored.
	Seq uint64 `json:"seq"`
	// Role is who the message is from.
	Role Role `json:"role"`
	// Text is the message.
	Text string `json:"text"`
	// Actor is who said it: a person for a user message, the agent for its
	// own answer.
	Actor event.Actor `json:"actor,omitempty"`
	// RunID is the run that produced the entry, empty for a message that
	// reached the journal by reconciliation rather than through a run.
	RunID string `json:"run_id,omitempty"`
	// SurfaceID is the message's id on its surface (a Slack timestamp, a
	// GitHub comment id), empty when unknown.
	SurfaceID string `json:"surface_id,omitempty"`
	// At is when the message was said, or when it was appended if that is
	// all anyone knows. The store stamps a zero value with its own clock.
	At time.Time `json:"at"`
	// Attachments are the files that came with the message, as references,
	// in the order the surface listed them. The bytes stay in pkg/blob: a
	// journal entry is replayed into every later run of the thread, and a
	// reference is what can be replayed byte for byte. Nil for a message
	// with none, which is also what every entry written before this field
	// existed reads back as.
	Attachments []attachment.Attachment `json:"attachments,omitempty"`
}

// validate checks what Append requires of an entry.
func (e Entry) validate() error {
	switch {
	case !e.Role.Valid():
		return fmt.Errorf("%w: entry has role %q, want %q or %q", ErrInvalid, e.Role, RoleUser, RoleAssistant)
	case strings.TrimSpace(e.Text) == "" && len(e.Attachments) == 0:
		return fmt.Errorf("%w: entry has neither text nor attachments", ErrInvalid)
	case e.RunID == "" && e.SurfaceID == "":
		return fmt.Errorf("%w: entry has neither a run id nor a surface message id, so a retried append could not be recognized", ErrInvalid)
	}
	if err := attachment.ValidateAll(e.Attachments); err != nil {
		return fmt.Errorf("%w: entry attachments: %w", ErrInvalid, err)
	}
	return nil
}

// clone returns e with its own copy of the attachment list, so that nothing
// a caller holds can reach back into a stored entry. An empty list becomes
// nil: "no attachments" has one representation, whichever store answered.
func (e Entry) clone() Entry {
	if len(e.Attachments) == 0 {
		e.Attachments = nil
		return e
	}
	e.Attachments = append([]attachment.Attachment(nil), e.Attachments...)
	return e
}

// Store is a journal of threads.
//
// Memory and Postgres implement it and are held to it by one test suite.
type Store interface {
	// Append adds entries to the end of a thread, in the order given, and
	// returns one stored entry per input, in the same order.
	//
	// It is atomic and contiguous: either every new entry is written or none
	// is, and the new entries of one call take consecutive sequence numbers,
	// so two appenders to one thread can never interleave or lose an entry.
	//
	// It is idempotent. An input is already held when the thread has an entry
	// with the same non-empty RunID and the same Role, or with the same
	// non-empty SurfaceID. Such an input writes nothing, and the entry
	// returned for it is the one already stored, untouched. This holds within
	// one call as well as across calls.
	Append(ctx context.Context, key Key, entries ...Entry) ([]Entry, error)

	// Head returns the sequence of a thread's newest entry, or zero for a
	// thread with none. It is what a run pins.
	Head(ctx context.Context, key Key) (uint64, error)

	// History returns the entries with Seq <= upTo, oldest first. Because
	// the journal is append-only, the answer for a given upTo is the same
	// forever. Asking for more than the thread holds is ErrGap.
	History(ctx context.Context, key Key, upTo uint64) ([]Entry, error)
}

// Option configures a store.
type Option func(*settings)

type settings struct {
	clock func() time.Time
}

func newSettings(opts []Option) settings {
	s := settings{clock: time.Now}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// WithClock replaces the clock used to stamp entries appended with a zero At.
func WithClock(now func() time.Time) Option {
	return func(s *settings) {
		if now != nil {
			s.clock = now
		}
	}
}

// prepare validates an append and stamps the entries it will consider. Both
// backends run it before touching storage, so they refuse the same inputs.
// Times are truncated to the microsecond, which is what Postgres keeps: the
// entry returned by Append is then the entry every later read returns.
func (s settings) prepare(ctx context.Context, key Key, entries []Entry) ([]Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("%w (entry %d of %d for %s)", err, i+1, len(entries), key)
		}
		if e.At.IsZero() {
			e.At = s.clock()
		}
		e.At = e.At.UTC().Truncate(time.Microsecond)
		e.Seq = 0
		out[i] = e.clone()
	}
	return out, nil
}

// check validates a read.
func check(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return key.Validate()
}
