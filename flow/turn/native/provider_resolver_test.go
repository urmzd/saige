package native_test

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/turn/native"
)

// A provider added while a deployment runs is found through the resolver,
// and a registered name never reaches it.
func TestTheResolverBuildsAProviderNoOptionRegistered(t *testing.T) {
	t.Parallel()

	rev := offlineRevision(1, "")
	rev.Spec.Provider, rev.Spec.Model = "team-anthropic", "claude-sonnet-5-5"
	var asked []string
	var built native.Spec
	resolver := func(name string) (native.ProviderFunc, bool) {
		asked = append(asked, name)
		if name != "team-anthropic" {
			return nil, false
		}
		return func(s native.Spec) (saigetypes.Provider, error) {
			built = s
			return &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("from the team key")}}, nil
		}, true
	}
	r, err := native.New(rev, native.WithProviderResolver(resolver))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, res := collect(t, r, native.Turn{Text: "hi"})
	if !strings.Contains(res.Text, "from the team key") {
		t.Fatalf("reply %q, want the resolved provider's", res.Text)
	}
	if built.Model != "claude-sonnet-5-5" || built.Provider != "team-anthropic" {
		t.Fatalf("the factory got %s/%s, want team-anthropic/claude-sonnet-5-5", built.Provider, built.Model)
	}

	// The built-in offline provider is registered, so the resolver is not asked for it.
	asked = nil
	r, err = native.New(offlineRevision(1, ""), native.WithScript(agenttest.TextResponse("offline")), native.WithProviderResolver(resolver))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	collect(t, r, native.Turn{Text: "hi"})
	if len(asked) != 0 {
		t.Fatalf("the resolver was asked for %v, want a registered name to win", asked)
	}
}

// A name the resolver does not know fails the turn as any unknown provider does.
func TestAnUnknownProviderStillFailsWithAResolver(t *testing.T) {
	t.Parallel()

	rev := offlineRevision(1, "")
	rev.Spec.Provider, rev.Spec.Model = "nowhere", "x"
	_, err := native.New(rev, native.WithProviderResolver(func(string) (native.ProviderFunc, bool) { return nil, false }))
	if err == nil || !strings.Contains(err.Error(), `"nowhere"`) {
		t.Fatalf("New returned %v, want it to name the unknown provider", err)
	}
}
