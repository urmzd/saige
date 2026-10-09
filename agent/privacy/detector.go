// Package privacy keeps personal data out of provider requests, the
// conversation tree, telemetry, and durable snapshots.
//
// A Detector finds sensitive spans. A Vault swaps each value for a
// reversible placeholder such as <<EMAIL_1>> and restores it later; the same
// value always maps to the same placeholder within a vault. ToolRedactor
// applies a vault at the tool boundary: tools receive real values, everything
// else sees placeholders. Provider applies a vault at the provider boundary
// for hosts that only need to keep personal data in process.
//
// Placeholders are reversible. Redact replaces values permanently with
// [REDACTED:LABEL] for data that must never come back, such as memory writes.
package privacy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Span is one sensitive value found in a text, as a half-open byte range.
type Span struct {
	Start int
	End   int
	Label string // e.g. EMAIL, PHONE, CREDIT_CARD
}

// Detector finds sensitive spans in a text. Spans must lie within text and
// must not overlap; Chain resolves overlaps when several detectors are
// combined. A named-entity model can implement Detector and feed the same
// placeholder pipeline, so its findings are reversible too.
type Detector interface {
	Detect(ctx context.Context, text string) ([]Span, error)
}

// DetectorFunc adapts a function into a Detector.
type DetectorFunc func(ctx context.Context, text string) ([]Span, error)

// Detect implements Detector.
func (f DetectorFunc) Detect(ctx context.Context, text string) ([]Span, error) { return f(ctx, text) }

// Pattern is one regular expression with an optional validator. A match the
// validator rejects is not reported, which keeps checksummed formats such as
// card numbers from flagging every long digit run.
type Pattern struct {
	Label    string
	Expr     string
	Validate func(match string) bool
}

// RegexDetector matches every pattern in one pass over the text. The patterns
// are combined into a single alternation, so at each position the first
// pattern in order wins. Put specific patterns before general ones.
//
// A match the validator rejects does not hide the text from the other
// patterns: when the single pass meets a rejection, the detector scans again
// with each pattern on its own and keeps, at each position, the first
// pattern in order whose match is valid.
type RegexDetector struct {
	re       *regexp.Regexp
	patterns []Pattern
	groups   []int            // capture group index of each pattern
	each     []*regexp.Regexp // each pattern compiled alone
}

// NewRegexDetector compiles patterns into one detector.
func NewRegexDetector(patterns ...Pattern) (*RegexDetector, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("privacy: no patterns")
	}
	parts := make([]string, len(patterns))
	eachRe := make([]*regexp.Regexp, 0, len(patterns))
	for i, p := range patterns {
		if p.Label == "" {
			return nil, fmt.Errorf("privacy: pattern %d has no label", i)
		}
		if !validLabel(p.Label) {
			return nil, fmt.Errorf("privacy: label %q must be upper-case letters, digits, or underscores", p.Label)
		}
		each, err := regexp.Compile(p.Expr)
		if err != nil {
			return nil, fmt.Errorf("privacy: pattern %s: %w", p.Label, err)
		}
		eachRe = append(eachRe, each)
		parts[i] = fmt.Sprintf("(?P<p%d>%s)", i, p.Expr)
	}
	re, err := regexp.Compile(strings.Join(parts, "|"))
	if err != nil {
		return nil, fmt.Errorf("privacy: combine patterns: %w", err)
	}
	d := &RegexDetector{re: re, patterns: append([]Pattern(nil), patterns...), groups: make([]int, len(patterns)), each: eachRe}
	for i := range patterns {
		d.groups[i] = re.SubexpIndex(fmt.Sprintf("p%d", i))
	}
	return d, nil
}

// Detect implements Detector.
func (d *RegexDetector) Detect(_ context.Context, text string) ([]Span, error) {
	var spans []Span
	for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
		for i, g := range d.groups {
			start, end := m[2*g], m[2*g+1]
			if start < 0 {
				continue
			}
			p := d.patterns[i]
			if p.Validate != nil && !p.Validate(text[start:end]) {
				return d.detectEach(text), nil
			}
			spans = append(spans, Span{Start: start, End: end, Label: p.Label})
			break
		}
	}
	return spans, nil
}

// detectEach runs every pattern alone, drops matches its validator rejects,
// and merges the rest: the earliest start wins, then the earlier pattern.
// Overlapping later matches are dropped.
func (d *RegexDetector) detectEach(text string) []Span {
	type ranked struct {
		Span
		order int
	}
	var all []ranked
	for i, re := range d.each {
		p := d.patterns[i]
		for _, m := range re.FindAllStringIndex(text, -1) {
			if p.Validate == nil || p.Validate(text[m[0]:m[1]]) {
				all = append(all, ranked{Span{Start: m[0], End: m[1], Label: p.Label}, i})
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].order < all[j].order
	})
	var spans []Span
	end := -1
	for _, s := range all {
		if s.Start < end {
			continue
		}
		spans = append(spans, s.Span)
		end = s.End
	}
	return spans
}

// DefaultPatterns returns the built-in patterns, specific formats first.
func DefaultPatterns() []Pattern {
	return []Pattern{
		{Label: "EMAIL", Expr: `[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`},
		{Label: "SECRET", Expr: `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b|\b(?:sk|pk|rk)_(?:live|test)_[0-9A-Za-z]{16,}\b|\bgh[pousr]_[0-9A-Za-z]{36,}\b`},
		{Label: "CREDIT_CARD", Expr: `\b(?:\d[ -]?){12,18}\d\b`, Validate: validCard},
		{Label: "SSN", Expr: `\b\d{3}-\d{2}-\d{4}\b`, Validate: validSSN},
		{Label: "IBAN", Expr: `\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`, Validate: validIBAN},
		{Label: "IP_ADDRESS", Expr: `\b(?:\d{1,3}\.){3}\d{1,3}\b`, Validate: validIPv4},
		{Label: "PHONE", Expr: `(?:\+\d{1,3}[ .-]?)?(?:\(\d{3}\)[ .-]?|\b\d{3}[ .-]?)\d{3}[ .-]?\d{4}\b`, Validate: validPhone},
	}
}

var defaultDetector = func() *RegexDetector {
	d, err := NewRegexDetector(DefaultPatterns()...)
	if err != nil {
		panic(err)
	}
	return d
}()

// DefaultDetector returns a detector for the built-in patterns.
func DefaultDetector() Detector { return defaultDetector }

// Chain runs several detectors and merges their spans. Where spans overlap,
// the earliest start wins, then the longest, then the earlier detector.
func Chain(detectors ...Detector) Detector {
	return DetectorFunc(func(ctx context.Context, text string) ([]Span, error) {
		type ranked struct {
			Span
			order int
		}
		var all []ranked
		for i, d := range detectors {
			spans, err := d.Detect(ctx, text)
			if err != nil {
				return nil, err
			}
			for _, s := range spans {
				all = append(all, ranked{s, i})
			}
		}
		sort.SliceStable(all, func(i, j int) bool {
			a, b := all[i], all[j]
			if a.Start != b.Start {
				return a.Start < b.Start
			}
			if a.End-a.Start != b.End-b.Start {
				return a.End-a.Start > b.End-b.Start
			}
			return a.order < b.order
		})
		out := make([]Span, 0, len(all))
		end := -1
		for _, s := range all {
			if s.Start < end {
				continue
			}
			out = append(out, s.Span)
			end = s.End
		}
		return out, nil
	})
}

// detectChecked runs d and rejects spans that fall outside text or overlap,
// so a faulty detector cannot corrupt the text being rewritten.
func detectChecked(ctx context.Context, d Detector, text string) ([]Span, error) {
	spans, err := d.Detect(ctx, text)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	prev := 0
	for _, s := range spans {
		if s.Start < prev || s.End <= s.Start || s.End > len(text) {
			return nil, fmt.Errorf("privacy: detector returned invalid span [%d,%d) for text of length %d", s.Start, s.End, len(text))
		}
		if !validLabel(s.Label) {
			return nil, fmt.Errorf("privacy: detector returned invalid label %q", s.Label)
		}
		prev = s.End
	}
	return spans, nil
}

// Redact replaces every detected value with [REDACTED:LABEL]. Unlike a vault
// placeholder it cannot be reversed.
func Redact(ctx context.Context, d Detector, text string) (string, error) {
	if d == nil {
		d = DefaultDetector()
	}
	spans, err := detectChecked(ctx, d, text)
	if err != nil {
		return "", err
	}
	return rewrite(text, spans, func(s Span) string { return "[REDACTED:" + s.Label + "]" }), nil
}

func rewrite(text string, spans []Span, replace func(Span) string) string {
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s.Start])
		b.WriteString(replace(s))
		last = s.End
	}
	b.WriteString(text[last:])
	return b.String()
}

func validLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, r := range label {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validCard checks the length and the Luhn checksum.
func validCard(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// validSSN rejects area numbers 000, 666, and 900-999, group 00, and serial 0000.
func validSSN(s string) bool {
	d := digits(s)
	if len(d) != 9 {
		return false
	}
	area, group, serial := d[:3], d[3:5], d[5:]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

// validIBAN checks the ISO 13616 mod-97 checksum.
func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	rem := 0
	for _, r := range rearranged {
		switch {
		case r >= '0' && r <= '9':
			rem = (rem*10 + int(r-'0')) % 97
		case r >= 'A' && r <= 'Z':
			rem = (rem*100 + int(r-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// validIPv4 checks that every octet is at most 255.
func validIPv4(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if len(part) == 0 || len(part) > 3 {
			return false
		}
		n := 0
		for _, r := range part {
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

func validPhone(s string) bool {
	n := len(digits(s))
	return n >= 10 && n <= 15
}
