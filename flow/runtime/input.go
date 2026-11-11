package runtime

import (
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/event"
)

// runID is the run this input belongs to.
func (in Input) runID() string {
	if in.Run != "" {
		return in.Run
	}
	return RunID(in.Trigger.Tenant, in.Trigger.ID)
}

// request is what the trigger asks: its request, empty for a body that asks
// nothing.
func (in Input) request() event.Request {
	r, _ := event.RequestOf(in.Trigger.Data)
	return r
}

// Actor is who the run acts for: the actor on the trigger's request.
func (in Input) Actor() event.Actor { return in.request().Actor }

// surface is the address the trigger came from, and false for a trigger with
// no surface (its source is not an address), such as a schedule's tick.
func (in Input) surface() (address.Address, bool) {
	a, err := address.Parse(in.Trigger.Source)
	if err != nil {
		return address.Address{}, false
	}
	return a, true
}

// Origin is the surface the trigger came through: the scheme of its source,
// such as "slack", "github", "cron" or "delegation".
func (in Input) Origin() string {
	if a, ok := in.surface(); ok {
		return a.Connector
	}
	scheme, _, _ := strings.Cut(in.Trigger.Source, ":")
	return scheme
}

// prompt is what the trigger asks of the model: its request text and the
// trigger context rendered next to it (event.Prompt).
func (in Input) prompt() (string, map[string]string) {
	text, context, _ := event.Prompt(in.Trigger.Data)
	return text, context
}

// rendered is the trigger as the catalog renders it for a model, and "" when
// it cannot be rendered.
func (in Input) rendered() string {
	s, err := event.Render(in.Trigger)
	if err != nil {
		return ""
	}
	return s
}
