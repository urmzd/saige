package eval

import (
	"reflect"
	"testing"
)

func TestCatalogProvenanceAndDrift(t *testing.T) {
	var base Provenance
	base.AddRoute("p/a", "p", "h1", "rev1")
	base.AddRoute("p/b", "p", "h2", "rev1")
	base.AddRoute("", "p", "h9", "rev1")
	if c := base.Catalog; c.Revision != "rev1" || !reflect.DeepEqual(c.Presets, []string{"p"}) || len(c.Configs) != 2 {
		t.Fatalf("catalog %+v", c)
	}
	exp := cloneProvenance(base)
	exp.Catalog.Configs["p/b"] = "h3"
	if base.Catalog.Configs["p/b"] != "h2" {
		t.Fatal("clone aliases the configs")
	}
	drift := ConfigDrift(base, exp)
	if len(drift) != 1 || drift[0] != "profile p/b: configuration h2 vs h3" {
		t.Fatalf("drift %v", drift)
	}
	if ConfigDrift(base, Provenance{}) != nil {
		t.Fatal("a run without catalog provenance is not drift")
	}
	suite := &SuiteResult{Name: "s"}
	c := CompareSuites(suite, suite, WithProvenance(base, exp))
	if len(c.Warnings) != 1 {
		t.Fatalf("comparison warnings %v", c.Warnings)
	}
}

func TestDialProvenanceAndDrift(t *testing.T) {
	var base, exp Provenance
	base.AddDial("creativity", "focused", "temperature=0.3")
	base.AddDial("creativity", "focused", "temperature=0.3")
	base.AddDial("reasoning", "on:high", "effort=high")
	exp.AddDial("creativity", "focused", "dropped")
	exp.AddDial("reasoning", "on:high", "effort=high")
	exp.AddDial("reasoning", "off", "effort=low")
	if got := base.Dials["creativity"]; len(got) != 1 || len(got[0].Sent) != 1 {
		t.Fatalf("dedup: %+v", got)
	}
	drift := ConfigDrift(base, exp)
	if len(drift) != 1 || drift[0] != "dial creativity focused: sent as [temperature=0.3] vs [dropped]" {
		t.Fatalf("drift: %v", drift)
	}
}
