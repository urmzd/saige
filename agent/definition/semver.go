package definition

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version: major.minor.patch with an optional
// pre-release and build. Build metadata is kept but never compared.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
	Build               string
}

// ParseVersion parses a full semantic version such as 1.4.0 or
// 2.0.0-rc.1. A leading "v" is accepted.
func ParseVersion(s string) (Version, error) {
	v, parts, err := parsePartial(s)
	if err != nil {
		return Version{}, err
	}
	if parts != 3 {
		return Version{}, fmt.Errorf("version %q must have major.minor.patch", s)
	}
	return v, nil
}

// parsePartial parses 1, 1.2 or 1.2.3 (with pre-release and build on a full
// version) and reports how many numeric parts were given. "x" and "*" end
// the version early.
func parsePartial(s string) (Version, int, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v Version
	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Build = s[i+1:]
		s = s[:i]
		if v.Build == "" || !validIdents(v.Build, false) {
			return Version{}, 0, fmt.Errorf("version %q: invalid build metadata", orig)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" || !validIdents(pre, true) {
			return Version{}, 0, fmt.Errorf("version %q: invalid pre-release", orig)
		}
		v.Pre = strings.Split(pre, ".")
	}
	fields := strings.Split(s, ".")
	if len(fields) > 3 || s == "" {
		return Version{}, 0, fmt.Errorf("invalid version %q", orig)
	}
	nums := []*int{&v.Major, &v.Minor, &v.Patch}
	n := 0
	for i, f := range fields {
		if f == "x" || f == "X" || f == "*" {
			if i < len(fields)-1 && fields[i+1] != "x" && fields[i+1] != "X" && fields[i+1] != "*" {
				return Version{}, 0, fmt.Errorf("invalid version %q", orig)
			}
			break
		}
		x, err := parseNumber(f)
		if err != nil {
			return Version{}, 0, fmt.Errorf("version %q: %w", orig, err)
		}
		*nums[i] = x
		n++
	}
	if (v.Pre != nil || v.Build != "") && n != 3 {
		return Version{}, 0, fmt.Errorf("version %q: a pre-release needs major.minor.patch", orig)
	}
	return v, n, nil
}

func parseNumber(f string) (int, error) {
	if f == "" || len(f) > 1 && f[0] == '0' {
		return 0, fmt.Errorf("invalid number %q", f)
	}
	x, err := strconv.Atoi(f)
	if err != nil || x < 0 {
		return 0, fmt.Errorf("invalid number %q", f)
	}
	return x, nil
}

func validIdents(s string, numericNoLeadingZero bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		numeric := true
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				numeric = false
			default:
				return false
			}
		}
		if numeric && numericNoLeadingZero && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare orders versions by semantic version precedence: -1, 0 or 1.
func (v Version) Compare(o Version) int {
	if c := cmp.Compare(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(v.Patch, o.Patch); c != 0 {
		return c
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		a, b := v.Pre[i], o.Pre[i]
		an, aerr := strconv.Atoi(a)
		bn, berr := strconv.Atoi(b)
		var c int
		switch {
		case aerr == nil && berr == nil:
			c = cmp.Compare(an, bn)
		case aerr == nil:
			c = -1
		case berr == nil:
			c = 1
		default:
			c = strings.Compare(a, b)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(v.Pre), len(o.Pre))
}

func (v Version) sameCore(o Version) bool {
	return v.Major == o.Major && v.Minor == o.Minor && v.Patch == o.Patch
}

// Range is a version constraint in npm syntax: exact versions (1.2.3),
// partial versions (1.2, 1.x), caret (^1.2) and tilde (~1.2.3) ranges,
// comparators (>=1.0.0 <2.0.0) and alternatives joined by ||. An empty
// range or * matches every release.
//
// A pre-release matches only a range that names a pre-release of the same
// major.minor.patch, so ^1.0.0 never picks 1.1.0-rc.1.
type Range struct {
	sets [][]comparator
	text string
}

type comparator struct {
	op string // "=", ">", ">=", "<", "<="
	v  Version
}

func (c comparator) matches(v Version) bool {
	d := v.Compare(c.v)
	switch c.op {
	case ">":
		return d > 0
	case ">=":
		return d >= 0
	case "<":
		return d < 0
	case "<=":
		return d <= 0
	}
	return d == 0
}

// ParseRange parses a version constraint.
func ParseRange(s string) (Range, error) {
	r := Range{text: strings.TrimSpace(s)}
	if r.text == "" {
		return r, nil
	}
	for alt := range strings.SplitSeq(r.text, "||") {
		var set []comparator
		fields := strings.Fields(alt)
		if len(fields) == 0 {
			return Range{}, fmt.Errorf("invalid version range %q: empty alternative", s)
		}
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			// "1.2.3 - 2.3.4" is an inclusive hyphen range.
			if i+2 < len(fields) && fields[i+1] == "-" {
				lo, err := expand(">=", fields[i])
				if err != nil {
					return Range{}, fmt.Errorf("invalid version range %q: %w", s, err)
				}
				hi, err := expand("<=", fields[i+2])
				if err != nil {
					return Range{}, fmt.Errorf("invalid version range %q: %w", s, err)
				}
				set = append(set, lo...)
				set = append(set, hi...)
				i += 2
				continue
			}
			op := ""
			for _, p := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
				if strings.HasPrefix(f, p) {
					op, f = p, f[len(p):]
					break
				}
			}
			if f == "" && i+1 < len(fields) {
				// An operator separated from its version: ">= 1.2.0".
				i++
				f = fields[i]
			}
			cs, err := expand(op, f)
			if err != nil {
				return Range{}, fmt.Errorf("invalid version range %q: %w", s, err)
			}
			set = append(set, cs...)
		}
		r.sets = append(r.sets, set)
	}
	return r, nil
}

// expand turns one operator and a possibly partial version into
// comparators.
func expand(op, s string) ([]comparator, error) {
	if s == "*" || s == "x" || s == "X" {
		if op == "" || op == "=" || op == ">=" || op == "<=" || op == "^" || op == "~" {
			return nil, nil
		}
		return nil, fmt.Errorf("%s%s matches nothing", op, s)
	}
	v, n, err := parsePartial(s)
	if err != nil {
		return nil, err
	}
	upper := func() Version {
		switch n {
		case 1:
			return Version{Major: v.Major + 1}
		case 2:
			return Version{Major: v.Major, Minor: v.Minor + 1}
		}
		return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	}
	// lowest pre-release of a version, so <2.0.0 excludes 2.0.0-rc.1 when
	// written as a partial bound.
	floor := func(x Version) Version { x.Pre = []string{"0"}; return x }
	switch op {
	case "", "=":
		if n == 3 {
			return []comparator{{op: "=", v: v}}, nil
		}
		return []comparator{{op: ">=", v: v}, {op: "<", v: floor(upper())}}, nil
	case ">=":
		return []comparator{{op: ">=", v: v}}, nil
	case ">":
		if n == 3 {
			return []comparator{{op: ">", v: v}}, nil
		}
		return []comparator{{op: ">=", v: upper()}}, nil
	case "<":
		return []comparator{{op: "<", v: floor(v)}}, nil
	case "<=":
		if n == 3 {
			return []comparator{{op: "<=", v: v}}, nil
		}
		return []comparator{{op: "<", v: floor(upper())}}, nil
	case "~":
		hi := Version{Major: v.Major, Minor: v.Minor + 1}
		if n == 1 {
			hi = Version{Major: v.Major + 1}
		}
		return []comparator{{op: ">=", v: v}, {op: "<", v: floor(hi)}}, nil
	case "^":
		var hi Version
		switch {
		case v.Major > 0 || n == 1:
			hi = Version{Major: v.Major + 1}
		case v.Minor > 0 || n == 2:
			hi = Version{Minor: v.Minor + 1}
		default:
			hi = Version{Patch: v.Patch + 1}
		}
		return []comparator{{op: ">=", v: v}, {op: "<", v: floor(hi)}}, nil
	}
	return nil, fmt.Errorf("unknown operator %q", op)
}

// Matches reports whether v satisfies the range.
func (r Range) Matches(v Version) bool {
	if len(r.sets) == 0 {
		return len(v.Pre) == 0
	}
	for _, set := range r.sets {
		if setMatches(set, v) {
			return true
		}
	}
	return false
}

func setMatches(set []comparator, v Version) bool {
	for _, c := range set {
		if !c.matches(v) {
			return false
		}
	}
	if len(v.Pre) == 0 {
		return true
	}
	for _, c := range set {
		if len(c.v.Pre) > 0 && c.v.sameCore(v) && !isFloor(c.v) {
			return true
		}
	}
	return false
}

// isFloor reports a synthetic lower pre-release bound.
func isFloor(v Version) bool { return len(v.Pre) == 1 && v.Pre[0] == "0" }

func (r Range) String() string { return r.text }
