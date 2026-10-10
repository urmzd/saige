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
func probes(c *Catalog, provider types.ProviderName) []string {
	var out []string
	for _, k := range sortedKeys(c.Models) {
		v, prefix, _ := splitModelKey(k)
		if v == provider {
			m := string(prefix)
			out = append(out, m, m+"-20990101", m+":latest", "hf.co/someone/"+m)
		}
	}
	out = append(out, "unknown-model-xyz")
	sort.Strings(out)
	return out
}

func lookupRecords(c *Catalog) []goldenLookup {
	providers := map[types.ProviderName]bool{}
	for k := range c.Models {
		v, _, _ := splitModelKey(k)
		providers[v] = true
	}
	for _, ep := range c.Endpoints {
		if ep.DefaultOfferingTemplate != "" {
			for _, v := range ep.Serves {
				providers[v] = true
			}
		}
	}
	var out []goldenLookup
	for _, p := range sortedKeys(providers) {
		for _, m := range probes(c, p) {
			caps, known := c.Lookup(p, types.ModelID(m))
			r := goldenLookup{Provider: string(p), Model: m, Known: known, Caps: caps}
			if e, ok := c.Describe(p, types.ModelID(m)); ok {
				r.Described = true
				if string(e.Prefix) == m {
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

// TestUpgradeV1Golden is the migration's safety net: the last version 1
// catalog, upgraded, resolves every family and every probe exactly as the
// version 1 reader did, recorded in v1/lookup_golden.json before the
// format changed.
func TestUpgradeV1Golden(t *testing.T) {
	c, err := LoadFile(filepath.Join("testdata", "v1", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	got := jsonLines(t, lookupRecords(c))
	want, err := os.ReadFile(filepath.Join("testdata", "v1", "lookup_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		gl, wl := bytes.Split(got, []byte("\n")), bytes.Split(want, []byte("\n"))
		for i := range min(len(gl), len(wl)) {
			if !bytes.Equal(gl[i], wl[i]) {
				t.Fatalf("line %d differs\n got: %s\nwant: %s", i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("got %d lines, want %d", len(gl), len(wl))
	}
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
