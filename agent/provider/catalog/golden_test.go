package catalog

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenLookup is one recorded Lookup and Describe result.
type goldenLookup struct {
	Provider  string                  `json:"provider"`
	Model     string                  `json:"model"`
	Known     bool                    `json:"known"`
	Caps      types.ModelCapabilities `json:"caps"`
	Described bool                    `json:"described"`
	Entry     *Entry                  `json:"entry,omitempty"`
}

// probes are the model names recorded for a provider: every family, plus
// dated, tagged, path-prefixed and unknown variants.
func probes(c *Catalog, provider string) []string {
	var out []string
	for _, m := range c.Models {
		if m.Provider == provider {
			out = append(out, m.Prefix, m.Prefix+"-20990101", m.Prefix+":latest", "hf.co/someone/"+m.Prefix)
		}
	}
	out = append(out, "unknown-model-xyz")
	sort.Strings(out)
	return out
}

func lookupRecords(c *Catalog) []goldenLookup {
	providers := map[string]bool{}
	for _, m := range c.Models {
		providers[m.Provider] = true
	}
	for p := range c.Baselines {
		providers[p] = true
	}
	var out []goldenLookup
	for _, p := range sortedKeys(providers) {
		for _, m := range probes(c, p) {
			caps, known := c.Lookup(p, m)
			r := goldenLookup{Provider: p, Model: m, Known: known, Caps: caps}
			if e, ok := c.Describe(p, m); ok {
				r.Described = true
				if e.Prefix == m {
					e.Caps = types.ModelCapabilities{} // recorded in Caps already
					r.Entry = &e
				}
			}
			out = append(out, r)
		}
	}
	return out
}

// checkGolden compares got with a golden file, rewriting it with -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; review the change and run go test -run %s -update", name, t.Name())
	}
}

// TestDefaultLookupGolden freezes what the embedded catalog resolves. A
// change to default.json shows up as a reviewed diff of this file.
func TestDefaultLookupGolden(t *testing.T) {
	checkGolden(t, "lookup_golden.json", jsonLines(t, lookupRecords(Default())))
}

// jsonLines writes one compact record per line, so diffs stay readable.
func jsonLines[T any](t *testing.T, records []T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		if i < len(records)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("]\n")
	return b.Bytes()
}

// writeRepoFile rewrites a file in the package directory.
func writeRepoFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
