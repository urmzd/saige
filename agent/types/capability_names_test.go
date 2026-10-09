package types_test

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// Every Capability constant must appear in KnownCapabilities, or a catalog
// row that declares it fails to load.
func TestKnownCapabilitiesCoverDeclarations(t *testing.T) {
	known := types.KnownCapabilities()
	seen := 0
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?Cap\w+\s+(?:Capability\s+)?=\s+"([a-z_]+)"`)
	for _, file := range []string{"capabilities.go", "capabilities_prefill.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			seen++
			if !slices.Contains(known, types.Capability(m[1])) {
				t.Errorf("%s: capability %q missing from KnownCapabilities", file, m[1])
			}
		}
	}
	if seen != len(known) {
		t.Errorf("found %d capability constants, KnownCapabilities lists %d", seen, len(known))
	}
	if !slices.IsSorted(known) {
		t.Error("KnownCapabilities is not sorted")
	}
}
