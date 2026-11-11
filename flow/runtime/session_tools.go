package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn/native"
)

var sessionToolNames = []string{"session_call", "session_listen", "session_read", "session_status", "session_link"}

// WithSessionRuntime installs the native session verbs in a Sandbox. A function
// lets composition build the sandbox before the durable runtime that owns it.
func WithSessionRuntime(get func() *Runtime) SandboxOption {
	return func(s *Sandbox) { s.sessionRuntime = get }
}

// WithSessionTools limits the session verbs a turn with a session is offered
// to those named: session_call, session_listen, session_read, session_status
// and session_link. Without it a turn is offered every one. With no names it
// is offered none, which is what a host that calls agents through its own
// tool (WithCallTool) and reads sessions through its own wants. A verb left
// out is no tool of the turn, even when a spec names it; a name that is not
// a session verb is ignored. Sessions themselves are unaffected.
func WithSessionTools(names ...string) SandboxOption {
	return func(s *Sandbox) { s.sessionTools = append([]string{}, names...) }
}

func (r *Runtime) sessionTools(req Request, send func(event.Body) error) map[string]saigetypes.Tool {
	out := map[string]saigetypes.Tool{}
	for _, name := range sessionToolNames {
		out[name] = &sessionTool{runtime: r, request: req, name: name, send: send}
	}
	return out
}

type sessionTool struct {
	runtime *Runtime
	request Request
	name    string
	send    func(event.Body) error
}

func (t *sessionTool) Definition() saigetypes.ToolDef {
	p := map[string]saigetypes.PropertyDef{}
	required := []string{}
	switch t.name {
	case "session_call":
		p["agent"] = saigetypes.PropertyDef{Type: "string", Description: "An agent in your call policy"}
		p["text"] = saigetypes.PropertyDef{Type: "string", Description: "The request"}
		p["label"] = saigetypes.PropertyDef{Type: "string", Description: "Optional independent conversation label"}
		p["fresh"] = saigetypes.PropertyDef{Type: "boolean"}
		p["wait"] = saigetypes.PropertyDef{Type: "boolean", Description: "Wait for the answer; default true"}
		required = []string{"agent", "text"}
	case "session_listen":
		p["sessions"] = saigetypes.PropertyDef{Type: "array", Description: "Session IDs to wait for", Items: &saigetypes.PropertyDef{Type: "string"}}
		p["until"] = saigetypes.PropertyDef{Type: "string", Enum: []string{"end", "message"}, Description: "Wait for each current run to end (default), or for messages after a transcript cursor"}
		p["cursors"] = saigetypes.PropertyDef{Type: "array", Description: "For message waits, one last-seen cursor per session; omitted cursors start at its current transcript head", Items: &saigetypes.PropertyDef{Type: "object", Properties: map[string]saigetypes.PropertyDef{"session": {Type: "string"}, "since": {Type: "integer"}}, Required: []string{"session", "since"}}}
		required = []string{"sessions"}
	case "session_read":
		p["session"] = saigetypes.PropertyDef{Type: "string"}
		p["since"] = saigetypes.PropertyDef{Type: "integer"}
		p["summary"] = saigetypes.PropertyDef{Type: "boolean"}
		required = []string{"session"}
	case "session_status":
		p["parent"] = saigetypes.PropertyDef{Type: "string", Description: "Parent session, defaults to your session"}
	case "session_link":
		p["session"] = saigetypes.PropertyDef{Type: "string"}
		p["kind"] = saigetypes.PropertyDef{Type: "string", Description: "context or evidence"}
		required = []string{"session"}
	}
	return saigetypes.ToolDef{Name: t.name, Description: strings.ReplaceAll(t.name, "_", " ") + " within your session tree", Parameters: saigetypes.ParameterSchema{Type: "object", Properties: p, Required: required}}
}
func (t *sessionTool) AwaitQuestion(c saigetypes.ToolCallPart) (event.Question, bool) {
	if t.name != "session_call" && t.name != "session_listen" {
		return event.Question{}, false
	}
	if t.name == "session_call" {
		if a, _ := c.Arguments["agent"].(string); a == "" {
			return event.Question{}, false
		}
		if text, _ := c.Arguments["text"].(string); strings.TrimSpace(text) == "" {
			return event.Question{}, false
		}
	}
	raw, _ := json.Marshal(c.Arguments)
	return event.Question{Prompt: "Waiting for sessions", Tool: &event.QuestionTool{Name: c.Name, CallID: c.ID, Arguments: raw}}, true
}

//nolint:gocyclo // one case per session tool action
func (t *sessionTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if t.request.Session == nil || t.runtime.sessions == nil {
		return "", session.ErrDenied
	}
	if t.name == "session_call" || t.name == "session_listen" {
		answer, ok := native.AnswerFor(ctx)
		if !ok {
			return "", fmt.Errorf("%w: session wait was not resumed", ErrInvalid)
		}
		if answer.RunID != "" {
			if u, err := t.runtime.childUsage(t.request.Tenant, t.request.RunID)(ctx, answer.RunID); err == nil {
				_ = t.send(u)
			}
		}
		if t.name == "session_listen" {
			var results []sessionWait
			if json.Unmarshal([]byte(answer.Text), &results) == nil {
				for _, result := range results {
					if u, err := t.runtime.childUsage(t.request.Tenant, t.request.RunID)(ctx, result.Run); err == nil {
						_ = t.send(u)
					}
				}
			}
		}
		if answer.Note != "" && answer.Text == "" {
			return "", errors.New(answer.Note)
		}
		return answer.Text, nil
	}
	id, _ := args["session"].(string)
	if t.name == "session_status" {
		id, _ = args["parent"].(string)
		if id == "" {
			id = t.request.Session.ID
		}
	}
	target, err := t.runtime.sessionAccess(ctx, *t.request.Session, id)
	if err != nil {
		return "", err
	}
	var out any
	switch t.name {
	case "session_read":
		var since uint64
		if n, ok := args["since"].(float64); ok {
			if n < 0 || n > 1<<53 {
				return "", session.ErrInvalid
			}
			since = uint64(n)
		}
		entries, err := thread.ReadWindow(ctx, t.runtime.threads, target.ThreadKey(), since, session.MaxPage)
		if err != nil {
			return "", err
		}
		if id != t.request.Session.ID {
			if err := t.runtime.sessions.Link(ctx, t.request.Tenant, t.request.Session.ID, session.Link{Target: id, Kind: "context"}); err != nil {
				return "", err
			}
		}
		if summary, _ := args["summary"].(bool); summary {
			var b strings.Builder
			for _, e := range entries {
				text := []rune(e.Text)
				if len(text) > 400 {
					text = append(text[:400], '…')
				}
				fmt.Fprintf(&b, "[%d] run=%s %s: %s\n", e.Seq, e.RunID, e.Role, string(text))
			}
			return b.String(), nil
		}
		out = entries
	case "session_link":
		kind, _ := args["kind"].(string)
		if kind == "" {
			kind = "context"
		}
		if err := t.runtime.sessions.Link(ctx, t.request.Tenant, t.request.Session.ID, session.Link{Target: id, Kind: kind}); err != nil {
			return "", err
		}
		out = map[string]string{"session": id, "kind": kind}
	case "session_status":
		children, err := t.runtime.sessions.List(ctx, t.request.Tenant, session.Filter{Parent: id, Limit: session.MaxPage})
		if err != nil {
			return "", err
		}
		statuses := []map[string]any{}
		for _, c := range children {
			runs, err := t.runtime.sessions.Runs(ctx, t.request.Tenant, c.ID, 1)
			if err != nil {
				return "", err
			}
			state := "done"
			if len(runs) > 0 {
				rec, err := t.runtime.loadRun(ctx, runs[0])
				if err != nil {
					return "", err
				}
				state = string(rec.Status)
				if !rec.Status.Terminal() {
					state = "working"
					_, records, err := t.runtime.recordsOf(ctx, runs[0], false, prefixQuestion, prefixAnswer)
					if err != nil {
						return "", err
					}
					if _, ok := pendingQuestion(records); ok {
						state = "waiting"
					}
				}
			}
			statuses = append(statuses, map[string]any{"session": c.ID, "agent": c.Key.Agent, "label": c.Key.Label, "status": state})
		}
		out = statuses
	}
	raw, err := json.Marshal(out)
	return string(raw), err
}

// sessionAccess confines agent reads to the same tree. A link records context;
// creating a link alone never grants access to a different tree.
func (r *Runtime) sessionAccess(ctx context.Context, from session.Record, id string) (session.Record, error) {
	target, err := r.sessions.Get(ctx, from.Key.Tenant, id)
	if err != nil {
		return session.Record{}, err
	}
	root := func(rec session.Record) (string, error) {
		for n := 0; rec.Depth > 0; n++ {
			if n >= 32 {
				return "", session.ErrDenied
			}
			p, err := r.sessions.Get(ctx, from.Key.Tenant, rec.Key.Parent)
			if err != nil {
				return "", err
			}
			rec = p
		}
		return rec.ID, nil
	}
	a, err := root(from)
	if err != nil {
		return session.Record{}, err
	}
	b, err := root(target)
	if err != nil {
		return session.Record{}, err
	}
	if a != b {
		grants, ok := r.sessions.(session.AccessStore)
		if !ok {
			return session.Record{}, session.ErrDenied
		}
		allowed, err := grants.MayRead(ctx, from.Key.Tenant, id, from.ID)
		if err != nil {
			return session.Record{}, err
		}
		if !allowed {
			return session.Record{}, session.ErrDenied
		}
	}
	return target, nil
}
