package catalog

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func loadTestdata(t *testing.T, name string) *Catalog {
	t.Helper()
	c, err := LoadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func canonical(t *testing.T, c *Catalog) []byte {
	t.Helper()
	out, err := c.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stripRaw drops the as-written bytes kept for merging, which differ between
// two spellings of the same catalog.
func stripRaw(c *Catalog) *Catalog {
	c = c.clone()
	for i := range c.Models {
		c.Models[i].raw = nil
	}
	for _, m := range []map[string]ModelSpec{c.Templates, c.Baselines} {
		for name, spec := range m {
			spec.raw = nil
			m[name] = spec
		}
	}
	return c
}

func TestRoundTripGolden(t *testing.T) {
	c := loadTestdata(t, "full.json")
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	got := canonical(t, c)
	checkGolden(t, "full.canonical.json", got)
	again, err := Load(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stripRaw(c), stripRaw(again)) {
		t.Fatal("reloading the canonical form changed the catalog")
	}
	if !bytes.Equal(canonical(t, again), got) {
		t.Fatal("canonical form is not a fixed point")
	}
}

func TestDefaultCatalogValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range c.PresetNames() {
		if _, err := c.Resolve(name); err != nil {
			t.Errorf("preset %s: %v", name, err)
		}
	}
	if _, ok := c.Presets[c.DefaultPreset]; !ok {
		t.Errorf("default preset %q missing", c.DefaultPreset)
	}
	checkGolden(t, "default_issues.json", jsonLines(t, c.Issues()))
	if raw, _ := os.ReadFile("data/default.json"); !bytes.Equal(raw, DefaultJSON()) {
		t.Fatal("embedded catalog differs from data/default.json")
	}
}
