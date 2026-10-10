package batch

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestManifestAndWireIDs(t *testing.T) {
	a, err := Manifest("p", "m", requests("x", "y"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Manifest("p", "m", requests("x", "y"))
	c, _ := Manifest("p", "other", requests("x", "y"))
	reqs := requests("x", "y")
	reqs[1].Messages = []types.Message{types.NewSystemMessage("q-y")}
	d, _ := Manifest("p", "m", reqs)
	if a != b || a == c || a == d {
		t.Fatalf("manifests: %s %s %s %s", a, b, c, d)
	}
	pfx := wirePrefix("job", a)
	if i, ok := wireIndex(pfx, wireID(pfx, 7), 8); !ok || i != 7 {
		t.Fatalf("wire index = %d %v", i, ok)
	}
	for _, bad := range []string{wireID(pfx, 8), pfx + "-07", "other-1", pfx + "-x"} {
		if _, ok := wireIndex(pfx, bad, 8); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}
