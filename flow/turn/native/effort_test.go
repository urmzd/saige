package native_test

import (
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn/native"
)

// A spec's effort reaches the primary model's ProviderFunc and never a
// fallback's: it was chosen for the primary, and a fallback that may not
// take it runs at its own default.
func TestEffortReachesThePrimaryOnly(t *testing.T) {
	t.Parallel()

	m := newModels(map[string]saigetypes.Provider{
		"primary": &agenttest.ScriptedProvider{},
		"backup":  &agenttest.ScriptedProvider{},
	})
	rev := fallbackRevision()
	rev.Spec.Effort = "high"
	if _, err := native.New(rev, m.opts...); err != nil {
		t.Fatal(err)
	}
	if got := m.specs["primary"].Effort; got != "high" {
		t.Errorf("the primary was built with effort %q, want high", got)
	}
	if got := m.specs["backup"].Effort; got != "" {
		t.Errorf("the fallback was built with effort %q, want none", got)
	}
}
