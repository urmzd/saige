package memstore_test

import (
	"testing"

	"github.com/urmzd/saige/agent/store/memstore"
	"github.com/urmzd/saige/agent/store/storetest"
	"github.com/urmzd/saige/agent/types"
)

// TestConformance runs the shared Store suite. A memstore holds one
// conversation, so each conversation ID maps to its own instance.
func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T, conversationID string) types.Store {
		return memstore.New()
	})
}
