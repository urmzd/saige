package address_test

import (
	"errors"
	"testing"

	"github.com/urmzd/saige/flow/address"
)

func TestParseRoundTrip(t *testing.T) {
	for _, in := range []string{
		"github://urmzd/fiber/issues/42",
		"slack://T0123/C0456",
		"jira://acme/PROJ-5",
		"cron://acme/nightly-review",
		"webhook://acme/deploys",
		"slack://T0123",
	} {
		a, err := address.Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if a.String() != in {
			t.Fatalf("Parse(%q).String() = %q", in, a.String())
		}
	}
}

func TestParseFields(t *testing.T) {
	a, err := address.Parse("github://urmzd/fiber/issues/42")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Connector != "github" {
		t.Errorf("Connector = %q", a.Connector)
	}
	if a.Workspace != "urmzd" {
		t.Errorf("Workspace = %q", a.Workspace)
	}
	if got := a.Resource(); got != "fiber/issues/42" {
		t.Errorf("Resource() = %q", got)
	}
}

func TestParseParams(t *testing.T) {
	a, err := address.Parse("slack://T0123/C0456?thread=1699123456.001")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, ok := a.Param("thread")
	if !ok || got != "1699123456.001" {
		t.Fatalf("Param(thread) = %q, %v", got, ok)
	}
	if _, ok := a.Param("absent"); ok {
		t.Error("Param reported an absent key as present")
	}
}

// Params are sorted on render so an address is stable as a map key and
// comparable across processes.
func TestStringIsDeterministic(t *testing.T) {
	a := address.MustParse("slack://T01/C02").
		WithParam("z", "1").
		WithParam("a", "2").
		WithParam("m", "3")
	want := "slack://T01/C02?a=2&m=3&z=1"
	for i := 0; i < 10; i++ {
		if got := a.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
}

func TestWithParamDoesNotMutate(t *testing.T) {
	base := address.MustParse("slack://T01/C02")
	derived := base.WithParam("thread", "1699.001")
	if _, ok := base.Param("thread"); ok {
		t.Fatal("WithParam mutated the receiver")
	}
	if _, ok := derived.Param("thread"); !ok {
		t.Fatal("WithParam did not set the key on the copy")
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"no scheme", "urmzd/fiber"},
		{"no workspace", "github://"},
		{"scheme only", "github:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := address.Parse(tc.in); err == nil {
				t.Fatalf("Parse(%q) = nil error", tc.in)
			} else if !errors.Is(err, address.ErrInvalid) {
				t.Fatalf("Parse(%q) error = %v, want ErrInvalid", tc.in, err)
			}
		})
	}
}

// The workspace becomes a topic-safe identifier downstream, so an unsafe one
// must fail at parse time rather than at publish time.
func TestParseRejectsUnsafeWorkspace(t *testing.T) {
	for _, in := range []string{
		"jira://acme.atlassian.net/PROJ-5",
		"github://urmzd:fiber/issues",
	} {
		if _, err := address.Parse(in); err == nil {
			t.Fatalf("Parse(%q) accepted an unsafe workspace", in)
		}
	}
}

// The path is where unsafe characters legitimately live: issue numbers, Jira
// keys, and thread timestamps all survive a round trip.
func TestPathCarriesUnsafeCharacters(t *testing.T) {
	a, err := address.Parse("jira://acme/PROJ-5?status=In%20Review")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.Resource(); got != "PROJ-5" {
		t.Fatalf("Resource() = %q", got)
	}
	if got, _ := a.Param("status"); got != "In Review" {
		t.Fatalf("Param(status) = %q", got)
	}
}

// A path segment is the unit of identity, so a separator inside one has to
// survive being written down and read back. Splitting the decoded path would
// silently turn one segment into two, and a target that names a different
// resource than the one the route asked for is a delivery to the wrong place.
func TestPathSegmentsSurviveEncodedSeparators(t *testing.T) {
	cases := []struct {
		name string
		path []string
	}{
		{"encoded slash", []string{"refs/heads/main"}},
		{"percent", []string{"50%"}},
		{"space and hash", []string{"design doc #4"}},
		{"question mark", []string{"a?b", "c"}},
		{"mixed", []string{"fiber", "pull/7", "files"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := address.Address{Connector: "github", Workspace: "urmzd", Path: tc.path}
			got, err := address.Parse(want.String())
			if err != nil {
				t.Fatalf("Parse(%q): %v", want.String(), err)
			}
			if len(got.Path) != len(tc.path) {
				t.Fatalf("Parse(%q).Path = %q, want %q", want.String(), got.Path, tc.path)
			}
			for i := range tc.path {
				if got.Path[i] != tc.path[i] {
					t.Fatalf("segment %d = %q, want %q", i, got.Path[i], tc.path[i])
				}
			}
			if got.String() != want.String() {
				t.Fatalf("re-rendered as %q, want %q", got.String(), want.String())
			}
		})
	}
}

// A connector's name is the scheme of every address that reaches it, so a name
// that cannot be a scheme names a connector nothing can address.
func TestValidScheme(t *testing.T) {
	for _, ok := range []string{"github", "slack", "cron", "webhook", "x-internal", "a.b", "s3+http"} {
		if err := address.ValidScheme(ok); err != nil {
			t.Errorf("ValidScheme(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "GitHub", "my_hook", "3cx", "-lead", "has space", "sla:ck"} {
		if err := address.ValidScheme(bad); err == nil {
			t.Errorf("ValidScheme(%q) = nil, want ErrInvalid", bad)
		} else if !errors.Is(err, address.ErrInvalid) {
			t.Errorf("ValidScheme(%q) error = %v, want ErrInvalid", bad, err)
		}
	}
	// The check has to agree with Parse: anything it accepts must be
	// resolvable, and anything it rejects must be unresolvable.
	for _, bad := range []string{"GitHub", "my_hook", "3cx"} {
		if _, err := address.Parse(bad + "://acme/thing"); err == nil {
			t.Errorf("Parse resolved %q://, which ValidScheme rejects", bad)
		}
	}
}

func TestIsZero(t *testing.T) {
	if !(address.Address{}).IsZero() {
		t.Error("zero Address did not report IsZero")
	}
	if address.MustParse("cron://acme/nightly").IsZero() {
		t.Error("parsed Address reported IsZero")
	}
	if got := (address.Address{}).String(); got != "" {
		t.Errorf("zero Address String() = %q, want empty", got)
	}
}
