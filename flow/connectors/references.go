package connectors

import (
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// References converts the references mention read into the ones an event
// carries, in order. The two types agree field for field: mention is a
// stdlib leaf and cannot name event's, and the conversion below stops
// compiling the moment they disagree.
func References(refs []mention.Reference) []event.Reference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]event.Reference, len(refs))
	for i, r := range refs {
		out[i] = event.Reference(r)
	}
	return out
}
