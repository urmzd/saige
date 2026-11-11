package isolate

import (
	"errors"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	valid := []Spec{
		{Image: "img"},
		{Image: "img", User: "1000"},
		{Image: "img", User: "1000:1000"},
		{Image: "img", Network: Network{Proxy: "gate:3128"}},
		{Image: "img", Network: Network{Proxy: "[::1]:3128"}},
		{Image: "img", Labels: map[string]string{"team": "a"}},
		{Image: "img", Connectors: &ConnectorSpec{}},
		{Image: "img", Connectors: &ConnectorSpec{Image: "other", User: "7"}},
	}
	for _, s := range valid {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	invalid := map[string]Spec{
		"zero value":         {},
		"flag as image":      {Image: "--privileged"},
		"space in image":     {Image: "a b"},
		"root user":          {Image: "img", User: "0"},
		"root group":         {Image: "img", User: "1000:0"},
		"named user":         {Image: "img", User: "root"},
		"negative memory":    {Image: "img", Resources: Resources{MemoryBytes: -1}},
		"proxy without port": {Image: "img", Network: Network{Proxy: "gate"}},
		"proxy url":          {Image: "img", Network: Network{Proxy: "http://gate:3128"}},
		"reserved label":     {Image: "img", Labels: map[string]string{LabelPrefix + "instance": "x"}},
		"label with =":       {Image: "img", Labels: map[string]string{"a=b": "x"}},
		"connector root":     {Image: "img", Connectors: &ConnectorSpec{User: "0"}},
		"connector image":    {Image: "img", Connectors: &ConnectorSpec{Image: "-x"}},
		"connector limits":   {Image: "img", Connectors: &ConnectorSpec{Resources: Resources{PIDs: -1}}},
		"connector proxy":    {Image: "img", Connectors: &ConnectorSpec{Network: Network{Proxy: ":1"}}},
	}
	for name, s := range invalid {
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec", name, err)
		}
	}
}

func TestResourcesDefaultsAreNeverUnlimited(t *testing.T) {
	r := Resources{}.WithDefaults()
	if r.CPUMillis <= 0 || r.MemoryBytes <= 0 || r.PIDs <= 0 || r.DiskBytes <= 0 {
		t.Fatalf("defaults %+v", r)
	}
	if got := (Resources{PIDs: 9}).WithDefaults(); got.PIDs != 9 {
		t.Fatalf("an explicit limit was replaced: %+v", got)
	}
}

func TestSpecKey(t *testing.T) {
	a := Spec{Image: "img", Labels: map[string]string{"x": "1", "y": "2"}}
	b := Spec{Image: "img", Labels: map[string]string{"y": "2", "x": "1"}, Resources: Resources{}.WithDefaults()}
	if a.Key() != b.Key() {
		t.Fatal("equal specs have different keys")
	}
	for _, other := range []Spec{
		{Image: "img2", Labels: a.Labels},
		{Image: "img", Labels: a.Labels, Connectors: &ConnectorSpec{}},
	} {
		if other.Key() == a.Key() {
			t.Errorf("%+v shares a key with %+v", other, a)
		}
	}
}

func TestCommandValidate(t *testing.T) {
	if err := (Command{Argv: []string{"true"}, Dir: "a/b", Env: map[string]string{"A": "x\ny"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Command{
		"empty":           {},
		"empty argv0":     {Argv: []string{""}},
		"nul":             {Argv: []string{"a\x00b"}},
		"negative":        {Argv: []string{"true"}, Timeout: -1},
		"absolute dir":    {Argv: []string{"true"}, Dir: "/etc"},
		"escaping dir":    {Argv: []string{"true"}, Dir: "a/../.."},
		"connectors dir":  {Argv: []string{"true"}, Dir: "a", In: Connectors},
		"bad compartment": {Argv: []string{"true"}, In: 7},
		"empty env":       {Argv: []string{"true"}, Env: map[string]string{"": "x"}},
		"env with =":      {Argv: []string{"true"}, Env: map[string]string{"A=B": "x"}},
	} {
		if err := c.Validate(); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("%s: err = %v, want ErrInvalidCommand", name, err)
		}
	}
}

func TestParseUser(t *testing.T) {
	if uid, gid, err := ParseUser(""); uid != -1 || gid != -1 || err != nil {
		t.Fatalf("empty: %d %d %v", uid, gid, err)
	}
	if uid, gid, err := ParseUser("10:20"); uid != 10 || gid != 20 || err != nil {
		t.Fatalf("10:20: %d %d %v", uid, gid, err)
	}
	if uid, gid, err := ParseUser("10"); uid != 10 || gid != -1 || err != nil {
		t.Fatalf("10: %d %d %v", uid, gid, err)
	}
}
