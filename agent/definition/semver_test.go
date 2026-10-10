package definition

import "testing"

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"1.2.3", "v0.0.1", "1.0.0-rc.1", "1.0.0-alpha.beta+build.5", "10.20.30"} {
		if _, err := ParseVersion(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-01", "a.b.c", "1.2.3+"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("%s: want an error", s)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	order := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	for i := 1; i < len(order); i++ {
		a, _ := ParseVersion(order[i-1])
		b, _ := ParseVersion(order[i])
		if a.Compare(b) >= 0 || b.Compare(a) <= 0 {
			t.Errorf("%s should sort before %s", a, b)
		}
	}
	a, _ := ParseVersion("1.0.0+a")
	b, _ := ParseVersion("1.0.0+b")
	if a.Compare(b) != 0 {
		t.Error("build metadata must not affect precedence")
	}
}

func TestRangeMatches(t *testing.T) {
	cases := []struct {
		rng  string
		yes  []string
		no   []string
		fail bool
	}{
		{rng: "", yes: []string{"0.0.1", "9.9.9"}, no: []string{"1.0.0-rc.1"}},
		{rng: "*", yes: []string{"1.2.3"}, no: []string{"1.0.0-rc.1"}},
		{rng: "1.2.3", yes: []string{"1.2.3"}, no: []string{"1.2.4"}},
		{rng: "=1.2.3", yes: []string{"1.2.3"}, no: []string{"1.2.2"}},
		{rng: "1.2", yes: []string{"1.2.0", "1.2.9"}, no: []string{"1.3.0", "1.1.9"}},
		{rng: "1.x", yes: []string{"1.0.0", "1.9.0"}, no: []string{"2.0.0", "0.9.0"}},
		{rng: "^1.2.3", yes: []string{"1.2.3", "1.9.9"}, no: []string{"1.2.2", "2.0.0", "2.0.0-rc.1"}},
		{rng: "^1.2", yes: []string{"1.2.0", "1.8.0"}, no: []string{"2.0.0"}},
		{rng: "^0.2.3", yes: []string{"0.2.3", "0.2.9"}, no: []string{"0.3.0"}},
		{rng: "^0.0.3", yes: []string{"0.0.3"}, no: []string{"0.0.4"}},
		{rng: "~1.2.3", yes: []string{"1.2.3", "1.2.9"}, no: []string{"1.3.0"}},
		{rng: "~1", yes: []string{"1.0.0", "1.9.0"}, no: []string{"2.0.0"}},
		{rng: ">=1.0.0 <2.0.0", yes: []string{"1.0.0", "1.99.0"}, no: []string{"2.0.0", "0.9.9"}},
		{rng: ">= 1.0.0", yes: []string{"3.0.0"}, no: []string{"0.1.0"}},
		{rng: ">1.2", yes: []string{"1.3.0"}, no: []string{"1.2.9"}},
		{rng: "<=1.2", yes: []string{"1.2.9"}, no: []string{"1.3.0"}},
		{rng: "1.0.0 - 1.4.0", yes: []string{"1.0.0", "1.4.0"}, no: []string{"1.4.1"}},
		{rng: "^1.0.0 || ^3.0.0", yes: []string{"1.5.0", "3.1.0"}, no: []string{"2.0.0"}},
		{rng: "^1.0.0-rc.1", yes: []string{"1.0.0-rc.2", "1.0.0", "1.5.0"}, no: []string{"1.1.0-rc.1"}},
		{rng: "1.0.0-rc.1", yes: []string{"1.0.0-rc.1"}, no: []string{"1.0.0"}},
		{rng: "^nope", fail: true},
		{rng: ">*", fail: true},
		{rng: "1.2 ||", fail: true},
	}
	for _, tc := range cases {
		r, err := ParseRange(tc.rng)
		if tc.fail {
			if err == nil {
				t.Errorf("%q: want an error", tc.rng)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.rng, err)
			continue
		}
		for _, s := range tc.yes {
			if v, _ := ParseVersion(s); !r.Matches(v) {
				t.Errorf("%q should match %s", tc.rng, s)
			}
		}
		for _, s := range tc.no {
			if v, _ := ParseVersion(s); r.Matches(v) {
				t.Errorf("%q should not match %s", tc.rng, s)
			}
		}
	}
}

func TestParseRef(t *testing.T) {
	r, err := ParseRef("reviewer@^1.2")
	if err != nil || r.Name != "reviewer" || r.Range != "^1.2" || r.String() != "reviewer@^1.2" {
		t.Fatalf("got %+v %v", r, err)
	}
	if r, err := ParseRef("reviewer"); err != nil || r.Range != "" {
		t.Fatalf("got %+v %v", r, err)
	}
	for _, bad := range []string{"", "@1.0.0", "Reviewer", "rev_iewer", "rev-", "x@^y"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
