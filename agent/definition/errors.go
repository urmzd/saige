package definition

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid matches every *ValidationError.
var ErrInvalid = errors.New("invalid agent definition")

// ErrNotFound reports a reference no loaded definition satisfies.
var ErrNotFound = errors.New("agent definition not found")

// Issue codes. Tests and tools match on these rather than on messages.
const (
	CodeSyntax         = "syntax"
	CodeFrontmatter    = "frontmatter"
	CodeUnsupported    = "unsupported_yaml"
	CodeUnknownKey     = "unknown_key"
	CodeDuplicateKey   = "duplicate_key"
	CodeWrongType      = "wrong_type"
	CodeNullNotAllowed = "null_not_allowed"
	CodeMissing        = "missing_field"
	CodeBadValue       = "invalid_value"
	CodeVersion        = "unsupported_version"
	CodeUntrusted      = "untrusted_field"
	CodeUnresolved     = "unresolved_reference"
	CodeCycle          = "reference_cycle"
	CodeDuplicate      = "duplicate_definition"
)

// Issue is one problem with a definition. Path addresses the value, such as
// "skills[1].mode", and Line is its line in the file when known.
type Issue struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (i Issue) String() string {
	var b strings.Builder
	if i.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", i.Line)
	}
	if i.Path != "" {
		b.WriteString(i.Path + ": ")
	}
	b.WriteString(i.Message)
	return b.String()
}

// ValidationError carries every issue found in one definition, or across a
// set of them. errors.Is(err, ErrInvalid) matches it.
type ValidationError struct {
	// Source names the file or definition, when known.
	Source string
	Issues []Issue
}

// maxReportedIssues bounds Error's length; Issues holds all of them.
const maxReportedIssues = 8

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(ErrInvalid.Error())
	if e.Source != "" {
		b.WriteString(" " + e.Source)
	}
	for i, is := range e.Issues {
		if i == maxReportedIssues {
			fmt.Fprintf(&b, "; and %d more", len(e.Issues)-i)
			break
		}
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(is.String())
	}
	return b.String()
}

// Is matches ErrInvalid.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

// issues collects problems with their paths.
type issues []Issue

func (l *issues) add(path string, line int, code, format string, args ...any) {
	*l = append(*l, Issue{Path: path, Line: line, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (l issues) asError(source string) error {
	if len(l) == 0 {
		return nil
	}
	return &ValidationError{Source: source, Issues: append([]Issue(nil), l...)}
}

// joinPath appends a key to a path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// suggest returns the candidate closest to s, when it is close enough to be
// a likely typo.
func suggest(s string, candidates []string) string {
	best, bestD := "", len(s)/2+1
	for _, c := range candidates {
		if d := editDistance(strings.ToLower(s), strings.ToLower(c)); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
