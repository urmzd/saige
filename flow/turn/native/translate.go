package native

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	saige "github.com/urmzd/saige/agent"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/event"
)

// Translator converts saige's typed deltas into event bodies.
//
// Its only state is the arguments of the turn's tool calls. saige names the
// tool on every tool delta, but it does not repeat the arguments: they arrive
// once, on the PartEnd that completes the model's call, and again on the
// BeforeTool hook as the tool is about to run with them. The translator keeps
// them by call id so that the executing ToolCall and the ToolDone can carry
// them. A Translator is made per turn with NewTranslator and is safe for
// concurrent use, because hooks call it from the goroutines that run tools.
type Translator struct {
	redact ToolArguments

	mu sync.Mutex
	// asked holds the arguments the model sent for each call, and ran the
	// ones each call ran with, rendered, by scoped call id. They are kept
	// apart because a tool can start running (and an approval can have
	// edited its arguments) before its executing event is translated.
	asked map[string]rendered
	ran   map[string]rendered
	// parts maps an open tool call part to its call id and the argument
	// fragments streamed for it, by scoped part index.
	parts map[string]*openCall
}

// rendered is a call's arguments as an event carries them.
type rendered struct {
	json  json.RawMessage
	bytes int
}

type openCall struct {
	id   string
	name string
	args strings.Builder
}

// ToolArguments rewrites a tool call's arguments before any event carries
// them. It receives the tool's name and the arguments as compact JSON, and
// returns what to publish: the same bytes, a scrubbed JSON object, or nil to
// leave them out. What it returns is not checked for being JSON beyond what
// publishing needs; a result that is not valid JSON is left out.
//
// It changes only what is published. The tool runs with, and the model sees,
// the arguments it was called with.
type ToolArguments func(tool string, args json.RawMessage) json.RawMessage

// NewTranslator returns a translator for one turn. A zero Translator works
// too.
func NewTranslator() *Translator { return newTranslator(nil) }

func newTranslator(redact ToolArguments) *Translator {
	return &Translator{redact: redact}
}

// maps makes the zero value usable. The caller holds mu.
func (t *Translator) maps() {
	if t.asked == nil {
		t.asked = map[string]rendered{}
	}
	if t.ran == nil {
		t.ran = map[string]rendered{}
	}
	if t.parts == nil {
		t.parts = map[string]*openCall{}
	}
}

// Delta maps one saige delta to an event body. The second result reports
// whether this delta becomes an event at all.
//
// Most deltas do not. saige's delta set is a rendering protocol (part starts
// and ends, token usage, marker prompts, handoffs), while the catalog is a
// published contract. Rather than invent types for the rest, this package
// publishes the ones that mean something to a subscriber and drops the
// framing:
//
//	PartDelta with Text                          -> run.delta.text.v1
//	PartDelta with Thinking                      -> run.delta.thinking.v1
//	PartStart of a tool_call, ToolExecStartDelta -> run.delta.tool.call.v1
//	ToolExecEndDelta                             -> run.delta.tool.done.v1
//
// The executing delta.tool.call and the delta.tool.done carry the call's
// arguments (see event.MaxToolArguments and ToolArguments). The requested
// one does not: it is published as the model starts the call, before its
// arguments exist.
//
// Citations are the exception that is not in this list: they are derived from
// retrieval instead (see Citations). saige's CitationDelta, which reports what
// a model or tool cited, is not translated yet and is dropped with the framing.
//
// Three deltas are not translated here, for reasons worth stating:
//
//   - ErrorDelta becomes the turn's error, not an event. Failure is a run
//     lifecycle fact and pkg/runtime publishes it with the attempt and
//     terminality a subscriber needs; a bare error event mid-stream would say
//     less and could contradict the lifecycle event that follows.
//   - StoppedDelta becomes Result.Stopped for the same reason: a stopped run
//     is announced by the host as run.stopped.v1, once, with the attempt.
//   - UsageDelta is accounting, and Runner.Run publishes it as run.usage.v1
//     with what only the runner knows: the call's key, the model's price and
//     its context window. Connectors do not render that kind, so token counts
//     never reach a Slack thread.
func (t *Translator) Delta(d saigetypes.Delta) (event.Body, bool) { return t.delta("", d) }

// delta translates d, a delta of the agent whose tool call path is scope.
// Part indices are only unique within one agent's stream, so a nested agent's
// parts are kept apart from its parent's by scope.
func (t *Translator) delta(scope string, d saigetypes.Delta) (event.Body, bool) {
	switch v := d.(type) {
	case saigetypes.PartDelta:
		switch {
		case v.Text != "":
			return event.Text{Text: v.Text}, true
		case v.Thinking != "":
			return event.Thinking{Text: v.Thinking}, true
		case v.Args != "":
			t.fragment(scope, v.Index, v.Args)
			return nil, false
		default:
			return nil, false
		}

	case saigetypes.PartStart:
		if v.Kind != saigetypes.KindToolCall {
			return nil, false
		}
		t.open(scope, v.Index, v.ID, v.Name)
		return event.ToolCall{
			ID: v.ID, Name: v.Name, Phase: event.PhaseRequested,
		}, true

	case saigetypes.PartEnd:
		t.close(scope, v)
		return nil, false

	case saigetypes.ToolExecStartDelta:
		args := t.requested(callKey(scope, v.ToolCallID))
		return event.ToolCall{
			ID: v.ToolCallID, Name: v.Name, Phase: event.PhaseExecuting,
			Arguments: args.json, ArgumentsBytes: args.bytes,
		}, true

	case saigetypes.ToolExecEndDelta:
		args := t.executed(callKey(scope, v.ToolCallID))
		return event.ToolDone{
			ID:             v.ToolCallID,
			Name:           v.Name,
			Result:         v.Result,
			Error:          v.Error,
			Arguments:      args.json,
			ArgumentsBytes: args.bytes,
		}, true

	case saigetypes.ToolExecDelta:
		// A streaming tool or sub-agent nested inside a tool call. Its inner
		// deltas are the agent's work too, so they translate the same way;
		// the tool call id already told a subscriber that a tool is running.
		return t.delta(scope+"/"+v.ToolCallID, v.Inner)

	default:
		return nil, false
	}
}

// Ran records the arguments a tool call is about to run with. They are what
// the ToolDone for the call carries, because an approval may have edited the
// ones the model sent; the executing ToolCall keeps carrying the model's. A
// call that never reaches its tool (its arguments did not parse, say) is
// reported with the model's. path is the tool call ids from the root agent
// down to the agent that made the call, empty for the root's own calls, as
// saige's hooks report it (saige.HookRun.Path).
func (t *Translator) Ran(callID, tool string, args map[string]any, path ...string) {
	if callID == "" {
		return
	}
	r := t.render(tool, args)
	t.mu.Lock()
	t.maps()
	t.ran[callKey(scopeOf(path), callID)] = r
	t.mu.Unlock()
}

// scopeOf is the scope delta gives the agent at path: one "/<call id>" per
// ToolExecDelta its deltas arrive wrapped in.
func scopeOf(path []string) string {
	var b strings.Builder
	for _, id := range path {
		b.WriteString("/")
		b.WriteString(id)
	}
	return b.String()
}

// callKey keys a call's arguments by the agent that made it, since call ids
// are only unique within one agent's stream.
func callKey(scope, callID string) string { return scope + "|" + callID }

// hooks feeds the translator the arguments each tool runs with.
func (t *Translator) hooks() saige.Hooks {
	return saige.Hooks{
		Name: "fiber-tool-arguments",
		BeforeTool: func(_ context.Context, ev *saige.BeforeToolEvent) error {
			t.Ran(ev.Call.ID, ev.Call.Name, ev.Arguments, ev.Path...)
			return nil
		},
	}
}

func partKey(scope string, index int) string { return scope + "#" + strconv.Itoa(index) }

func (t *Translator) open(scope string, index int, id, name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maps()
	t.parts[partKey(scope, index)] = &openCall{id: id, name: name}
}

func (t *Translator) fragment(scope string, index int, args string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.parts[partKey(scope, index)]; c != nil {
		c.args.WriteString(args)
	}
}

// close records the arguments of a completed tool call part: the part's own,
// when the producer sent it whole, or the fragments streamed for it.
func (t *Translator) close(scope string, end saigetypes.PartEnd) {
	key := partKey(scope, end.Index)
	t.mu.Lock()
	open := t.parts[key]
	delete(t.parts, key)
	t.mu.Unlock()

	var r rendered
	var id string
	switch p := end.Part.(type) {
	case saigetypes.ToolCallPart:
		if p.ArgumentsError != "" {
			return
		}
		id = p.ID
		r = t.render(p.Name, p.Arguments)
	case nil:
		if open == nil || open.args.Len() == 0 {
			return
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(open.args.String()), &args); err != nil {
			return
		}
		id = open.id
		r = t.render(open.name, args)
	default:
		return
	}
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maps()
	t.asked[callKey(scope, id)] = r
}

// seed records the arguments of the root agent's tool calls in msg, a model
// message whose deltas the translator will not see.
func (t *Translator) seed(msg saigetypes.AssistantMessage) {
	for _, part := range msg.Parts {
		p, ok := part.(saigetypes.ToolCallPart)
		if !ok || p.ID == "" || p.ArgumentsError != "" {
			continue
		}
		r := t.render(p.Name, p.Arguments)
		t.mu.Lock()
		t.maps()
		t.asked[callKey("", p.ID)] = r
		t.mu.Unlock()
	}
}

// requested returns the arguments the model sent for a call.
func (t *Translator) requested(key string) rendered {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.asked[key]
}

// executed returns the arguments a call ran with, or the model's when it
// never ran, and forgets the call: nothing is published about it after its
// ToolDone.
func (t *Translator) executed(key string) rendered {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.ran[key]
	if !ok {
		r = t.asked[key]
	}
	delete(t.ran, key)
	delete(t.asked, key)
	return r
}

// render turns arguments into what an event carries: compact JSON with keys
// in sorted order, scrubbed by the deployment's ToolArguments, and left out
// when over event.MaxToolArguments.
func (t *Translator) render(tool string, args map[string]any) rendered {
	if len(args) == 0 {
		return rendered{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return rendered{}
	}
	if t.redact != nil {
		b = t.redact(tool, b)
		if len(bytes.TrimSpace(b)) == 0 {
			return rendered{}
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, b); err != nil {
			return rendered{}
		}
		b = buf.Bytes()
	}
	if len(b) > event.MaxToolArguments {
		return rendered{bytes: len(b)}
	}
	return rendered{json: b}
}
