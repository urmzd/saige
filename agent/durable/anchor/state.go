package anchor

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/agent/types"
)

// StepState is one step of a run as its records show it.
type StepState struct {
	// Name is the step's name, segment prefix included ("input-1/llm-main-0").
	Name string
	// Done says the step has a recorded result, Result. A step that is
	// neither done nor Uncertain started on an attempt that has not ended.
	Done   bool
	Result *types.StepResult
	// Uncertain says the step may have taken effect without a recorded
	// result; Error says why, and Result holds the conservative budget
	// receipt saved before dispatch, when there is one.
	Uncertain bool
	Error     string
	StartedAt time.Time
	// CompletedAt is when the result or the uncertainty was recorded.
	CompletedAt time.Time
}

// InterruptState is one interrupt of a run and its reply, if any.
type InterruptState struct {
	Interrupt types.Interrupt
	// Request is the approval request, for approvals raised through
	// ResolveApproval.
	Request *types.ApprovalRequest
	// Reply is the recorded reply; Expired says it was the denial recorded
	// when the interrupt expired.
	Reply     *types.InterruptReply
	RepliedAt time.Time
	Expired   bool
}

// Input is one segment of a run's input log: the first input (Sequence 0),
// then each appended segment.
type Input struct {
	Sequence int
	Key      string
	At       time.Time
	Messages []types.Message
}

// Event is one entry of a run's history, rendered from its records.
type Event struct {
	At   time.Time
	Kind string // run.started, step.completed, approval.requested, ...
	ID   string // the step name, interrupt ID or segment number
}

// detail fills the parts of s that need every record: steps, interrupts,
// input segments, the latest answer, and whether an open run is idle.
func detail(s *State, r run.Run, records []run.Record, open bool) {
	byKey := make(map[string]run.Record, len(records))
	for _, rec := range records {
		byKey[rec.Key] = rec
	}
	s.CreatedAt, s.CompletedAt = r.CreatedAt, r.CompletedAt
	s.Cancelled = run.Cancelled(r)

	steps := map[string]*StepState{}
	step := func(name string) *StepState {
		st, ok := steps[name]
		if !ok {
			st = &StepState{Name: name}
			steps[name] = st
		}
		return st
	}
	for _, rec := range records {
		if rec.Kind != run.KindStep {
			continue
		}
		if !topLevel(rec) {
			// A started round: the dispatch record under the step's key.
			if strings.HasSuffix(rec.Key, "/"+dispatchName+"#0") {
				parent := strings.TrimSuffix(rec.Key, "/"+dispatchName+"#0")
				if _, released := byKey[parent+"/"+releasedName+"#0"]; released {
					continue // the round ended before dispatch
				}
				if i := strings.LastIndexByte(parent, '#'); i > 0 && strings.HasPrefix(parent, stepPrefix) && !strings.Contains(parent[:i], "#") {
					st := step(strings.TrimPrefix(parent[:i], stepPrefix))
					if st.StartedAt.IsZero() || rec.RecordedAt.Before(st.StartedAt) {
						st.StartedAt = rec.RecordedAt
					}
				}
			}
			continue
		}
		if !strings.HasPrefix(rec.Name, stepPrefix) {
			continue // uncertainty markers are read with their reconciliations below
		}
		st := step(strings.TrimPrefix(rec.Name, stepPrefix))
		var raw []byte
		var res types.StepResult
		if json.Unmarshal(rec.Output, &raw) == nil && decode(raw, &res) == nil {
			st.Done, st.Result, st.Uncertain, st.Error = true, &res, false, ""
			st.CompletedAt = rec.RecordedAt
		}
	}
	for _, u := range uncertainSteps(records, byKey) {
		st := step(u.Step)
		if st.Done {
			continue
		}
		st.Uncertain, st.Error = true, u.Error
		if u.Receipt != nil {
			st.Result = &types.StepResult{Receipt: u.Receipt}
		}
		if rec, ok := byKey[strings.Replace(u.eventKey, "event:"+reconcilePrefix, uncertainPrefix, 1)]; ok {
			st.CompletedAt = rec.RecordedAt
		}
	}
	names := make([]string, 0, len(steps))
	for name := range steps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s.Steps = append(s.Steps, *steps[name])
	}

	if all, byKey, err := interrupts(records); err == nil {
		for id, f := range all {
			is := InterruptState{Interrupt: asInterrupt(id, f.posted), Request: f.posted.Request}
			if rec, ok := byKey[f.replyKey]; ok {
				if reply, err := replyFrom(id, rec.Output); err == nil {
					is.Reply, is.RepliedAt, is.Expired = &reply, rec.RecordedAt, reply.IdempotencyKey == expiredReplyKey
				}
			}
			s.Interrupts = append(s.Interrupts, is)
		}
		sort.Slice(s.Interrupts, func(i, j int) bool {
			a, b := s.Interrupts[i].Interrupt, s.Interrupts[j].Interrupt
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
			return a.ID < b.ID
		})
	}

	if segs, err := segments(r, records); err == nil {
		for _, seg := range segs {
			s.Inputs = append(s.Inputs, Input{Sequence: seg.seq, Key: seg.key, At: seg.at, Messages: seg.messages})
		}
	}

	// The latest answer: the run's output, or the last segment answered.
	if r.Status == run.StatusSucceeded {
		if msg, err := finalMessage(r.Output); err == nil {
			s.Final = msg
		}
	}
	answered := -1
	for _, rec := range records {
		if rec.Kind != run.KindStep || !strings.HasPrefix(rec.Name, finalPrefix) {
			continue
		}
		k, err := strconv.Atoi(strings.TrimPrefix(rec.Name, finalPrefix))
		if err != nil || k < answered {
			continue
		}
		answered = k
		s.Final = nil
		var raw []byte
		if json.Unmarshal(rec.Output, &raw) == nil && len(raw) > 0 {
			var msg types.AssistantMessage
			if decode(raw, &msg) == nil {
				s.Final = &msg
			}
		}
	}
	s.Idle = open && !r.Status.Terminal() && len(s.Pending) == 0 && len(s.Uncertain) == 0 &&
		answered >= 0 && answered == len(s.Inputs)-1
	s.History = Events(r, records)
}

// topLevel reports whether rec sits at the top of the workflow, under no
// parent key.
func topLevel(rec run.Record) bool {
	i := strings.LastIndexByte(rec.Key, '#')
	if i < 0 || rec.Key[:i] != rec.Name {
		return false
	}
	_, err := strconv.Atoi(rec.Key[i+1:])
	return err == nil
}

// Events renders a run's records as a readable history, oldest first: when
// the run was accepted and finished, each step result and uncertainty,
// each interrupt and its reply, each appended input and reconciliation.
func Events(r run.Run, records []run.Record) []Event {
	out := []Event{{At: r.CreatedAt, Kind: "run.accepted", ID: r.ID}}
	for _, rec := range records {
		var ev Event
		switch {
		case rec.Kind == run.KindStep && strings.HasPrefix(rec.Name, stepPrefix):
			if !topLevel(rec) {
				continue
			}
			ev = Event{Kind: "step.completed", ID: strings.TrimPrefix(rec.Name, stepPrefix)}
		case rec.Kind == run.KindStep && strings.HasPrefix(rec.Name, uncertainPrefix):
			ev = Event{Kind: "step.uncertain", ID: strings.TrimPrefix(rec.Name, uncertainPrefix)}
		case rec.Kind == run.KindStep && strings.HasPrefix(rec.Name, interruptPrefix):
			kind := "interrupt.posted"
			if p, err := decodePosted(rec.Output); err == nil && p.Request != nil {
				kind = "approval.requested"
			}
			ev = Event{Kind: kind, ID: strings.TrimPrefix(rec.Name, interruptPrefix)}
		case rec.Kind == run.KindEvent && strings.HasPrefix(rec.Name, replyPrefix):
			kind := "approval.decided"
			var p replyPayload
			if json.Unmarshal(rec.Output, &p) == nil && p.Key == expiredReplyKey {
				kind = "interrupt.expired"
			}
			ev = Event{Kind: kind, ID: strings.TrimPrefix(rec.Name, replyPrefix)}
		case rec.Kind == run.KindEvent && strings.HasPrefix(rec.Name, reconcilePrefix):
			ev = Event{Kind: "step.reconciled", ID: strings.TrimPrefix(rec.Name, reconcilePrefix)}
		case rec.Kind == run.KindEvent && strings.HasPrefix(rec.Name, inputEvent):
			ev = Event{Kind: "input.appended", ID: strings.TrimPrefix(rec.Name, inputEvent)}
		default:
			continue
		}
		ev.At = rec.RecordedAt
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	switch {
	case run.Cancelled(r):
		out = append(out, Event{At: r.CompletedAt, Kind: "run.cancelled", ID: r.ID})
	case r.Status == run.StatusSucceeded:
		out = append(out, Event{At: r.CompletedAt, Kind: "run.completed", ID: r.ID})
	case r.Status == run.StatusFailed:
		out = append(out, Event{At: r.CompletedAt, Kind: "run.failed", ID: r.ID})
	}
	return out
}
