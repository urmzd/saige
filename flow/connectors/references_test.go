package connectors_test

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// The grammar and the event spell every kind alike, so a reference read on
// any surface reaches a host under the name the wire documents.
func TestReferencesKeepEveryFieldAndKindAcrossTheConversion(t *testing.T) {
	kinds := map[string]string{
		mention.KindAgent:  event.ReferenceAgent,
		mention.KindPerson: event.ReferencePerson,
		mention.KindPlace:  event.ReferencePlace,
		mention.KindLink:   event.ReferenceLink,
	}
	for read, wire := range kinds {
		if read != wire {
			t.Errorf("mention writes %q where event documents %q", read, wire)
		}
	}
	in := []mention.Reference{{Kind: mention.KindLink, Name: "n", ID: "i", Address: "github://a/b/pull/1", URL: "https://github.com/a/b/pull/1"}}
	want := []event.Reference{{Kind: event.ReferenceLink, Name: "n", ID: "i", Address: "github://a/b/pull/1", URL: "https://github.com/a/b/pull/1"}}
	if got := connectors.References(in); !reflect.DeepEqual(got, want) {
		t.Errorf("References = %+v, want %+v", got, want)
	}
	if got := connectors.References(nil); got != nil {
		t.Errorf("References(nil) = %+v, want nil so the field is left out", got)
	}
}
