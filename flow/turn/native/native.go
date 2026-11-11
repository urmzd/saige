// Package native is the turn.Runtime that runs on saige (Runtime), and the
// Runner it is built on.
//
// It is the seam between two vocabularies. saige speaks providers, messages,
// tools, and a stream of typed deltas; flow speaks pinned definitions and
// catalog events. This package translates in one direction, a definition in
// and event bodies out, and knows nothing about how the definition was
// stored, which run it executes, or where its events end up.
//
// # What it produces
//
// A turn's output is a sequence of event bodies (pkg/event). They are not
// envelopes, because an envelope needs run identity (channel, sequence,
// tenant, pinned version) and this package cannot know any of it. The host
// stamps that on.
//
// # Citations
//
// A turn's citations come from two places. Its retrieved passages are
// published first (see Retriever and Citations), which makes them honest: they
// report the passages the turn was given, not the ones a model claims to have
// used. The sources a tool attributes its result to follow as the tools run,
// from saige's CitationDelta, at any depth. A tool's citation that repeats one
// already published is dropped (see Translator.Delta).
//
// # Context layout
//
// A provider's prompt cache reuses a request only up to the first byte that
// differs from an earlier one, so the request is laid out from what never
// changes to what always does:
//
//	tool definitions, sorted by name      identical for every run of a revision
//	system prompt, from the pinned spec   identical for every run of a revision
//	the thread's history, oldest first    identical for every run of a thread,
//	                                      and only ever appended to
//	the request: trigger metadata,        fixed for the whole turn
//	  retrieved passages, the text
//	the tool loop's history               only ever appended to
//
// Everything that varies from one run to the next (metadata, retrieval
// results) lives in the request message, never in the system prompt, so it
// cannot sit ahead of content that would otherwise be shared. Inside a turn
// each model call sends the previous call's request plus what was added since.
// The single exception is a compaction, which is off unless a budget is set
// or a host may ask for one (Compaction.OnRequest);
// see WithCompaction for what it rewrites and why it is rare, and
// WithPromptCache for where the cache breakpoints go.
//
// # History across runs
//
// A turn may carry the conversation so far in Turn.History. It is replayed
// into the request in the order given, one message per entry, after the
// system prompt and before the new request, and nothing is added to it: a
// Message is a role and a text, so there is no timestamp or run id that could
// reach the model and differ between two runs.
//
// A history message with attachments is rendered as its text followed by its
// files, resolved the way the request's files are. Resolution is a function of
// the references, the stored bytes and the pinned model, and one budget is
// spent across the history's files in order and then the request's, so a
// message renders the same bytes as history that it rendered as the request:
// the files of run k's request are spent from the same remaining budget in
// run k and in every later run that replays it. A long thread can therefore
// use up the per-run file budget, after which further files are notes; that
// is the price of a prefix that never changes.
//
// That gives a thread the same property a tool loop has. If the caller keeps
// history append-only and records each exchange as it happened (Result.Prompt
// is the request message exactly as it was sent, Result.Text is the answer),
// then the first request of run k is a byte prefix of the first request of
// run k+1, and a prefix cache reads the whole earlier conversation. The
// exception is the same one as inside a turn: a compaction rewrites the
// middle of the history and keeps the tools, the system prompt, and the
// oldest message.
//
// This package does not store history, and does not know what a thread is.
// With no History a turn starts a fresh conversation, and the prefix two runs
// of the same revision share is the tool definitions and the system prompt.
//
// # Stopping a turn
//
// A turn can be asked to stop while it runs (Turn.Stop). The model call in
// flight is cancelled, running tools are cancelled through their context, and
// the turn returns what it had answered so far with Result.Stopped set and no
// error: a stopped turn is neither a completed one nor a failed one, and a
// caller that retries failures must not retry it. This package does not decide
// what a stopped turn means for a run or a thread; pkg/runtime does.
//
// # Parking a turn on a person
//
// A turn that may park (Turn.Park) ends at a question for a person instead of
// waiting for one: an approval of a marked tool call, or the agent's
// ask_person tool. A batch of tool calls parks once, on every call in it that
// waits. Result.Parked holds the questions and everything a later
// turn (Turn.Resume) needs to continue it with the answer, sending exactly
// what an uninterrupted turn would have sent. A turn that may not park
// refuses approvals instead. See park.go.
//
// # Usage
//
// Every model call the turn makes is published as a run.usage.v1 body the
// moment saige reports it: the tokens in and out, the prompt cache's share,
// the model, the cost where saige's price catalog has a rate card for it, and
// the model's context window. Publishing per call rather than once at the end
// is what keeps the tokens of a turn that fails or is stopped on the record:
// they were spent either way. Result.Usage is the same calls summed. Nothing
// is priced from anything but saige's catalog; an unknown model reports its
// tokens and no cost.
//
// # Revisions are pinned, not resolved
//
// New takes a Revision, never an agent name. Nothing in this package can look
// up "the current definition of docs-bot", because a durable run replays from
// the top and a definition that moved between attempts would make the replay
// diverge from its own ledger. Pinning is pkg/runtime's job; refusing to
// un-pin is this package's.
package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	saige "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/convert"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"

	"github.com/urmzd/saige/flow/attachment"
	"github.com/urmzd/saige/flow/attachment/resolve"
)

// DefaultMaxIter bounds the agent loop's tool-calling iterations per turn. It
// matches saige's own default and is exposed so a deployment can lower it: an
// agent that loops is a cost incident, and the run's retry budget will not stop
// it because a looping turn never fails.
const DefaultMaxIter = 10

// Role says who a history message is from.
type Role = turn.Role

const (
	// RoleUser is a message said to the agent.
	RoleUser = turn.RoleUser
	// RoleAssistant is an answer the agent gave.
	RoleAssistant = turn.RoleAssistant
)

// Message is one earlier message of the conversation a turn continues: see
// turn.Message and "History across runs".
type Message = turn.Message

// DefaultStopReason is the reason a stop given none is reported with, on
// every path: saige's own default, so a turn that saige wraps up and a turn
// stopped before it began say the same thing.
const DefaultStopReason = saige.DefaultStopReason

// RequestText is the request message a turn of text and meta sends before any
// retrieved passages are added: the trigger metadata, in sorted key order,
// then the text. It is what a turn that was stopped before it began records as
// its request, and what a caller can record when it never learned the prompt.
func RequestText(text string, meta map[string]string) string {
	return event.RequestText(text, meta)
}

// Stop asks a turn to wrap up before it has finished. An empty Reason takes
// saige's default.
type Stop = turn.Stop

// Hold is a host's request that the run wait for a person: see
// "Holding a turn for the host" in park.go.
type Hold = turn.Hold

// Turn is one request to an agent.
type Turn struct {
	Budget *turn.Budget

	// Instructions is a trusted host-generated return contract. It supplements
	// the pinned system rules; callers must not supply arbitrary system text.
	Instructions string

	// Stop, when set, is how the turn is asked to wrap up early: the first
	// value received on it stops the turn (see "Stopping a turn" in the
	// package documentation). A nil channel is a turn that cannot be stopped,
	// only cancelled through its context. Nothing is read from it once the
	// turn has returned.
	Stop <-chan Stop
	// Compact carries a host's requests to compact the turn's history. Each
	// value asks for a compaction before the turn's next model call; values
	// that arrive before that call fold into one. A request after the last
	// model call changes nothing, and so does one when there is nothing old
	// enough to summarize (see Compaction.KeepTurns). It needs
	// Compaction.OnRequest; without it values are read and ignored. Nil is
	// a turn nobody can ask.
	Compact <-chan CompactRequest
	// Holds reads the host's holds on the run, when the host can hold it.
	// With Park, the turn ends at its next batch of tool calls on the oldest
	// hold its answers do not settle (see "Holding a turn for the host" in
	// park.go). Nil is a turn nobody holds.
	Holds turn.Holds
	// Attachments are the files that arrived with the request. They need
	// WithAttachments, and Tenant, which scopes every read of them.
	Attachments []attachment.Attachment
	// Tenant is the authorization scope the turn runs under. It is only read
	// to resolve attachments.
	Tenant string
	// History is the conversation so far, oldest first. It is replayed
	// verbatim ahead of the request. The caller owns its order and its
	// stability: see "History across runs" in the package documentation.
	History []Message
	// Text is the request, with the agent mention already stripped.
	Text string
	// Meta is trigger-specific context (a pull request number, an issue
	// title). It is rendered into the prompt in sorted key order so the same
	// tag produces the same prompt on every attempt.
	Meta map[string]string
	// Accounting is the prefix a model call's usage is keyed under:
	// "<prefix>/<n>" for the turn's nth call. saige's own AccountingID, when
	// it reports one, is carried beside it and not used as the key. The caller makes it unique to this execution (pkg/runtime
	// uses the run, the attempt and the delegation path), which makes every
	// call's key unique. Empty is DefaultAccounting.
	Accounting string
	// Park lets the turn end at a question for a person instead of waiting
	// for the answer: see "Parking a turn on a person" in park.go. A turn
	// that may not park (a delegated sub-task, a caller with nobody to ask)
	// refuses every approval and tells the model that ask_person cannot be
	// answered, so it can carry on.
	Park bool
	// QuestionTTL is how long a question the turn parks on waits before its
	// default applies. Zero is DefaultQuestionTTL.
	QuestionTTL time.Duration
	// Resume continues a turn that parked, with the answers its pending tool
	// calls have. History, Text, Meta and Attachments must be the parked
	// turn's; the request is the parked turn's recorded prompt, not Text
	// rendered again.
	Resume *Parked
}

// Result is what a turn produced.
type Result struct {
	// Text is the agent's final answer, the concatenation of its text
	// deltas.
	Text string
	// Prompt is the request message exactly as it was sent to the model:
	// trigger metadata, retrieved passages, and the turn's text. A caller
	// that keeps history records this, not Turn.Text, as what the user said,
	// because a later run only shares a request prefix with this one if it
	// replays the same bytes.
	Prompt string
	// Chunks counts the event bodies the turn emitted, citations included.
	Chunks int
	// Citations counts the retrieved passages the turn was grounded in.
	Citations int
	// Usage is what the turn's own model calls spent, with the share read
	// from and written to the prompt cache and the cost where saige can
	// price the model. It is reported for a failed turn too, since the
	// tokens were spent either way. Calls made by agents this turn delegated
	// to are not in it; they are reported as run.usage.v1 bodies of their own.
	Usage event.Usage
	// Compactions lists each time the turn's history was summarized, in
	// order, as each was also published (run.context.compacted.v1). It is
	// empty unless WithCompaction set a budget the turn crossed, or let a
	// host ask and one did.
	Compactions []event.Compaction
	// Stopped is set when the turn ended because it was asked to stop, and
	// says why and by whom. Text is then what had been answered so far, which
	// may be nothing. It is nil for a turn that finished before the request
	// took effect: that turn completed, and its Text is the whole answer.
	Stopped *Stop
	// Parked is set when the turn ended at a question for a person: Text is
	// what it had said so far, and a turn with Turn.Resume continues it.
	Parked *Parked
}

// Runner runs turns of one pinned revision.
//
// Construction validates everything a turn needs (the provider exists and is
// configured, every approval names a granted tool), so a run fails at accept
// time with a clear reason instead of halfway through a Slack thread. A granted
// tool this process cannot resolve is the exception: it is dropped, the model
// is told, and the operator is logged the reason (see Unavailable), unless the
// runner is built WithStrictTools. A Runner
// is safe for concurrent use only insofar as its provider and tools are; each
// turn gets its own conversation tree.
type Runner struct {
	rev      Revision
	provider saigetypes.Provider
	// models are the providers behind provider, primary first: provider
	// itself when the spec names no fallbacks. Usage is priced per member.
	models    []saigetypes.Provider
	tools     *saigetypes.ToolRegistry
	retriever Retriever
	files     *resolve.Resolver
	maxIter   int
	logger    *slog.Logger
	// compaction is the history budget; its zero value never compacts.
	compaction Compaction
	// toolArgs scrubs tool arguments before events carry them; nil
	// publishes them as they are, within event.MaxToolArguments.
	toolArgs ToolArguments
	// unavailable are the granted tools New dropped, in grant order.
	unavailable []UnavailableTool
	// schema is what the final answer is constrained to (Spec.Output);
	// nil is free text.
	schema *saigetypes.ParameterSchema
}

// UnavailableTool is a tool the revision grants that this process could not
// resolve when the runner was built, and why.
type UnavailableTool struct {
	Name string `json:"name"`
	// Reason is the operator's: the error, which may name infrastructure.
	Reason string `json:"reason"`
	// Note is what the model was told about why, when the error offered one
	// (ModelNote). Empty for every other reason, which the model never sees.
	Note string `json:"note,omitempty"`
}

// ModelNote is implemented by an error whose cause the model may be told. A
// granted tool or group dropped for such an error is announced to the model
// with the note beside its name, so the agent can tell the person it is
// talking to what they can do about it (connect a credential of their own),
// where any other reason is the operator's alone because it may name
// infrastructure the model has no business describing. Whoever writes a note
// writes it for the model: one sentence that names nothing the person does
// not already know, and the same every turn for the same person, so it does
// not move the cached prefix.
type ModelNote interface {
	ModelNote() string
}

// unavailableTool is a dropped grant, with the model's note when err has one.
func unavailableTool(name string, err error) UnavailableTool {
	u := UnavailableTool{Name: name, Reason: err.Error()}
	var n ModelNote
	if errors.As(err, &n) {
		u.Note = strings.Join(strings.Fields(n.ModelNote()), " ")
	}
	return u
}

// Option configures a Runner.
type Option func(*config)

type config struct {
	providers map[string]ProviderFunc
	resolver  ProviderResolver
	script    [][]saigetypes.Delta
	tools     Tools
	groups    map[string]ToolGroup
	retriever Retriever
	maxIter   int
	logger    *slog.Logger
	// compaction and promptCache are the context policy; see context.go.
	compaction  Compaction
	promptCache string
	files       *resolve.Resolver
	toolArgs    ToolArguments
	strictTools bool
}

// WithStrictTools makes a granted tool that cannot be resolved fail New with
// the lookup's error (ErrNoSuchTool for one nobody contributes), as it did
// before tools were dropped. Without it the tool is left out, the model is
// told it is unavailable, and the runner reports it (Runner.Unavailable).
// A host that would rather not run an agent at all than run it short of a
// tool passes this.
func WithStrictTools() Option {
	return func(c *config) { c.strictTools = true }
}

// WithToolArguments sets how tool arguments are scrubbed before the events
// of a call carry them. Without it they are published as the model sent
// them, or as an approval edited them, and left out only when they exceed
// event.MaxToolArguments. A deployment whose tools take secrets, or whose
// streams reach people who should not read every argument, passes one.
func WithToolArguments(fn ToolArguments) Option {
	return func(c *config) { c.toolArgs = fn }
}

// WithAttachments lets turns carry files. The resolver reads each attachment
// from the blob store and turns it into what the pinned model can take: the
// bytes themselves where the model has that modality, derived text where it
// does not, and a note saying a file was attached where neither is possible.
//
// Without it a turn that carries attachments is refused. Running it anyway
// would answer a question about a file as though no file had been sent, and a
// confident answer to the wrong question is worse than an error.
func WithAttachments(r *resolve.Resolver) Option {
	return func(c *config) { c.files = r }
}

// WithProvider registers or replaces a provider factory under name. A
// deployment adds its own backends this way; it also overrides a built-in, which
// is how a test pins "anthropic" to a fake without an API key.
func WithProvider(name string, fn ProviderFunc) Option {
	return func(c *config) {
		if name == "" || fn == nil {
			return
		}
		c.providers[name] = fn
	}
}

// WithProviderResolver consults r for a provider no WithProvider named. A
// name registered by WithProvider, or built in, is never passed to it.
func WithProviderResolver(r ProviderResolver) Option {
	return func(c *config) { c.resolver = r }
}

// provider is the factory for name: a registered one, else the resolver's.
func (c *config) provider(name string) (ProviderFunc, bool) {
	if fn, ok := c.providers[name]; ok && fn != nil {
		return fn, true
	}
	if c.resolver == nil {
		return nil, false
	}
	fn, ok := c.resolver(name)
	return fn, ok && fn != nil
}

// WithScript sets the delta sequences the offline provider replays, one per
// model call. It is the zero-infrastructure path: no key, no daemon, no
// network, and the whole tag-to-event path still runs.
func WithScript(script ...[]saigetypes.Delta) Option {
	return func(c *config) { c.script = script }
}

// WithTools sets the catalog the spec's tool names resolve against. Without
// one, every tool a spec names except ask_person is unavailable: dropped and
// announced to the model, or a build failure WithStrictTools. The model is
// always told, because an agent silently running without the tools it was
// defined with would answer the wrong question.
func WithTools(t Tools) Option {
	return func(c *config) { c.tools = t }
}

// WithRetriever grounds turns in a corpus. Without one, a spec's Sources are
// inert and the agent answers from the model alone.
func WithRetriever(r Retriever) Option {
	return func(c *config) { c.retriever = r }
}

// WithMaxIter bounds tool-calling iterations per turn (default DefaultMaxIter).
func WithMaxIter(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxIter = n
		}
	}
}

// WithLogger sets the logger. Default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// New builds a runner for one pinned revision.
func New(rev Revision, opts ...Option) (*Runner, error) {
	if err := validate(rev); err != nil {
		return nil, err
	}
	schema, err := responseSchema(rev)
	if err != nil {
		return nil, err
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, err
	}

	provider, models, err := cfg.modelChain(rev)
	if err != nil {
		return nil, err
	}

	tools, unavailable, err := cfg.toolset(rev)
	if err != nil {
		return nil, err
	}

	return &Runner{
		rev:         rev,
		provider:    provider,
		models:      models,
		tools:       tools,
		unavailable: unavailable,
		retriever:   cfg.retriever,
		files:       cfg.files,
		maxIter:     cfg.maxIter,
		logger:      cfg.logger,
		compaction:  cfg.compaction,
		toolArgs:    cfg.toolArgs,
		schema:      schema,
	}, nil
}

// Granted is what a revision's tool grants resolve to in one process: the
// tools a turn would show the model, and the grants it would drop.
type Granted struct {
	// Tools are the definitions the model is shown, sorted by name: every
	// granted tool that resolved, the members of every group that expanded,
	// and the built-in tools the revision grants.
	Tools []saigetypes.ToolDef `json:"tools"`
	// Unavailable are the grants left out, in grant order, as
	// Runner.Unavailable reports them.
	Unavailable []UnavailableTool `json:"unavailable,omitempty"`
}

// ResolveTools resolves rev's grants with opts exactly as New does and
// builds nothing else. No model client is made, so it needs no model
// credential, and no tool is called, though a catalog that expands a group
// may reach the server behind it, as it would for New. It is how a host
// shows what an agent would be granted before running it. The errors are
// New's: an approval naming a tool the revision is not granted, a malformed
// group, and with WithStrictTools any grant that does not resolve.
func ResolveTools(rev Revision, opts ...Option) (Granted, error) {
	if err := validate(rev); err != nil {
		return Granted{}, err
	}
	cfg, err := newConfig(opts)
	if err != nil {
		return Granted{}, err
	}
	tools, unavailable, err := cfg.toolset(rev)
	if err != nil {
		return Granted{}, err
	}
	return Granted{Tools: tools.Definitions(), Unavailable: unavailable}, nil
}

// newConfig builds the configuration opts describe. The offline and
// Anthropic providers are resolved last because they need what an option
// may have supplied: the script, the cache lifetime. An explicit
// WithProvider for the same name wins: overriding a built-in is the
// documented escape hatch.
func newConfig(opts []Option) (config, error) {
	cfg := config{
		providers: defaultProviders(),
		maxIter:   DefaultMaxIter,
		logger:    slog.Default(),
		// Caching is on by default because it is free to be wrong about: a
		// prefix nobody reads again costs a quarter more to write once, and
		// a tool loop of three calls has already earned that back.
		promptCache: PromptCacheShort,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if err := validPromptCache(cfg.promptCache); err != nil {
		return config{}, err
	}
	if cfg.providers[ProviderOffline] == nil {
		cfg.providers[ProviderOffline] = offlineProvider(cfg.script)
	}
	if cfg.providers[ProviderAnthropic] == nil {
		cfg.providers[ProviderAnthropic] = anthropicProvider(cfg.promptCache)
	}
	return cfg, nil
}

// toolset resolves the tools rev grants: the registry the model is offered
// and the grants dropped, in grant order. A grant that does not resolve is
// dropped unless strictTools, and an approval naming a tool the revision is
// not granted is refused.
func (cfg *config) toolset(rev Revision) (*saigetypes.ToolRegistry, []UnavailableTool, error) {
	// The registry hands tool definitions to the provider sorted by name, so
	// the order a spec lists its tools in never reaches the request. That
	// matters beyond tidiness: tool definitions are the first bytes of the
	// cached prefix, and an order that followed the spec would invalidate
	// the cache for an edit that only reordered a list.
	tools := saigetypes.NewToolRegistry()
	var unavailable []UnavailableTool
	for _, name := range rev.Spec.Tools {
		if IsToolGroup(name) {
			// The members are registered, never the group itself.
			g, err := cfg.group(rev, name)
			if err != nil {
				if cfg.strictTools || errors.Is(err, errMalformedGroup) {
					return nil, nil, err
				}
				// Like a single tool this process cannot honour, a group
				// it cannot expand right now costs the agent those tools,
				// not the run.
				unavailable = append(unavailable, unavailableTool(name, err))
				cfg.logger.Warn("flow/turn/native: a granted tool group is unavailable; the run continues without it",
					"agent", rev.Spec.Name, "version", rev.Version, "group", name, "error", err)
				continue
			}
			for _, t := range g.Tools {
				tools.Register(t)
			}
			continue
		}
		if name == AskPersonToolName {
			// Built in: its answer comes from the turn, which no catalog
			// can know.
			tools.Register(AskPersonTool())
			continue
		}
		var (
			t   saigetypes.Tool
			err error
		)
		if cfg.tools == nil {
			err = fmt.Errorf("%w: %q (no tool catalog is configured)", ErrNoSuchTool, name)
		} else {
			t, err = cfg.tools.Lookup(name)
		}
		if err != nil {
			if cfg.strictTools {
				return nil, nil, fmt.Errorf("native: agent %q version %s: %w", rev.Spec.Name, rev.Version, err)
			}
			// A grant this process cannot honour right now (a connector
			// not configured here, an MCP tool held back or disabled) costs
			// the agent that tool, not the run: the model is told it is
			// missing and the operator is told why.
			unavailable = append(unavailable, unavailableTool(name, err))
			cfg.logger.Warn("flow/turn/native: a granted tool is unavailable; the run continues without it",
				"agent", rev.Spec.Name, "version", rev.Version, "tool", name, "error", err)
			continue
		}
		tools.Register(t)
	}
	// A tool the spec requires approval for carries a saige marker, which is
	// what makes saige stop at its calls and ask.
	for _, name := range rev.Spec.Approvals {
		t, ok := tools.Get(name)
		if !ok {
			if slices.ContainsFunc(unavailable, func(u UnavailableTool) bool {
				prefix := groupMembers(u.Name)
				return u.Name == name || (prefix != "" && strings.HasPrefix(name, prefix))
			}) {
				// Dropped above, alone or with its group, so it cannot be
				// called and there is nothing to approve.
				continue
			}
			return nil, nil, fmt.Errorf("%w: agent %q version %s requires approval for %q, which it is not granted",
				ErrInvalid, rev.Spec.Name, rev.Version, name)
		}
		if _, marked := t.(*saigetypes.MarkedTool); marked {
			continue
		}
		tools.Register(saigetypes.WithMarkers(t, saigetypes.Marker{
			Kind:    MarkerApproval,
			Message: fmt.Sprintf("Allow %s to call %s?", rev.Spec.Name, name),
		}))
	}
	return tools, unavailable, nil
}

// Unavailable reports the granted tools this runner left out because they
// could not be resolved, in the order the revision grants them. It is empty
// for a runner built WithStrictTools, which fails instead.
func (r *Runner) Unavailable() []UnavailableTool { return slices.Clone(r.unavailable) }

// Revision returns the pinned revision this runner executes. Callers assert on
// it: a run that pinned revision 6 must still report 6 after the agent has been
// revised to 7.
func (r *Runner) Revision() Revision { return r.rev }

// SystemPrompt returns the prompt the pinned revision runs with. It exists so a
// caller can prove which definition executed, which a run id and a revision
// number alone cannot show.
func (r *Runner) SystemPrompt() string { return r.rev.Spec.SystemPrompt }

// Run executes one turn, handing every event body to emit as it is produced.
//
// emit is called on Run's goroutine, in order, and an error from it aborts the
// turn: the caller publishing chunks is the caller that cannot keep up or has
// gone away, and continuing to burn model tokens for a stream nobody is reading
// is worse than stopping.
//
// A failed turn returns an error and whatever text was produced before it
// failed, because a partially-answered turn is what a sink has already rendered
// and pretending otherwise would make the thread inconsistent with the run.
//
// A stopped turn returns no error: see Turn.Stop and Result.Stopped.
//
//nolint:gocyclo // a single pass over a turn's stream and loop state; splitting it would scatter shared state
func (r *Runner) Run(ctx context.Context, t Turn, emit func(context.Context, event.Body) error) (Result, error) {
	var res Result

	// A stop that was already waiting when the turn began ends it here,
	// before retrieval and before any model call. The request is recorded as
	// it would have been sent, without retrieved passages, since none were
	// fetched.
	if stop, ok := pendingStop(t.Stop); ok {
		res.Prompt = RequestText(t.Text, t.Meta)
		res.Stopped = stopOf(stop)
		return res, nil
	}

	send := func(c event.Body) error {
		if emit == nil {
			res.Chunks++
			return nil
		}
		if err := emit(ctx, c); err != nil {
			return err
		}
		res.Chunks++
		return nil
	}

	prompt := t.Text
	// retrieved are the citations published from retrieval, which the
	// translator must not publish again. A continuation retrieves nothing,
	// so it cannot tell a tool's citation that repeats one the parked turn
	// published, and publishes it.
	var retrieved []event.Body
	if t.Resume != nil {
		// A continuation sends the request the parked turn sent, retrieved
		// passages and all, and does not retrieve again: its citations were
		// published by that turn, and a second retrieval could differ.
		prompt = t.Resume.Prompt
	} else if r.retriever != nil && len(r.rev.Spec.Sources) > 0 {
		ac, err := r.retriever.Retrieve(ctx, t.Text)
		if err != nil {
			return res, fmt.Errorf("native: agent %q version %s: retrieve: %w", r.rev.Spec.Name, r.rev.Version, err)
		}
		retrieved = Citations(ac)
		for _, c := range retrieved {
			if err := send(c); err != nil {
				return res, err
			}
		}
		res.Citations = len(retrieved)
		if ac != nil && ac.Prompt != "" {
			prompt = ac.Prompt + "\n\n" + prompt
		}
	}
	if t.Resume == nil {
		prompt = event.RequestText(prompt, t.Meta)
	}
	res.Prompt = prompt

	// Files ride on the message they arrived with: a history message's after
	// its text, and the request's on the last message. They are the most
	// volatile thing a turn sends, so they go after everything a later turn
	// could share a cached prefix with.
	input, provider, pending, err := r.continuation(ctx, t, prompt)
	if err != nil {
		return res, fmt.Errorf("native: agent %q version %s: %w", r.rev.Spec.Name, r.rev.Version, err)
	}

	park := &parker{
		enabled: t.Park,
		tools:   r.tools,
		agent:   r.rev.Spec.Name,
		scope:   firstOf(t.Accounting, DefaultAccounting),
		ttl:     t.QuestionTTL,
		now:     time.Now,
		held:    t.Holds,
	}
	if park.ttl <= 0 {
		park.ttl = DefaultQuestionTTL
	}
	if t.Resume != nil {
		park.answers = t.Resume.Answers
	}
	// ask_person reads its answer from the context its call runs under.
	ctx = context.WithValue(ctx, answersKey{}, park.answers)

	agentCfg := saige.Config{
		Name:         r.rev.Spec.Name,
		SystemPrompt: invocationPrompt(unavailableNote(r.rev.Spec.SystemPrompt, r.unavailable), t.Instructions),
		Provider:     provider,
		Tools:        r.tools,
		MaxIter:      r.maxIter,
		Logger:       r.logger,
		Hooks:        []saige.Hooks{park.hooks()},
		// Calls an Approving tool wants approved stop and ask, as a marked
		// tool's do (see park.go).
		ToolGate: approvalGate(r.tools),
		// saige refuses a schema the provider cannot enforce
		// (types.ErrInvalidModelConfig) rather than drop it, and every
		// decorator around the model (retry, the fallback chain, a
		// continuation's replay) forwards that it can.
		ResponseSchema: r.schema,
	}
	if t.Budget != nil {
		b := t.Budget
		if b.CostMicros < 0 || b.Tokens < 0 || b.Requests < 0 || (b.CostMicros == 0 && b.Tokens == 0 && b.Requests == 0) {
			return res, fmt.Errorf("native: empty or invalid budget")
		}
		agentCfg.Budget = saigetypes.NewBudget(saigetypes.BudgetPolicy{Limit: saigetypes.Cost(b.CostMicros), MaxTokens: int(b.Tokens), MaxRequests: int(b.Requests), AllowUnpriced: b.CostMicros == 0})
	}
	// requests is how a host's request to compact reaches the agent loop;
	// nil when this deployment does not take them.
	var requests *compactRequests
	if r.compaction.OnRequest {
		base := saigetypes.EstimatingTokenizer{}
		if o, ok := convert.Target(r.provider); ok {
			base.Offering = &o
		}
		requests = newCompactRequests(base, r.compaction.budget())
		agentCfg.Tokenizer = requests
	}
	if cc := r.compaction.config(); cc != nil {
		agentCfg.CompactCfg = cc
		var guard *compactionGuard
		if r.compaction.MaxInputTokens > 0 {
			guard = newCompactionGuard(r.compaction.keepTurns())
		}
		agentCfg.Hooks = append(agentCfg.Hooks, compactionHooks(guard, requests))
	}
	// The translator's hook goes last, so the arguments it records are the
	// ones every other hook has finished with: what the tool runs with.
	translator := newTranslator(r.toolArgs)
	translator.published(retrieved)
	if pending != nil {
		// A continuation's first call is answered with the recorded message
		// and its deltas are dropped (see replaying), so the translator is
		// told the model's arguments from that message instead.
		translator.seed(*pending)
	}
	agentCfg.Hooks = append(agentCfg.Hooks, translator.hooks())
	agent, err := saige.New(agentCfg)
	if err != nil {
		return res, fmt.Errorf("native: agent %q version %s: %w", r.rev.Spec.Name, r.rev.Version, err)
	}

	// watched is closed when the watcher goroutine below returns. requests
	// learns it before the loop starts, so catching up with a watcher that
	// has stopped never blocks.
	watched := make(chan struct{})
	var caughtUp chan struct{}
	if requests != nil {
		requests.watch(watched)
		caughtUp = requests.sync
	}
	stream := agent.Invoke(ctx, input)
	accounts := newAccountant(t.Accounting, r.rev.Spec.Name, r.rev.Spec.Model, r.provider, r.models...)

	// The stop request is applied from its own goroutine, because this one is
	// busy draining the stream and saige's Stop is safe to call from anywhere.
	// The goroutine ends with the turn: settle runs on every return path
	// below, which is what makes "nothing is read from Turn.Stop once the
	// turn has returned" true rather than likely.
	stopping := newStopper(stream)
	finished := make(chan struct{})
	go func() {
		defer close(watched)
		compact := t.Compact
		for {
			select {
			case stop := <-t.Stop:
				stopping.apply(stop)
				return
			case req, ok := <-compact:
				switch {
				case !ok:
					compact = nil
				case requests != nil:
					requests.request(req)
				default:
					r.logger.Debug("flow/turn/native: a request to compact was ignored; this deployment does not take them",
						"agent", r.rev.Spec.Name)
				}
			case <-caughtUp:
				// The loop is about to measure, and every request taken
				// above is already recorded.
			case <-finished:
				return
			}
		}
	}()
	// settle is deferred as well as called: a panic anywhere below (an emit
	// that panics, say) must not leave the watcher waiting forever.
	var settled sync.Once
	settle := func() {
		settled.Do(func() {
			close(finished)
			<-watched
		})
	}
	defer settle()

	var answer strings.Builder
	var streamErr error
	replay := newReplaying(pending)
	for d := range stream.Deltas() {
		if replay.drop(d) {
			continue
		}
		if id, ok := batchDelta(d); ok && park.holds(id) {
			// The batch the turn parked on never ran; saige's account of
			// cancelling it is not news.
			continue
		}
		switch v := d.(type) {
		case saigetypes.PartDelta:
			answer.WriteString(v.Text)
		case saigetypes.ErrorDelta:
			// Failure is a lifecycle fact, not an event of its own (see
			// Translator.Delta). Keep the first one and let the stream
			// drain: abandoning the channel would leak the producer.
			if streamErr == nil && v.Error != nil {
				streamErr = v.Error
			}
		case saigetypes.StoppedDelta:
			// The turn wrapped up on request. Like failure it is a
			// lifecycle fact and not an event of its own; the request it
			// answers is read off the stream once it has drained.
		case saigetypes.UsageDelta:
			// One model call, reported as it completes so that a call the
			// turn never finishes (a failure, a stop) is still on the
			// record. See "Usage" in the package documentation.
			call := accounts.call(v)
			res.Usage = res.Usage.AddCall(call)
			c, ok := bodyOf[event.UsageCall](call)
			if !ok {
				continue
			}
			if err := send(c); err != nil {
				stream.Cancel()
				drain(stream)
				settle()
				return res, err
			}
			continue
		case saigetypes.CompactionDelta:
			rec := compactionRecord(v)
			if requests != nil {
				rec = requests.record(rec)
			}
			res.Compactions = append(res.Compactions, rec)
			c, ok := bodyOf[event.Compaction](rec)
			if !ok {
				continue
			}
			if err := send(c); err != nil {
				stream.Cancel()
				drain(stream)
				settle()
				return res, err
			}
			continue
		case saigetypes.MarkerDelta:
			// A tool call waits for approval. A call this turn was given the
			// answer to is answered with it. Any other is refused: the turn is
			// ending at its question (see park.go) and the call will be asked
			// again, with the answer, when the turn is continued, or the turn
			// cannot park at all and the model is told nobody can approve it.
			// A refusal never runs the tool, and nothing is left waiting.
			//
			// A refusal that cannot be delivered would leave the tool
			// waiting, so it fails the turn and stops the stream instead.
			err := stream.ResolveMarkerErr(v.ToolCallID, park.decide(v.ToolCallID))
			if err != nil {
				streamErr = errors.Join(streamErr, fmt.Errorf("refuse approval for tool call %s: %w", v.ToolCallID, err))
				stream.Cancel()
			}
		}
		c, ok := translator.Delta(d)
		if !ok {
			continue
		}
		if err := send(c); err != nil {
			stream.Cancel()
			drain(stream)
			settle()
			return res, err
		}
	}

	res.Text = answer.String()
	waitErr := stream.Wait()
	settle()
	if _, stopped := stream.Stopped(); !stopped && park.isParking() {
		if _, cancelled := stopping.cancelled(); !cancelled {
			parked, err := r.parkedOf(agent, park, t, len(res.Compactions) > 0)
			if err != nil {
				return res, fmt.Errorf("native: agent %q version %s: park: %w", r.rev.Spec.Name, r.rev.Version, err)
			}
			parked.Prompt = res.Prompt
			res.Parked = parked
			return res, nil
		}
	}
	if req, ok := stream.Stopped(); ok {
		// saige wrapped the turn up: the partial text is kept and the stream
		// ended cleanly.
		res.Stopped = &Stop{Reason: req.Reason, By: req.By}
		return res, nil
	}
	if stop, ok := stopping.cancelled(); ok {
		// The stream could not wrap up on request and was cancelled instead.
		// What the caller asked for still happened, with whatever text had
		// arrived, so it is reported as a stop and not as the cancellation
		// it was implemented with.
		res.Stopped = stopOf(stop)
		return res, nil
	}
	if waitErr != nil {
		streamErr = errors.Join(streamErr, waitErr)
	}
	if streamErr != nil && t.Budget != nil && errors.Is(streamErr, saigetypes.ErrBudgetExceeded) && agentCfg.Budget.Uncertain() == 0 {
		// Reaching an exact cap is an intentional stop. Preserve the answer already
		// produced; saige still refused every request beyond the reservation.
		res.Stopped = &Stop{Reason: "Budget reached"}
		return res, nil
	}
	if streamErr != nil {
		return res, fmt.Errorf("native: agent %q version %s: %w", r.rev.Spec.Name, r.rev.Version, streamErr)
	}
	return res, nil
}

// conversation builds the turn's input: the history, then the request, with
// every message's attachments resolved for the model this revision pins.
//
// One budget is spent across every file in the order the messages are sent,
// history first. That is what makes a message render the same bytes in
// every run that replays it (see "History across runs").
func (r *Runner) conversation(ctx context.Context, t Turn, prompt string) ([]saigetypes.Message, error) {
	input, err := conversation(t.History, prompt)
	if err != nil {
		return nil, err
	}
	var budget *resolve.Budget
	for i, m := range t.History {
		if len(m.Attachments) == 0 {
			continue
		}
		if budget == nil {
			budget = resolve.NewBudget()
		}
		msg, err := r.withFiles(ctx, t.Tenant, m.Attachments, m.Text, budget)
		if err != nil {
			return nil, fmt.Errorf("history message %d of %d: %w", i+1, len(t.History), err)
		}
		input[i] = msg
	}
	if len(t.Attachments) > 0 {
		if budget == nil {
			budget = resolve.NewBudget()
		}
		msg, err := r.withFiles(ctx, t.Tenant, t.Attachments, prompt, budget)
		if err != nil {
			return nil, err
		}
		input[len(input)-1] = msg
	}
	return input, nil
}

// withFiles builds a user message of text followed by its attachments,
// resolved for the model this revision pins. A provider that does not report
// what it accepts is treated as text only: showing a model bytes it cannot
// read fails the call, while derived text always works.
func (r *Runner) withFiles(ctx context.Context, tenant string, files []attachment.Attachment, text string, budget *resolve.Budget) (saigetypes.Message, error) {
	if r.files == nil {
		return nil, fmt.Errorf("%w: the turn carries %d attachments and this deployment has no attachment resolver", ErrInvalid, len(files))
	}
	var media saigetypes.ContentSupport
	if caps, ok := saigetypes.ProviderCapabilities(r.provider); ok {
		media = caps.Media
	}
	out, err := r.files.Resolve(ctx, resolve.Request{Tenant: tenant, Attachments: files, Model: media, Budget: budget})
	if err != nil {
		return nil, fmt.Errorf("attachments: %w", err)
	}
	return out.Message(text), nil
}

// stopOf normalizes a stop the way saige does, so every path reports the same
// reason for a request that gave none.
func stopOf(s Stop) *Stop {
	s.Reason = strings.TrimSpace(s.Reason)
	if s.Reason == "" {
		s.Reason = DefaultStopReason
	}
	return &s
}

// pendingStop reports a stop request that is already waiting, without
// blocking.
func pendingStop(ch <-chan Stop) (Stop, bool) {
	select {
	case stop := <-ch:
		return stop, true
	default:
		return Stop{}, false
	}
}

// stopper applies one stop request to a stream and remembers whether it had to
// fall back to cancelling it.
type stopper struct {
	stream *saige.EventStream

	mu       sync.Mutex
	fallback *Stop
}

func newStopper(stream *saige.EventStream) *stopper { return &stopper{stream: stream} }

// apply asks the stream to wrap up. A stream that cannot (saige refuses it for
// a replayed or remote stream and under a durable step runner) is cancelled,
// which ends it as surely and keeps less.
//
// A stream that has already ended is left alone: the turn finished before the
// request arrived and its outcome stands.
func (s *stopper) apply(stop Stop) {
	err := s.stream.Stop(saige.StopRequest{Reason: stop.Reason, By: stop.By})
	if err == nil || errors.Is(err, saige.ErrRunFinished) {
		return
	}
	s.mu.Lock()
	s.fallback = &stop
	s.mu.Unlock()
	s.stream.Cancel()
}

// cancelled reports the stop that was carried out by cancelling the stream.
func (s *stopper) cancelled() (Stop, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fallback == nil {
		return Stop{}, false
	}
	return *s.fallback, true
}

// drain empties a cancelled stream so its producing goroutine can exit.
func drain(s *saige.EventStream) {
	for range s.Deltas() {
	}
}

// conversation lays out what a turn sends after the system prompt: the
// history, then the request.
//
// Each history entry becomes exactly one message, in the order given, and
// entries are never merged, reordered or decorated. Merging two consecutive
// messages of one role would look tidier and would break the property this
// exists for: the message a run sent last would change shape once a later
// message joined it, and the next run's request would stop being an extension
// of this one.
//
// An entry this package cannot render is refused rather than skipped. A
// history with a hole in it is a different conversation, and an agent that
// answers a different conversation is worse than a turn that fails.
func conversation(history []Message, prompt string) ([]saigetypes.Message, error) {
	out := make([]saigetypes.Message, 0, len(history)+1)
	for i, m := range history {
		if strings.TrimSpace(m.Text) == "" && (m.Role != RoleUser || len(m.Attachments) == 0) {
			return nil, fmt.Errorf("%w: history message %d of %d has no text", ErrInvalid, i+1, len(history))
		}
		switch m.Role {
		case RoleUser:
			out = append(out, saigetypes.UserMsg(saigetypes.Text(m.Text)))
		case RoleAssistant:
			if len(m.Attachments) > 0 {
				return nil, fmt.Errorf("%w: history message %d of %d is the agent's answer and carries attachments", ErrInvalid, i+1, len(history))
			}
			out = append(out, saigetypes.AssistantMsg(saigetypes.Text(m.Text)))
		default:
			return nil, fmt.Errorf("%w: history message %d of %d has role %q, want %q or %q",
				ErrInvalid, i+1, len(history), m.Role, RoleUser, RoleAssistant)
		}
	}
	return append(out, saigetypes.UserMsg(saigetypes.Text(prompt))), nil
}

func providerNames(m map[string]ProviderFunc) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v != nil {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var _ Tools = ToolsFunc(nil)

// continuation builds a turn's input and the provider it calls. A fresh turn
// sends its history and request to the revision's provider. A continuation
// sends the conversation its parked turn had, and its first call is answered
// with the message that turn parked on (see replayFirst).
func (r *Runner) continuation(ctx context.Context, t Turn, prompt string) ([]saigetypes.Message, saigetypes.Provider, *saigetypes.AssistantMessage, error) {
	if t.Resume == nil {
		input, err := r.conversation(ctx, t, prompt)
		return input, r.provider, nil, err
	}
	var input []saigetypes.Message
	if !t.Resume.Whole {
		base, err := r.conversation(ctx, t, prompt)
		if err != nil {
			return nil, nil, nil, err
		}
		input = base
	}
	for i, raw := range t.Resume.Transcript {
		m, err := DecodeMessage(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parked transcript message %d: %w", i+1, err)
		}
		input = append(input, m)
	}
	m, err := DecodeMessage(t.Resume.Pending)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parked message: %w", err)
	}
	pending, ok := m.(saigetypes.AssistantMessage)
	if !ok {
		return nil, nil, nil, fmt.Errorf("%w: the parked message is a %s message, want assistant", ErrInvalid, m.Role())
	}
	return input, newReplayFirst(r.provider, pending), &pending, nil
}

// parkedOf records where a turn that parked stopped.
func (r *Runner) parkedOf(agent *saige.Agent, park *parker, t Turn, compacted bool) (*Parked, error) {
	tr := agent.Tree()
	branch, err := tr.FlattenBranch(tr.Active())
	if err != nil {
		return nil, err
	}
	whole := compacted || (t.Resume != nil && t.Resume.Whole)
	return park.parked(branch, len(t.History)+1, whole)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// unavailableNote tells the model which granted tools it does not have, after
// the spec's own prompt, so an agent whose instructions say "use jira_create"
// says it cannot rather than inventing the call. It names the tools and not
// the reasons: a reason is for the operator, and may name infrastructure the
// model has no business describing. The exception is a reason that was
// written for the model (ModelNote), such as "the person you act for has not
// connected a credential for issues", which follows its tool's name on a line
// of its own. The note follows the deployment, and the person a turn acts
// for, not the turn, so two turns of a revision on one deployment for one
// person send the same prompt.
func unavailableNote(system string, unavailable []UnavailableTool) string {
	if len(unavailable) == 0 {
		return system
	}
	names := make([]string, len(unavailable))
	for i, u := range unavailable {
		names[i] = u.Name
	}
	out := system + "\n\nThese tools you were granted are unavailable in this deployment, so do not try to call them: " +
		strings.Join(names, ", ") + "."
	for _, u := range unavailable {
		if u.Note != "" {
			out += "\n" + u.Name + ": " + u.Note
		}
	}
	return out
}

func invocationPrompt(system, instructions string) string {
	if instructions == "" {
		return system
	}
	return system + "\n\nInvocation return contract (subject to the agent rules above):\n" + instructions
}

// bodyOf copies an accounting record into the event body of the same JSON
// shape. A record that does not copy is a programming error in this package,
// so the body is dropped rather than failing a live turn over it.
func bodyOf[T event.Body](v any) (T, bool) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, false
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false
	}
	return out, true
}
