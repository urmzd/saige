package memstore_test

import (
	"testing"

	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
	"github.com/urmzd/saige/eval/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return memstore.New() })
}
