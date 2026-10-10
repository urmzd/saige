package memory_test

import (
	"testing"

	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/memory/memorytest"
)

func TestConformance(t *testing.T) {
	t.Run("mem", func(t *testing.T) {
		memorytest.RunConformance(t, func(*testing.T) memory.Store { return memory.NewMemStore() })
	})
	t.Run("file", func(t *testing.T) {
		memorytest.RunConformance(t, func(t *testing.T) memory.Store {
			fs, err := memory.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return fs
		})
	})
}
