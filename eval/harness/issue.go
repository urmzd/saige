package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Severity grades an [Issue].
type Severity string

const (
	// SeverityError makes the input unusable: a run refuses it.
	SeverityError Severity = "error"
	// SeverityWarning loads and runs, but probably not as intended.
	SeverityWarning Severity = "warning"
)

// Issue is one problem found while validating a manifest or a corpus. File
// is the file the problem is in, and Pointer locates it inside a JSON file
// as an RFC 6901 JSON Pointer (empty for the whole file or a non-JSON file).
type Issue struct {
	File     string   `json:"file,omitempty"`
	Pointer  string   `json:"pointer,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// String renders the issue as "file:/pointer: severity: message".
func (i Issue) String() string {
	loc := i.File
	switch {
	case loc == "":
		loc = i.Pointer
	case i.Pointer != "":
		loc += ":" + i.Pointer
	}
	if loc == "" {
		return fmt.Sprintf("%s: %s", i.Severity, i.Message)
	}
	return fmt.Sprintf("%s: %s: %s", loc, i.Severity, i.Message)
}

// HasErrors reports whether any issue has [SeverityError].
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// IssuesError joins the error-severity issues into one error, or returns
// nil when there are none.
func IssuesError(issues []Issue) error {
	var lines []string
	for _, i := range issues {
		if i.Severity == SeverityError {
			lines = append(lines, i.String())
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%d validation error(s):\n  %s", len(lines), strings.Join(lines, "\n  "))
}

func errorIssue(pointer, format string, args ...any) Issue {
	return Issue{Pointer: pointer, Severity: SeverityError, Message: fmt.Sprintf(format, args...)}
}

func warningIssue(pointer, format string, args ...any) Issue {
	return Issue{Pointer: pointer, Severity: SeverityWarning, Message: fmt.Sprintf(format, args...)}
}

// withFile sets File on every issue.
func withFile(file string, issues []Issue) []Issue {
	for i := range issues {
		issues[i].File = file
	}
	return issues
}

// decodeStrict decodes data into out and reports every problem as an issue:
// a syntax error with its line and column, each key that out's type does not
// declare with its pointer (and the closest known key), and a value of the
// wrong type with its pointer. It reports false when out was not filled.
func decodeStrict(data []byte, out any) ([]Issue, bool) {
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			line, col := lineCol(data, syn.Offset)
			return []Issue{errorIssue("", "invalid JSON at line %d, column %d: %v", line, col, err)}, false
		}
		return []Issue{errorIssue("", "invalid JSON: %v", err)}, false
	}
	issues := unknownKeys(generic, reflect.TypeOf(out), "")
	if err := json.Unmarshal(data, out); err != nil {
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) {
			pointer := "/" + strings.ReplaceAll(typ.Field, ".", "/")
			if typ.Field == "" {
				pointer = ""
			}
			return append(issues, errorIssue(pointer, "expected %s, got JSON %s", typ.Type, typ.Value)), false
		}
		return append(issues, errorIssue("", "%v", err)), false
	}
	return issues, true
}

// lineCol converts a byte offset into a 1-based line and column.
func lineCol(data []byte, offset int64) (int, int) {
	line, col := 1, 1
	for i := int64(0); i < offset && i < int64(len(data)); i++ {
		if data[i] == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return line, col
}

// unknownKeys walks a generically decoded JSON value against the Go type it
// will be decoded into and reports keys that type does not declare.
func unknownKeys(v any, t reflect.Type, pointer string) []Issue {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonFields(t)
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var issues []Issue
		for _, key := range keys {
			child := pointer + "/" + escapePointer(key)
			ft, ok := fields[key]
			if !ok {
				msg := fmt.Sprintf("unknown key %q", key)
				if near := closest(key, names); near != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", near)
				}
				issues = append(issues, errorIssue(child, "%s", msg))
				continue
			}
			issues = append(issues, unknownKeys(obj[key], ft, child)...)
		}
		return issues
	case reflect.Slice, reflect.Array:
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		var issues []Issue
		for i, item := range arr {
			issues = append(issues, unknownKeys(item, t.Elem(), fmt.Sprintf("%s/%d", pointer, i))...)
		}
		return issues
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		var issues []Issue
		for key, item := range obj {
			issues = append(issues, unknownKeys(item, t.Elem(), pointer+"/"+escapePointer(key))...)
		}
		return issues
	}
	return nil
}

// jsonFields maps the JSON names of a struct's fields to their types,
// following encoding/json's rules for tags, unexported fields, and
// embedded structs.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// closest returns the candidate within edit distance 2 of s, or "".
func closest(s string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := editDistance(s, c); d < bestDist {
			best, bestDist = c, d
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
