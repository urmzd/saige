package native

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// Runtime is the native turn.Runtime: it runs each turn with a Runner built
// for the pinned definition. The zero value runs with the default options.
type Runtime struct {
	// Options are passed to every Runner it builds.
	Options []Option
	// Knowledge, when set, grounds a turn in the sources its definition
	// names, scoped to the turn's tenant. It may return nil: that turn
	// retrieves nothing.
	Knowledge func(tenant string, sources []string) Retriever
}

var _ turn.Runtime = (*Runtime)(nil)

// Turn runs one turn of def for in.
//
// The request is the catalog rendering of in.Trigger, and the retrieval
// query is the same text. A trigger whose type is not in the catalog, or
// whose body is not of that type, is refused: there is nothing to render.
func (rt *Runtime) Turn(ctx context.Context, def turn.Pinned, in turn.Input, emit func(context.Context, event.Body) error) (turn.Result, error) {
	rev, err := RevisionOf(def)
	if err != nil {
		return turn.Result{}, err
	}
	text, err := Render(in.Trigger)
	if err != nil {
		return turn.Result{}, err
	}
	opts := rt.Options
	if c := CompactionOf(in.Compaction); c != nil {
		// Last, so the host's budget for this turn wins over the runtime's.
		opts = append(append([]Option(nil), opts...), WithCompaction(*c))
	}
	if rt.Knowledge != nil {
		names := make([]string, 0, len(def.Spec.Sources))
		for _, src := range def.Spec.Sources {
			names = append(names, src.Name)
		}
		if r := rt.Knowledge(in.Tenant, names); r != nil {
			opts = append(append([]Option(nil), opts...), WithRetriever(r))
		}
	}
	runner, err := New(rev, opts...)
	if err != nil {
		return turn.Result{}, err
	}
	t := Turn{
		Instructions: in.Instructions,
		Budget:       in.Budget,
		Stop:         turn.StopSignal(ctx),
		Compact:      turn.CompactSignal(ctx),
		Holds:        turn.HoldsFrom(ctx),
		Tenant:       in.Tenant,
		History:      in.History,
		Text:         text,
		Accounting:   in.Accounting,
		Park:         in.Park,
		QuestionTTL:  in.QuestionTTL,
	}
	if req, ok := event.RequestOf(in.Trigger.Data); ok {
		t.Attachments = req.Attachments
	}
	if in.Resume != nil {
		p, err := ParkedOf(*in.Resume)
		if err != nil {
			return turn.Result{}, err
		}
		t.Resume = &p
	}
	res, err := runner.Run(ctx, t, emit)
	out, cerr := ResultOf(res)
	if err != nil {
		return out, err
	}
	return out, cerr
}

// CompactionTo is a budget as a turn.Compaction, for a turn.Input. Nil stays
// nil: the runtime's own budget applies.
func CompactionTo(c *Compaction) *turn.Compaction {
	if c == nil {
		return nil
	}
	return &turn.Compaction{MaxInputTokens: c.MaxInputTokens, KeepTurns: c.KeepTurns, OnRequest: c.OnRequest}
}

// CompactionOf reads back a turn.Compaction. Nil stays nil.
func CompactionOf(c *turn.Compaction) *Compaction {
	if c == nil {
		return nil
	}
	return &Compaction{MaxInputTokens: c.MaxInputTokens, KeepTurns: c.KeepTurns, OnRequest: c.OnRequest}
}

// Render is the request text of a trigger: its catalog rendering.
func Render(trigger event.Envelope) (string, error) {
	entry, ok := event.Lookup(trigger.Type)
	if !ok {
		return "", fmt.Errorf("%w: trigger type %q is not in the catalog", ErrInvalid, trigger.Type)
	}
	if trigger.Data == nil || trigger.Data.Type() != trigger.Type {
		return "", fmt.Errorf("%w: trigger of type %q carries no body of that type", ErrInvalid, trigger.Type)
	}
	return entry.Render(trigger)
}

// RevisionOf is the Revision a pinned definition runs as.
func RevisionOf(def turn.Pinned) (Revision, error) {
	return def, validate(def)
}

// ResultOf is a Runner's result as a turn.Result.
func ResultOf(res Result) (turn.Result, error) {
	out := turn.Result{
		Text:      res.Text,
		Prompt:    res.Prompt,
		Citations: res.Citations,
		Stopped:   res.Stopped,
	}
	if err := recode(res.Usage, &out.Usage); err != nil {
		return out, err
	}
	if len(res.Compactions) > 0 {
		if err := recode(res.Compactions, &out.Compactions); err != nil {
			return out, err
		}
	}
	if res.Parked != nil {
		p, err := ParkedTo(*res.Parked)
		if err != nil {
			return out, err
		}
		out.Parked = &p
	}
	return out, nil
}

// ParkedTo is a parked turn as a turn.Parked: the question and prompt for
// the host, and the whole record as State.
func ParkedTo(p Parked) (turn.Parked, error) {
	var out turn.Parked
	state, err := json.Marshal(p)
	if err != nil {
		return out, fmt.Errorf("native: encode parked turn: %w", err)
	}
	out.Prompt = p.Prompt
	out.State = state
	if err := recode(p.Question, &out.Question); err != nil {
		return out, err
	}
	for _, w := range p.Batch {
		out.Questions = append(out.Questions, w.Question)
	}
	if len(p.Answers) > 0 {
		if err := recode(p.Answers, &out.Answers); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ParkedOf reads back a turn.Parked this package made, with the answers the
// host filled in. A record with no state was not made here and is refused.
func ParkedOf(p turn.Parked) (Parked, error) {
	var out Parked
	if len(p.State) == 0 {
		return out, fmt.Errorf("%w: the parked turn carries no native state", ErrInvalid)
	}
	if err := json.Unmarshal(p.State, &out); err != nil {
		return out, fmt.Errorf("%w: decode parked turn: %v", ErrInvalid, err)
	}
	out.Answers = nil
	if len(p.Answers) > 0 {
		if err := recode(p.Answers, &out.Answers); err != nil {
			return out, err
		}
	}
	return out, nil
}

// recode copies v into out through JSON: the payload and event types it is
// used between share one wire shape.
func recode(v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("native: convert %T: %w", v, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("native: convert %T to %T: %w", v, out, err)
	}
	return nil
}
