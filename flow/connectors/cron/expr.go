package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrExpression reports a cron expression this parser will not accept. Callers
// match with errors.Is.
//
// Every rejection is loud and immediate, at parse time, because the failure mode
// of the alternative is the worst one a scheduler has: an expression that is
// quietly reinterpreted runs an agent at hours nobody asked for, and nothing in
// the system ever reports an error. "@daily", "MON-FRI", "0 0 L * *" and
// "*/5 * * * * *" are all rejected here rather than approximated.
var ErrExpression = errors.New("cron: invalid expression")

// searchYears bounds how far Next will look for an occurrence. An expression
// like "0 0 30 2 *" — the thirtieth of February — is syntactically valid and
// never happens, and without a bound Next would loop forever looking for it.
// Five years is past every real schedule and past every leap year cycle.
const searchYears = 5

// field describes one of the five positions and the numbers it accepts.
type field struct {
	name     string
	min, max int
}

// fields are the five positions of a standard crontab line, in order.
//
// Day of week accepts 7 as well as 0 for Sunday. That is not laxity: crontab(5)
// documents both spellings, and half the schedules written by humans use 7.
var fields = [5]field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12},
	{name: "day of week", min: 0, max: 7},
}

// Spec is a parsed 5-field cron expression: minute, hour, day of month, month,
// day of week.
//
// Each field is a bit set rather than a list, so matching an instant is four
// shifts and four masks and Next can walk forward without allocating. The zero
// Spec matches nothing; use Parse.
type Spec struct {
	minute, hour, dom, month, dow uint64

	// domStar and dowStar record whether the day fields were written as a bare
	// "*". They are needed because the two day fields combine with OR when both
	// are restricted and with AND when either is not — see dayMatches.
	domStar, dowStar bool

	text string
}

// Parse reads a standard 5-field cron expression.
//
// The grammar, and nothing beyond it:
//
//	field  := item ("," item)*
//	item   := "*" | "*" "/" step | value | value "-" value | value "-" value "/" step
//	value  := a decimal number inside the field's range
//	step   := a positive decimal number
//
// Names (JAN, MON), macros (@daily), seconds, and the Quartz extensions (?, L,
// W, #) are not accepted, and each is refused with a message that says so. A
// scheduler that guessed at those would fire an agent on a day nobody chose.
func Parse(expr string) (Spec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf("%w: %q has %d fields, want 5: minute hour day-of-month month day-of-week",
			ErrExpression, expr, len(parts))
	}

	var (
		s    Spec
		bits [5]uint64
		star [5]bool
		err  error
	)
	for i, part := range parts {
		if bits[i], star[i], err = parseField(part, fields[i]); err != nil {
			return Spec{}, err
		}
	}

	s.minute, s.hour, s.dom, s.month, s.dow = bits[0], bits[1], bits[2], bits[3], bits[4]
	s.domStar, s.dowStar = star[2], star[4]
	s.text = strings.Join(parts, " ")
	return s, nil
}

// MustParse is Parse for constants and tests. It panics on a bad expression.
func MustParse(expr string) Spec {
	s, err := Parse(expr)
	if err != nil {
		panic(err)
	}
	return s
}

// String returns the expression as parsed, with runs of whitespace normalized.
func (s Spec) String() string { return s.text }

// parseField reads one comma-separated field.
//
// star reports whether the field was written as exactly "*". It is deliberately
// not "the field matches every value": "*/1" and "0-59" also match every minute,
// yet crontab(5) treats only the literal "*" as unrestricted when combining the
// two day fields, and reproducing that distinction is the difference between
// "0 0 * * 1" meaning Mondays and meaning every day.
func parseField(text string, f field) (uint64, bool, error) {
	star := text == "*"
	var bits uint64
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, false, fmt.Errorf("%w: %s field %q has an empty list item", ErrExpression, f.name, text)
		}
		b, err := parseItem(item, f)
		if err != nil {
			return 0, false, err
		}
		bits |= b
	}
	return bits, star, nil
}

// parseItem reads one list item: a value, a range, or either with a step.
func parseItem(item string, f field) (uint64, error) {
	spec, stepText, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepText)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%w: %s step %q is not a positive number, as in */5", ErrExpression, f.name, stepText)
		}
		step = n
	}

	var lo, hi int
	switch {
	case spec == "*":
		lo, hi = f.min, f.max

	case strings.Contains(spec, "-"):
		loText, hiText, _ := strings.Cut(spec, "-")
		var err error
		if lo, err = f.number(loText); err != nil {
			return 0, err
		}
		if hi, err = f.number(hiText); err != nil {
			return 0, err
		}
		if lo > hi {
			// Wrapping ranges ("22-2" for late nights) are a real thing people
			// want and a real thing this parser does not do. Saying so is better
			// than silently matching only hour 22.
			return 0, fmt.Errorf("%w: %s range %q runs backwards; write it as two items, %d-%d,%d-%d",
				ErrExpression, f.name, spec, lo, f.max, f.min, hi)
		}

	default:
		n, err := f.number(spec)
		if err != nil {
			return 0, err
		}
		if hasStep {
			return 0, fmt.Errorf("%w: %s step %q applies to a single value, which means only %d; write %d-%d/%s or */%s",
				ErrExpression, f.name, item, n, n, f.max, stepText, stepText)
		}
		lo, hi = n, n
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		// Day of week accepts 7 for Sunday and stores it as 0, which is what
		// time.Weekday reports.
		if v == 7 && f.name == fields[4].name {
			bits |= 1 << 0
			continue
		}
		bits |= 1 << uint(v)
	}
	return bits, nil
}

// number reads one value and bounds it to the field.
func (f field) number(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %s value %q is not a number; this parser takes numbers only, so write 1 rather than MON and 0 0 * * * rather than @daily",
			ErrExpression, f.name, s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%w: %s value %d is outside %d-%d", ErrExpression, f.name, n, f.min, f.max)
	}
	return n, nil
}

// Next returns the first occurrence strictly after t, computed in t's location,
// and reports whether one was found within searchYears.
//
// Strictly after is the property the scheduler is built on: Next(occurrence) is
// the following occurrence, never the same one, so advancing a cursor by
// repeated calls cannot stall or repeat.
//
// The walk skips rather than steps. A day whose month or weekday does not match
// advances a whole day, an hour that does not match advances a whole hour, and
// only a matching hour is scanned minute by minute — so "0 3 1 1 *" costs a few
// hundred comparisons a year instead of half a million.
func (s Spec) Next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	// Start at the top of the minute following after, so a call at 12:00:30
	// returns 12:01 and a call at exactly 12:00:00 returns 12:01 as well.
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)
	limit := t.AddDate(searchYears, 0, 0)

	for t.Before(limit) {
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
			continue
		}
		if !has(s.hour, t.Hour()) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !has(s.minute, t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// Matches reports whether t falls on an occurrence, to the minute.
func (s Spec) Matches(t time.Time) bool {
	return s.dayMatches(t) && has(s.hour, t.Hour()) && has(s.minute, t.Minute())
}

// dayMatches applies crontab(5)'s day rule, which is the one part of cron that
// surprises everybody:
//
//	both fields "*"          every day
//	only day-of-month given  that day of the month
//	only day-of-week given   that day of the week
//	both given               EITHER, not both
//
// So "0 0 1 * 1" is the first of the month AND every Monday, not "the first of
// the month if it is a Monday". Implementing it as an AND is the classic cron
// bug, and it is silent: the schedule simply almost never fires.
func (s Spec) dayMatches(t time.Time) bool {
	if !has(s.month, int(t.Month())) {
		return false
	}
	dom := has(s.dom, t.Day())
	dow := has(s.dow, int(t.Weekday()))
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

func has(bits uint64, v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return bits&(1<<uint(v)) != 0
}
