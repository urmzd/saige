package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	saige "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/provider/wrapper"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// Parking a turn on a person.
//
// A turn can need a person in two ways, and both are saige's own vocabulary:
//
//   - A tool call that needs approval. A tool carries saige markers
//     (types.MarkedTool), either because the host's catalog wrapped it or
//     because the agent's spec lists it in Approvals; or it implements
//     Approving and its Approval says this particular call needs one, which
//     saige is told through a tool gate. saige stops at the call and asks;
//     the answer is approve, deny, or approve with edited arguments.
//   - The ask_person tool (AskPersonTool), which the agent calls to put a
//     question to a person: a choice among options, or a free-text reply. Its
//     result is the answer.
//
// A turn cannot wait for the answer: a durable run must release its worker
// while a person thinks. So the turn ends at the question, and a later turn
// continues it with the answer. This file is how the two turns meet.
//
// # Ending at the question
//
// When the model's message asks for a tool call that needs a person and has no
// answer, nothing in that message's batch of tool calls runs: an unmarked call
// next to the question would otherwise take effect before the person decided,
// and again when the batch is continued. The turn ends with Result.Parked: the
// question, the model's message (Parked.Pending), and the conversation up to
// it (Parked.Transcript), exactly as saige holds them.
//
// # One park for the whole batch
//
// The turn parks at the level of the batch, not of the call. Every call of
// the message that waits and has no answer yet is a wait of the park
// (Parked.Batch, in call order): an approval, an ask_person question, a call
// to an Awaiting tool. So a message that delegates to three agents parks
// once, holding all three waits, and the runtime starts the three child runs
// side by side instead of one after the other's answer. A call waits on at
// most one thing per park: a call that needs both an approval and an awaited
// result waits on the approval first, and on its result at the park that
// follows the approval, as before.
//
// # Continuing with the answer
//
// The continuation is a turn with Turn.Resume. Its input is the same
// conversation the parked turn had, rebuilt from the same pinned history and
// request, then the transcript. Its first model call is not sent: it is
// answered with the recorded message, so saige executes that batch itself,
// with the answer, through its own tool path (markers, hooks, schema checks),
// and every later call sends exactly the bytes an uninterrupted turn would
// have sent. That is what keeps the provider's prompt cache warm across a
// park, and why the continuation is a new turn rather than a resumed process.
// The recorded call's deltas and usage are not published again: they were the
// parked turn's, and that turn already published and counted them.
//
// A batch with several waits is continued once, when every wait has its
// answer: the continuation carries them all, and the batch runs once, each
// call with its own answer, the calls that never waited included. A call
// that did not wait has not run before then, so it runs exactly once, and
// its result takes its place in call order like any other.
//
// # Holding a turn for the host
//
// A host can also ask that a run wait for a person, without the agent asking
// anything: a run that reached a spending limit, say (Turn.Holds). The turn
// reads the run's holds whenever the model sends a message with tool calls,
// before any of them runs, and parks that batch on the oldest hold its
// answers do not settle, exactly as on an approval: the question is of kind
// event.QuestionHold, nothing in the batch runs, and the continuation runs the
// batch once the hold is approved. A message with no tool calls ends the turn
// and is not held: the answer it gives is already spent. The runtime never
// continues a turn whose hold was denied; it stops the run instead.

// AskPersonToolName is the built-in tool an agent calls to ask a person a
// question. An agent has it when its spec lists it in Tools.
const AskPersonToolName = "ask_person"

// DefaultQuestionTTL is how long a question waits for an answer before its
// default applies.
const DefaultQuestionTTL = 24 * time.Hour

// MaxQuestionText bounds the free text of a question and its options, in
// bytes. It ends up in an event every sink is sent.
const MaxQuestionText = 4096

// MaxOptions bounds the options of a choice.
const MaxOptions = 32

// Parked is a turn that ended at a question. It is everything a later turn
// needs to continue it.
type Parked struct {
	// Question is what the turn is waiting on a person for: the first wait
	// of its batch.
	Question event.Question `json:"question"`
	// Prompt is the request the turn sent, exactly: a continuation sends it
	// again rather than rendering or retrieving anew.
	Prompt string `json:"prompt"`
	// CallID is where the answer goes among Answers: the tool call the
	// question is about, or, for a delegation's result, the call's id with
	// "/await" after it.
	CallID string `json:"call_id"`
	// Batch is every wait of the batch, in call order, Question and CallID
	// first, when the batch waits on more than one thing. Empty when it
	// waits on one, which keeps a single wait's record as it always was.
	// Waits reads either form.
	Batch []Wait `json:"batch,omitempty"`
	// Since is set by a runtime that stores a parked turn's record as the
	// part of the transcript after the first Since messages of the previous
	// park's: Transcript is then only what the turn added. A continuation
	// is always given the whole transcript.
	Since int `json:"since,omitempty"`
	// Transcript is the conversation the turn had when it parked, oldest
	// first, as saige messages in their persisted wire form (see
	// EncodeMessage). Without Whole it is only what the turn added after the
	// request; with Whole it is the whole conversation after the system
	// prompt, because the turn compacted and the history it started from is
	// no longer its prefix.
	Transcript []json.RawMessage `json:"transcript,omitempty"`
	Whole      bool              `json:"whole,omitempty"`
	// Pending is the model's message whose tool calls wait for the answer.
	Pending json.RawMessage `json:"pending"`
	// Answers are the answers the pending batch already has, by tool call
	// id: earlier questions about the same message.
	Answers map[string]event.Answer `json:"answers,omitempty"`
}

// Wait is one thing a parked batch waits on: a question, and where its
// answer goes among Parked.Answers.
type Wait struct {
	Question event.Question `json:"question"`
	CallID   string         `json:"call_id"`
}

// Waits returns every wait of the parked batch, in call order.
func (p Parked) Waits() []Wait {
	if len(p.Batch) > 0 {
		return p.Batch
	}
	return []Wait{{Question: p.Question, CallID: p.CallID}}
}

// parkedReason is what the hooks that end a turn at its question tell saige.
// It never reaches a caller: the turn reports Result.Parked instead.
const parkedReason = "waiting on a person"

// AskPersonTool returns the ask_person tool. native registers it for any
// agent whose spec names it; a host can register it in a catalog of its own
// too.
func AskPersonTool() saigetypes.Tool { return askPerson{} }

type askPerson struct{}

func (askPerson) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{
		Name: AskPersonToolName,
		Description: "Ask a person a question and wait for the answer. Give options to ask them to choose one; " +
			"leave them out to ask for a reply in their own words. Use it when the work needs a decision or " +
			"information only a person has. The run waits, possibly for a long time, so ask once, clearly.",
		Parameters: saigetypes.ParameterSchema{
			Type:     saigetypes.SchemaObject,
			Required: []string{"question"},
			Properties: map[string]saigetypes.PropertyDef{
				"question": {Type: saigetypes.SchemaString, Description: "One clear question."},
				"options": {Type: "array", Items: &saigetypes.PropertyDef{Type: saigetypes.SchemaString},
					Description: "The answers to choose from, when the answer is one of a few. Leave out for a free-text reply."},
				"default": {Type: saigetypes.SchemaString,
					Description: "The answer to use if nobody answers in time. For a choice it must be one of the options. Leave out when there is no safe default."},
				"asked_of": {Type: saigetypes.SchemaString,
					Description: "Who should answer, as a role or a person, if the request says. Leave out otherwise."},
			},
		},
		Capability: saigetypes.ToolCapabilityRead,
	}
}

// Execute returns the answer the turn was continued with. Outside a
// continuation there is none: the turn either parked before this ran, or it is
// one that cannot wait for a person (a delegated sub-task), and the model is
// told so.
func (askPerson) Execute(ctx context.Context, args map[string]any) (string, error) {
	if _, err := parseAsk(args); err != nil {
		// A call that is not a question never parked: say what was wrong.
		return "", err
	}
	info, _ := saigetypes.ToolCallInfoFromContext(ctx)
	answers, _ := ctx.Value(answersKey{}).(map[string]event.Answer)
	a, ok := answers[info.ID]
	if !ok {
		return "", errors.New("no person can be asked from here; carry on without the answer")
	}
	switch {
	case a.Choice != "":
		return a.Choice, nil
	case a.Text != "":
		return a.Text, nil
	}
	return "", errors.New("nobody answered before the question expired")
}

type answersKey struct{}

// MaxApprovers bounds the approvers an ApprovalRequest names. They travel on
// the question, in an event every sink is sent.
const MaxApprovers = 32

// ApprovalRequest is the approval one tool call needs, as an Approving tool
// describes it.
type ApprovalRequest struct {
	// Prompt is the question, for a person. Empty asks "Allow <agent> to
	// call <tool>?".
	Prompt string
	// AskedOf is a hint at who should answer, as on event.Question. It
	// admits nobody.
	AskedOf string
	// Approvers, when set, are the only people who may answer this call's
	// approval, named as a spec's Approvers are (principals, or
	// "role:<name>"), in place of the spec's. Empty leaves the spec's rule.
	Approvers []string
	// SelfApproval lets the person whose request raised the run approve
	// this call. It counts only with Approvers: without them the spec's
	// SelfApproval governs.
	SelfApproval bool
	// ExpiresIn is how long the question waits before it is denied. Zero
	// keeps the runner's question lifetime. It is capped at MaxApprovalWait.
	ExpiresIn time.Duration
}

// MaxApprovalWait bounds how long an Approving tool may ask a question to
// wait for an answer.
const MaxApprovalWait = 7 * 24 * time.Hour

// Approving is a tool whose calls may each need a person's approval, decided
// per call rather than per tool: a delegation tool that asks only when its
// callee's policy says so, say. Approval reports whether call needs one and
// what to ask. It decides from call.Name and call.Arguments alone (call.ID
// may be empty), must answer the same for the same arguments, and must be
// safe for concurrent use.
//
// An approved call runs, with the arguments the person approved; a denied or
// expired one does not, and the model is told why. A turn that cannot park
// refuses the call. A tool that is also listed in a spec's Approvals is asked
// about once, with the question Approval describes when it asks.
type Approving interface {
	Approval(call saigetypes.ToolCallPart) (ApprovalRequest, bool)
}

// PersonScoped is a tool that may run with the credential of the person the
// turn acts for, as a person-scoped MCP provider's tools do (pkg/mcptools).
// ActsForPerson reports whether it does. An approval of a call to one says
// so (event.QuestionTool.ActsForPerson), and the runtime takes edited
// arguments for it only from that person.
type PersonScoped interface {
	ActsForPerson() bool
}

// actsForPerson reports whether t, unwrapped from the marker a spec's
// Approvals adds, runs with the credential of the person the turn acts for.
func actsForPerson(t saigetypes.Tool) bool {
	if marked, ok := t.(*saigetypes.MarkedTool); ok {
		t = marked.Inner
	}
	p, ok := t.(PersonScoped)
	return ok && p.ActsForPerson()
}

// approvingOf returns the Approving behind a registered tool, unwrapping the
// marker a spec's Approvals adds.
func approvingOf(tools *saigetypes.ToolRegistry, name string) (Approving, saigetypes.Tool, bool) {
	t, ok := tools.Get(name)
	if !ok {
		return nil, nil, false
	}
	inner := t
	if marked, ok := t.(*saigetypes.MarkedTool); ok {
		inner = marked.Inner
	}
	a, ok := inner.(Approving)
	return a, t, ok
}

// approvalGate tells saige which calls an Approving tool wants approved, so
// saige stops at them and asks exactly as it does at a marked tool: the ask
// reaches the turn as a MarkerDelta, which decide answers. Every other call
// is allowed, leaving markers to the marker path.
func approvalGate(tools *saigetypes.ToolRegistry) saigetypes.ToolGate {
	return saigetypes.GateFunc(func(_ context.Context, def saigetypes.ToolDef, args map[string]any) saigetypes.GateDecision {
		a, _, ok := approvingOf(tools, def.Name)
		if !ok {
			return saigetypes.Allow()
		}
		req, ok := a.Approval(saigetypes.ToolCallPart{Name: def.Name, Arguments: args})
		if !ok {
			return saigetypes.Allow()
		}
		d := saigetypes.RequireApproval(req.Prompt)
		d.Marker = &saigetypes.Marker{Kind: MarkerApproval, Message: req.Prompt}
		return d
	})
}

// cleanApprovers trims, bounds and de-duplicates an Approving tool's
// approvers, keeping their order.
func cleanApprovers(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, who := range in {
		who = clipText(who)
		if who == "" || seen[who] || len(out) == MaxApprovers {
			continue
		}
		seen[who] = true
		out = append(out, who)
	}
	return out
}

// Awaiting is a tool whose result does not come from running it in the turn
// but arrives later from outside: a delegation to a child run that the
// runtime starts and that answers when it ends. A turn that may park ends at a
// call to it, as at a question, with the question AwaitQuestion describes,
// and the continuation runs the call with the answer it was given (AnswerFor).
type Awaiting interface {
	AwaitQuestion(call saigetypes.ToolCallPart) (event.Question, bool)
}

// AnswerFor returns the answer the tool call ctx belongs to was continued
// with, for an Awaiting tool's Execute.
func AnswerFor(ctx context.Context) (event.Answer, bool) {
	info, ok := saigetypes.ToolCallInfoFromContext(ctx)
	if !ok {
		return event.Answer{}, false
	}
	answers, _ := ctx.Value(answersKey{}).(map[string]event.Answer)
	a, ok := answers[awaitKey(info.ID)]
	return a, ok
}

// awaitKey is where a call's awaited result is kept among a batch's answers.
// It is apart from the call's own id because one call can need both: an
// approval to start a delegation, then the delegation's result.
func awaitKey(callID string) string { return callID + "/await" }

// askArgs are ask_person's arguments, validated.
type askArgs struct {
	question string
	options  []string
	def      string
	askedOf  string
}

// parseAsk reads ask_person's arguments. A call it cannot read is not a
// question: the tool runs and tells the model what was wrong.
func parseAsk(args map[string]any) (askArgs, error) {
	var a askArgs
	a.question = clipText(stringArg(args, "question"))
	if a.question == "" {
		return a, errors.New("ask_person needs a question")
	}
	a.def = clipText(stringArg(args, "default"))
	a.askedOf = clipText(stringArg(args, "asked_of"))
	if raw, ok := args["options"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return a, errors.New("ask_person options must be a list of strings")
		}
		seen := map[string]bool{}
		for _, v := range list {
			s, ok := v.(string)
			s = clipText(s)
			if !ok || s == "" {
				return a, errors.New("ask_person options must be non-empty strings")
			}
			if !seen[s] {
				seen[s] = true
				a.options = append(a.options, s)
			}
		}
		if len(a.options) > MaxOptions {
			return a, fmt.Errorf("ask_person offers %d options; the limit is %d", len(a.options), MaxOptions)
		}
	}
	if a.def != "" && len(a.options) > 0 && !contains(a.options, a.def) {
		return a, errors.New("ask_person's default must be one of its options")
	}
	return a, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// clipText trims free text and bounds it, on a rune boundary.
func clipText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= MaxQuestionText {
		return s
	}
	end := MaxQuestionText
	for end > 0 && s[end]&0xC0 == 0x80 {
		end--
	}
	return s[:end]
}

// parker decides whether a turn parks, and ends it at the question when it
// does. Its hooks run on saige's loop goroutine and on tool goroutines, and the
// delta loop reads it, so its state is guarded.
type parker struct {
	enabled bool
	tools   *saigetypes.ToolRegistry
	agent   string
	scope   string
	ttl     time.Duration
	now     func() time.Time
	answers map[string]event.Answer
	held    turn.Holds

	mu      sync.Mutex
	parking bool
	waits   []Wait
	pending *saigetypes.AssistantMessage
	batch   map[string]bool
}

func (p *parker) hooks() saige.Hooks {
	return saige.Hooks{
		Name: "fiber.park",
		AfterModelCall: func(ctx context.Context, ev *saige.AfterModelCallEvent) error {
			if len(ev.Path) > 0 || ev.Message == nil || ev.Err != nil {
				return nil
			}
			p.afterModel(ctx, *ev.Message)
			return nil
		},
		BeforeTool: func(_ context.Context, ev *saige.BeforeToolEvent) error {
			if len(ev.Path) == 0 && p.holds(ev.Call.ID) {
				return saige.Abort(parkedReason)
			}
			return nil
		},
		BeforeModelCall: func(_ context.Context, ev *saige.BeforeModelCallEvent) error {
			if len(ev.Path) == 0 && p.isParking() {
				return saige.Abort(parkedReason)
			}
			return nil
		},
	}
}

// afterModel looks at a message the model just sent, before any of its tool
// calls run. Every call that needs a person or awaits a result and has no
// answer is a wait, and any wait parks the whole batch, once, on all of them.
func (p *parker) afterModel(ctx context.Context, msg saigetypes.AssistantMessage) {
	if !p.enabled {
		return
	}
	var calls []saigetypes.ToolCallPart
	for _, part := range msg.Parts {
		if tc, ok := part.(saigetypes.ToolCallPart); ok {
			calls = append(calls, tc)
		}
	}
	waits := p.holdWait(ctx, calls)
	if len(waits) == 0 {
		waits = p.callWaits(calls)
	}
	if len(waits) == 0 {
		return
	}
	batch := make(map[string]bool, len(calls))
	for _, tc := range calls {
		batch[tc.ID] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.parking {
		return
	}
	p.parking, p.waits, p.batch = true, waits, batch
	pending := msg
	p.pending = &pending
}

// holdWait is the wait a host's hold puts on a batch: the oldest hold its
// answers do not settle, alone. A hold comes first: nothing in the batch
// runs, and nothing it waits on starts, until the hold is settled.
func (p *parker) holdWait(ctx context.Context, calls []saigetypes.ToolCallPart) []Wait {
	if len(calls) == 0 || p.held == nil {
		return nil
	}
	for _, h := range p.held(ctx) {
		if _, settled := p.answers[holdKey(h.ID)]; settled || strings.TrimSpace(h.ID) == "" {
			continue
		}
		return []Wait{{Question: p.hold(h), CallID: holdKey(h.ID)}}
	}
	return nil
}

// callWaits is what each call of a batch waits on, in call order: its
// approval or question while it has no answer, else the result it awaits
// while that has not arrived. A call waits on one thing per park, so a
// delegation that needs approval starts only once it is approved.
func (p *parker) callWaits(calls []saigetypes.ToolCallPart) []Wait {
	var waits []Wait
	for _, tc := range calls {
		if _, answered := p.answers[tc.ID]; !answered {
			if q, ok := p.need(tc); ok {
				waits = append(waits, Wait{Question: q, CallID: tc.ID})
				continue
			}
		}
		if _, arrived := p.answers[awaitKey(tc.ID)]; !arrived {
			if q, ok := p.await(tc); ok {
				waits = append(waits, Wait{Question: q, CallID: awaitKey(tc.ID)})
			}
		}
	}
	return waits
}

// need reports the question a call puts to a person, if it puts one.
func (p *parker) need(tc saigetypes.ToolCallPart) (event.Question, bool) {
	now := p.now().UTC()
	q := event.Question{Agent: p.agent, At: now, ExpiresAt: now.Add(p.ttl)}
	if tc.Name == AskPersonToolName {
		a, err := parseAsk(tc.Arguments)
		if err != nil {
			return event.Question{}, false
		}
		q.Prompt, q.AskedOf = a.question, a.askedOf
		if len(a.options) > 0 {
			q.Kind, q.Options = event.QuestionChoice, a.options
			if a.def != "" {
				q.Default = &event.Answer{Choice: a.def, Expired: true}
			}
		} else {
			q.Kind = event.QuestionReply
			if a.def != "" {
				q.Default = &event.Answer{Text: a.def, Expired: true}
			}
		}
		q.ID = saigetypes.InterruptID(p.scope, nil, q.Kind, tc.ID)
		if q.Default != nil {
			q.Default.QuestionID = q.ID
		}
		return q, true
	}
	if a, t, ok := approvingOf(p.tools, tc.Name); ok {
		if req, ok := a.Approval(tc); ok {
			q.Kind = event.QuestionApproval
			q.Prompt, q.AskedOf = clipText(req.Prompt), clipText(req.AskedOf)
			q.Approvers = cleanApprovers(req.Approvers)
			q.SelfApproval = req.SelfApproval && len(q.Approvers) > 0
			if req.ExpiresIn > 0 {
				q.ExpiresAt = now.Add(min(req.ExpiresIn, MaxApprovalWait))
			}
			return p.approval(q, tc, t), true
		}
	}
	t, ok := p.tools.Get(tc.Name)
	if !ok {
		return event.Question{}, false
	}
	marked, ok := t.(*saigetypes.MarkedTool)
	if !ok || len(marked.Markers) == 0 {
		return event.Question{}, false
	}
	q.Kind = event.QuestionApproval
	for _, m := range marked.Markers {
		if q.Prompt == "" {
			q.Prompt = clipText(m.Message)
		}
		if who, ok := m.Meta["asked_of"].(string); ok && q.AskedOf == "" {
			q.AskedOf = clipText(who)
		}
	}
	return p.approval(q, tc, t), true
}

// approval completes an approval question about tc: the default prompt, the
// call, its id, and a denial as the default on expiry.
func (p *parker) approval(q event.Question, tc saigetypes.ToolCallPart, t saigetypes.Tool) event.Question {
	if q.Prompt == "" {
		q.Prompt = fmt.Sprintf("Allow %s to call %s?", p.agent, tc.Name)
	}
	args, _ := json.Marshal(tc.Arguments)
	schema, _ := json.Marshal(t.Definition().Parameters)
	q.Tool = &event.QuestionTool{Name: tc.Name, CallID: tc.ID, Arguments: args, Schema: schema, ActsForPerson: actsForPerson(t)}
	q.ID = saigetypes.InterruptID(p.scope, nil, q.Kind, tc.ID)
	q.Default = &event.Answer{QuestionID: q.ID, Decision: event.DecisionDeny, Expired: true,
		Note: "nobody answered before the question expired"}
	return q
}

// holdKey is where the answer to a host's hold is kept among a turn's
// answers. No tool call id has its shape.
func holdKey(id string) string { return "hold:" + id }

// hold is the question a host's hold puts to a person: approve and the batch
// runs, deny or no answer in time and the run is stopped.
func (p *parker) hold(h Hold) event.Question {
	now := p.now().UTC()
	q := event.Question{
		Kind: event.QuestionHold, Agent: p.agent, At: now, ExpiresAt: now.Add(p.ttl),
		Prompt: clipText(h.Prompt), Approvers: cleanApprovers(h.Approvers),
		Hold: &event.Hold{ID: clipText(h.ID), Topic: clipText(h.Topic), Data: h.Data},
	}
	if q.Prompt == "" {
		q.Prompt = fmt.Sprintf("May %s go on?", p.agent)
	}
	if h.ExpiresIn > 0 {
		q.ExpiresAt = now.Add(min(h.ExpiresIn, MaxApprovalWait))
	}
	q.ID = saigetypes.InterruptID(p.scope, nil, q.Kind, holdKey(h.ID))
	q.Default = &event.Answer{QuestionID: q.ID, Decision: event.DecisionDeny, Expired: true,
		Note: "nobody answered before the question expired"}
	return q
}

// await reports the wait a call to an Awaiting tool parks on.
func (p *parker) await(tc saigetypes.ToolCallPart) (event.Question, bool) {
	t, ok := p.tools.Get(tc.Name)
	if !ok {
		return event.Question{}, false
	}
	if marked, ok := t.(*saigetypes.MarkedTool); ok {
		t = marked.Inner
	}
	aw, ok := t.(Awaiting)
	if !ok {
		return event.Question{}, false
	}
	q, ok := aw.AwaitQuestion(tc)
	if !ok {
		return event.Question{}, false
	}
	q.Kind = event.QuestionDelegation
	q.Agent, q.At = p.agent, p.now().UTC()
	q.ExpiresAt, q.Default = time.Time{}, nil
	q.ID = saigetypes.InterruptID(p.scope, nil, q.Kind, tc.ID)
	return q, true
}

func (p *parker) isParking() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parking
}

// holds reports whether a tool call is in the batch the turn parked on.
func (p *parker) holds(callID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parking && p.batch[callID]
}

// answer returns the answer a call was continued with.
func (p *parker) answer(callID string) (event.Answer, bool) {
	a, ok := p.answers[callID]
	return a, ok
}

// MarkerApproval is the kind of the saige marker a tool listed in a spec's
// Approvals carries.
const MarkerApproval = "human_approval"

// decide is what saige is told about a marked call: the answer the turn was
// continued with, or a refusal.
func (p *parker) decide(callID string) saige.Resolution {
	if a, ok := p.answer(callID); ok {
		if r, err := resolution(a); err == nil {
			return r
		}
		return saige.Resolution{Message: "the approved arguments could not be read"}
	}
	if p.holds(callID) {
		return saige.Resolution{Message: parkedReason}
	}
	return saige.Resolution{Message: "no person can approve this call here; it was not run"}
}

// resolution is what saige is told about a marked call that has an answer.
func resolution(a event.Answer) (saige.Resolution, error) {
	if a.Decision != event.DecisionApprove {
		msg := strings.TrimSpace(a.Note)
		if msg == "" {
			msg = "the person declined"
		}
		return saige.Resolution{Message: msg, Approver: a.By}, nil
	}
	r := saige.Resolution{Approved: true, Message: a.Note, Approver: a.By}
	if len(a.Arguments) > 0 {
		// Numbers stay json.Number, as saige reads tool arguments from a
		// record: an integer above 2^53 must not come back rounded.
		var args map[string]any
		dec := json.NewDecoder(bytes.NewReader(a.Arguments))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return saige.Resolution{}, fmt.Errorf("edited arguments: %w", err)
		}
		r.ModifiedArgs = args
	}
	return r, nil
}

// parked builds the turn's Parked from the conversation saige ended with.
// base is how many messages of it, after the system prompt, the turn was
// given rather than produced; whole keeps them.
func (p *parker) parked(branch []saigetypes.Message, base int, whole bool) (*Parked, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.parking || p.pending == nil || len(p.waits) == 0 {
		return nil, errors.New("the turn did not park")
	}
	// The branch is: the system prompt, what the turn was given, what it
	// produced, the pending message, and the errors saige recorded for the
	// batch it never ran. The transcript is the part between.
	end := -1
	for i := len(branch) - 1; i >= 0; i-- {
		if am, ok := branch[i].(saigetypes.AssistantMessage); ok && sameCalls(am, *p.pending) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New("the message the turn parked on is not in its conversation")
	}
	start := 0
	if len(branch) > 0 {
		if _, ok := branch[0].(saigetypes.SystemMessage); ok {
			start = 1
		}
	}
	if !whole {
		start += base
	}
	if start > end {
		return nil, errors.New("the turn's conversation is shorter than what it was given")
	}
	out := &Parked{Question: p.waits[0].Question, CallID: p.waits[0].CallID, Whole: whole}
	if len(p.waits) > 1 {
		out.Batch = append([]Wait(nil), p.waits...)
	}
	for _, m := range branch[start:end] {
		raw, err := EncodeMessage(m)
		if err != nil {
			return nil, err
		}
		out.Transcript = append(out.Transcript, raw)
	}
	pending, err := EncodeMessage(*p.pending)
	if err != nil {
		return nil, err
	}
	out.Pending = pending
	if len(p.answers) > 0 {
		out.Answers = make(map[string]event.Answer, len(p.answers))
		for k, v := range p.answers {
			out.Answers[k] = v
		}
	}
	return out, nil
}

// sameCalls reports whether two assistant messages ask for the same tool
// calls, which is how the pending message is found again in the branch.
func sameCalls(a, b saigetypes.AssistantMessage) bool {
	ids := func(m saigetypes.AssistantMessage) []string {
		var out []string
		for _, p := range m.Parts {
			if tc, ok := p.(saigetypes.ToolCallPart); ok {
				out = append(out, tc.ID)
			}
		}
		return out
	}
	x, y := ids(a), ids(b)
	if len(x) == 0 || len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// wireMessage is a saige message in a form that survives JSON: its role and
// its parts in saige's own part codec.
type wireMessage struct {
	Role  saigetypes.Role   `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

// EncodeMessage writes a saige message for a parked turn's record. Media a
// part carries inline is kept: the continuation sends it again.
func EncodeMessage(m saigetypes.Message) (json.RawMessage, error) {
	w := wireMessage{Role: m.Role()}
	for _, part := range saigetypes.PartsOf(m) {
		raw, err := saigetypes.MarshalPartInline(part)
		if err != nil {
			return nil, fmt.Errorf("native: encode %s message: %w", m.Role(), err)
		}
		w.Parts = append(w.Parts, raw)
	}
	return json.Marshal(w)
}

// DecodeMessage reads a message EncodeMessage wrote.
func DecodeMessage(raw json.RawMessage) (saigetypes.Message, error) {
	var w wireMessage
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("native: decode message: %w", err)
	}
	switch w.Role {
	case saigetypes.RoleSystem:
		var m saigetypes.SystemMessage
		for _, raw := range w.Parts {
			p, err := saigetypes.UnmarshalRolePart[saigetypes.SystemPart](raw)
			if err != nil {
				return nil, fmt.Errorf("native: decode system message: %w", err)
			}
			m.Parts = append(m.Parts, p)
		}
		return m, nil
	case saigetypes.RoleUser:
		var m saigetypes.UserMessage
		for _, raw := range w.Parts {
			p, err := saigetypes.UnmarshalRolePart[saigetypes.UserPart](raw)
			if err != nil {
				return nil, fmt.Errorf("native: decode user message: %w", err)
			}
			m.Parts = append(m.Parts, p)
		}
		return m, nil
	case saigetypes.RoleAssistant:
		var m saigetypes.AssistantMessage
		for _, raw := range w.Parts {
			p, err := saigetypes.UnmarshalRolePart[saigetypes.AssistantPart](raw)
			if err != nil {
				return nil, fmt.Errorf("native: decode assistant message: %w", err)
			}
			m.Parts = append(m.Parts, p)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%w: message role %q", ErrInvalid, w.Role)
	}
}

// replayFirst answers the first model call of a continuation with the message
// the parked turn ended on, and sends every later call to the real provider.
// It embeds saige's decorator base, so every optional provider interface
// (capabilities, options, structured output) is the real provider's.
type replayFirst struct {
	wrapper.Base
	msg  saigetypes.AssistantMessage
	mu   sync.Mutex
	used bool
}

func newReplayFirst(inner saigetypes.Provider, msg saigetypes.AssistantMessage) *replayFirst {
	r := &replayFirst{msg: msg}
	r.Base = wrapper.NewBase(inner, nil)
	return r
}

func (r *replayFirst) Stream(ctx context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	r.mu.Lock()
	first := !r.used
	r.used = true
	r.mu.Unlock()
	if !first {
		// Every later call is a provider request, checked as the base checks
		// one: a response schema the provider cannot enforce is refused, not
		// dropped.
		return r.Base.Stream(ctx, req)
	}
	deltas := saigetypes.MessageDeltas(r.msg)
	// This is a durable replay, not a provider request. Its zero receipt releases
	// saige's reservation without consuming the continuation's live allowance.
	deltas = append(deltas, saigetypes.UsageDelta{CacheHit: true})
	ch := make(chan saigetypes.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// replaying drops what the continuation's first, recorded, call streams: the
// parked turn published those deltas and counted that call already. It ends
// at the call's usage report, or at the first delta of anything else once the
// message's last part has been seen.
type replaying struct {
	active   bool
	lastPart int
	seenLast bool
}

func newReplaying(msg *saigetypes.AssistantMessage) *replaying {
	if msg == nil {
		return &replaying{}
	}
	return &replaying{active: true, lastPart: len(msg.Parts) - 1}
}

// drop reports whether d belongs to the recorded call.
func (r *replaying) drop(d saigetypes.Delta) bool {
	if !r.active {
		return false
	}
	switch v := d.(type) {
	case saigetypes.PartStart, saigetypes.PartDelta:
		return true
	case saigetypes.PartEnd:
		if v.Index >= r.lastPart {
			r.seenLast = true
		}
		return true
	case saigetypes.UsageDelta:
		if r.seenLast {
			r.active = false
			return true
		}
		return false
	default:
		if r.seenLast {
			r.active = false
		}
		return false
	}
}

// batchDelta reports the tool call a delta is about, for the deltas of a
// batch the turn parked on, which never ran and are not published.
func batchDelta(d saigetypes.Delta) (string, bool) {
	switch v := d.(type) {
	case saigetypes.ToolExecStartDelta:
		return v.ToolCallID, true
	case saigetypes.ToolExecEndDelta:
		return v.ToolCallID, true
	case saigetypes.ToolExecDelta:
		return v.ToolCallID, true
	}
	return "", false
}
