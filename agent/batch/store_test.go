package batch_test

import (
	"testing"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/batch/batchtest"
)

func TestMemoryStore(t *testing.T) { batchtest.StoreConformance(t, batch.NewMemoryStore()) }

func TestFileStore(t *testing.T) {
	s, err := batch.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	batchtest.StoreConformance(t, s)
}
