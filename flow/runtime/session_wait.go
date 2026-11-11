package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/thread"
)

const sessionListenPrefix = "fiber.session.listen."
const sessionListenerPrefix = "fiber.session.listener."

type sessionWait struct {
	Session string `json:"session"`
	Run     string `json:"run,omitempty"`
	Until   string `json:"until,omitempty"`
	Since   uint64 `json:"since,omitempty"`
}
type sessionListener struct {
	Parent   string `json:"parent"`
	Question string `json:"question"`
}

//nolint:gocyclo // one pass over a session listen request and its limits
func (r *Runtime) prepareSessionListen(ctx context.Context, in Input, q event.Question) ([]sessionWait, error) {
	return durable.Step(ctx, sessionListenPrefix+q.ID, func(ctx context.Context) ([]sessionWait, error) {
		if q.Tool == nil || in.Session == nil || r.sessions == nil {
			return nil, session.ErrDenied
		}
		var args struct {
			Sessions []string `json:"sessions"`
			Until    string   `json:"until"`
			Cursors  []struct {
				Session string `json:"session"`
				Since   uint64 `json:"since"`
			} `json:"cursors"`
		}
		if err := json.Unmarshal(q.Tool.Arguments, &args); err != nil {
			return nil, durable.NonRetryable(err)
		}
		if len(args.Sessions) == 0 || len(args.Sessions) > 8 {
			return nil, durable.NonRetryable(fmt.Errorf("%w: listen names one to eight sessions", session.ErrInvalid))
		}
		if args.Until == "" {
			args.Until = "end"
		}
		if args.Until != "end" && args.Until != "message" {
			return nil, durable.NonRetryable(session.ErrInvalid)
		}
		cursors := map[string]uint64{}
		if len(args.Cursors) > 8 {
			return nil, durable.NonRetryable(session.ErrInvalid)
		}
		for _, c := range args.Cursors {
			if _, ok := cursors[c.Session]; ok {
				return nil, durable.NonRetryable(session.ErrInvalid)
			}
			cursors[c.Session] = c.Since
		}
		out := []sessionWait{}
		seen := map[string]bool{}
		for _, id := range args.Sessions {
			if seen[id] {
				continue
			}
			seen[id] = true
			target, err := r.sessionAccess(ctx, *in.Session, id)
			if err != nil {
				return nil, durable.NonRetryable(err)
			}
			// Waiting on oneself or an ancestor would deadlock the tree.
			if args.Until == "end" && target.Depth <= in.Session.Depth {
				return nil, durable.NonRetryable(session.ErrDenied)
			}
			wait := sessionWait{Session: id, Until: args.Until}
			if args.Until == "message" {
				if r.threads == nil {
					return nil, durable.NonRetryable(session.ErrDenied)
				}
				since, ok := cursors[id]
				if !ok {
					since, err = r.threads.Head(ctx, target.ThreadKey())
					if err != nil {
						return nil, err
					}
				}
				wait.Since = since
			}
			runs, err := r.sessions.Runs(ctx, in.Trigger.Tenant, id, 1)
			if err != nil {
				return nil, err
			}
			if len(runs) == 0 && args.Until == "end" {
				return nil, durable.NonRetryable(fmt.Errorf("%w: session %s has no run", session.ErrInvalid, id))
			}
			if len(runs) > 0 {
				wait.Run = runs[0]
			}
			out = append(out, wait)
			if wait.Run == "" {
				continue
			}
			listener := sessionListener{Parent: in.runID(), Question: q.ID}
			raw, _ := json.Marshal(listener)
			_, err = r.stops.Record(ctx, run.Record{RunID: runs[0], Key: sessionListenerPrefix + in.runID() + "." + q.ID, Kind: run.KindEvent, Name: sessionListenerPrefix + in.runID() + "." + q.ID, Codec: "application/json", Status: run.RecordOK, Output: raw})
			if err != nil && err != run.ErrAlreadyRecorded {
				return nil, err
			}
		}
		for id := range cursors {
			if !seen[id] {
				return nil, durable.NonRetryable(session.ErrInvalid)
			}
		}
		return out, nil
	})
}
func (r *Runtime) sessionListenEnded(ctx context.Context, in Input, q event.Question, waits []sessionWait) (*event.Answer, bool) {
	results := []map[string]any{}
	for _, w := range waits {
		target, err := r.sessionAccess(ctx, *in.Session, w.Session)
		if err != nil {
			results = append(results, map[string]any{"session": w.Session, "error": "session access denied"})
			continue
		}
		if w.Until == "message" {
			entries, err := thread.ReadWindow(ctx, r.threads, target.ThreadKey(), w.Since, session.MaxPage)
			if err != nil || len(entries) == 0 {
				return nil, false
			}
			results = append(results, map[string]any{"session": w.Session, "since": entries[len(entries)-1].Seq, "messages": entries})
			continue
		}
		rec, err := r.loadRun(ctx, w.Run)
		if err != nil || !rec.Status.Terminal() {
			return nil, false
		}
		var out Output
		result := map[string]any{"session": w.Session, "run": w.Run, "status": rec.Status}
		if rec.Status == run.StatusSucceeded {
			if json.Unmarshal(rec.Output, &out) != nil {
				return nil, false
			}
			result["text"] = out.Text
			if out.Stopped != nil {
				result["status"] = "stopped"
				result["reason"] = out.Stopped.Reason
			}
		} else {
			result["error"] = rec.Error
		}
		results = append(results, result)
	}
	raw, _ := json.Marshal(results)
	return &event.Answer{QuestionID: q.ID, Text: string(raw), At: r.now().UTC()}, true
}

func (r *Runtime) wakeSessionListeners(ctx context.Context, in Input) {
	if r.stops == nil {
		return
	}
	_, records, err := r.recordsOf(ctx, in.runID(), false, sessionListenerPrefix)
	if err != nil {
		return
	}
	for _, rec := range records {
		if !strings.HasPrefix(rec.Key, sessionListenerPrefix) {
			continue
		}
		var listener sessionListener
		if json.Unmarshal(rec.Output, &listener) == nil {
			r.wake(ctx, listener.Parent)
		}
	}
}
