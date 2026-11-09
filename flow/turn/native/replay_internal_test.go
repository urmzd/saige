package native

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
)

// A continuation replays its recorded first call, then asks the provider.
// Those later calls are checked: a schema the provider cannot enforce is
// refused, never sent without it.
func TestAContinuationRefusesASchemaItsProviderCannotEnforce(t *testing.T) {
	t.Parallel()
	inner := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("free text")}}
	r := newReplayFirst(inner, saigetypes.AssistantMessage{})
	schema := &saigetypes.ParameterSchema{Type: "object"}
	if _, err := r.Stream(t.Context(), saigetypes.Request{Schema: schema}); err != nil {
		t.Fatalf("the replayed call = %v", err)
	}
	if _, err := r.Stream(t.Context(), saigetypes.Request{Schema: schema}); !errors.Is(err, saigetypes.ErrInvalidModelConfig) {
		t.Fatalf("a later call with a schema the provider cannot enforce = %v", err)
	}
	if _, err := r.Stream(t.Context(), saigetypes.Request{}); err != nil {
		t.Fatalf("a later call without a schema = %v", err)
	}
}
