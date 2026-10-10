package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// fileChange is one file an export writes. Before is nil for a file that
// does not exist yet.
type fileChange struct {
	Path   string
	Before []byte
	After  []byte
	Mode   os.FileMode
}

// actionUnchanged is the action of a file the export leaves as it is.
const actionUnchanged = "unchanged"

func (c fileChange) action() string {
	switch {
	case c.Before == nil:
		return "create"
	case bytes.Equal(c.Before, c.After):
		return actionUnchanged
	}
	return "update"
}

// exportPlan is every file an export changes, and the definition fields
// the harness cannot express.
type exportPlan struct {
	changes []fileChange
	notes   []string
}

func (p *exportPlan) note(format string, args ...any) {
	p.notes = append(p.notes, fmt.Sprintf(format, args...))
}

// set plans path to hold after, reading what it holds now. A path planned
// twice keeps the first plan's Before.
func (p *exportPlan) set(path string, after []byte) error {
	for i, c := range p.changes {
		if c.Path == path {
			p.changes[i].After = after
			return nil
		}
	}
	before, err := readIfExists(path)
	if err != nil {
		return err
	}
	p.changes = append(p.changes, fileChange{Path: path, Before: before, After: after, Mode: 0o644})
	return nil
}

// executable marks a planned file as a script.
func (p *exportPlan) executable(path string) {
	for i, c := range p.changes {
		if c.Path == path {
			p.changes[i].Mode = 0o755
		}
	}
}

// current is what path holds once the plan's earlier changes apply.
func (p *exportPlan) current(path string) ([]byte, error) {
	for _, c := range p.changes {
		if c.Path == path {
			return c.After, nil
		}
	}
	return readIfExists(path)
}

func readIfExists(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the export's own target
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// apply writes every changed file, creating its directory.
func (p *exportPlan) apply() error {
	for _, c := range p.changes {
		if c.action() == actionUnchanged {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil { //nolint:gosec // config directories are meant to be readable
			return err
		}
		if err := os.WriteFile(c.Path, c.After, c.Mode); err != nil {
			return err
		}
	}
	return nil
}

// report prints each change, its diff when diffs is set, and the notes.
func (p *exportPlan) report(w io.Writer, base string, dryRun, diffs bool) {
	for _, c := range p.changes {
		action := c.action()
		if dryRun && action != actionUnchanged {
			action = "would " + action
		}
		fmt.Fprintf(w, "%-16s %s\n", action, displayPath(base, c.Path))
		if diffs && c.action() != actionUnchanged {
			fmt.Fprint(w, unifiedDiff(displayPath(base, c.Path), c.Before, c.After))
		}
	}
	for _, n := range p.notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

func displayPath(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// ── JSON merge ──────────────────────────────────────────────────────

// jsonMember is one key of a JSON object, in file order.
type jsonMember struct {
	key   string
	value json.RawMessage
}

// parseObject reads a JSON object, keeping its key order.
func parseObject(raw []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var out []jsonMember
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, jsonMember{key: key, value: v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON object")
	}
	return out, nil
}

func encodeObject(members []jsonMember) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// indentOf guesses a JSON file's indentation from its first indented line.
var indentRE = regexp.MustCompile(`(?m)^([ \t]+)\S`)

func indentOf(raw []byte) string {
	if m := indentRE.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return "  "
}

// mergeJSON sets the value at path (object keys) in a JSON document,
// creating objects on the way, and keeps every other key and their order.
// An empty document starts from seed, an object of keys to put first.
func mergeJSON(raw []byte, path []string, value any, seed []jsonMember) ([]byte, error) {
	val, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	indent := "  "
	doc := bytes.TrimSpace(raw)
	if len(doc) == 0 {
		doc = encodeObject(seed)
	} else {
		indent = indentOf(raw)
	}
	out, err := setJSONPath(doc, path, val)
	if err != nil {
		return nil, err
	}
	var compact, pretty bytes.Buffer
	if err := json.Compact(&compact, out); err != nil {
		return nil, err
	}
	if err := json.Indent(&pretty, compact.Bytes(), "", indent); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

func setJSONPath(doc []byte, path []string, val json.RawMessage) ([]byte, error) {
	members, err := parseObject(doc)
	if err != nil {
		return nil, err
	}
	key := path[0]
	for i, m := range members {
		if m.key != key {
			continue
		}
		if len(path) == 1 {
			members[i].value = val
			return encodeObject(members), nil
		}
		if trimmed := bytes.TrimSpace(m.value); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("%q is not an object", key)
		}
		inner, err := setJSONPath(m.value, path[1:], val)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		members[i].value = inner
		return encodeObject(members), nil
	}
	if len(path) == 1 {
		return encodeObject(append(members, jsonMember{key: key, value: val})), nil
	}
	inner, err := setJSONPath([]byte("{}"), path[1:], val)
	if err != nil {
		return nil, err
	}
	return encodeObject(append(members, jsonMember{key: key, value: inner})), nil
}

// ── TOML merge ──────────────────────────────────────────────────────

var tomlHeaderRE = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*(#.*)?$`)

// mergeTOMLTable replaces the table named by key (such as
// mcp_servers.saige-helper) and its sub-tables with block, or appends
// block, leaving every other line of the file as it was. A file that
// already defines the table's parent inline or with dotted keys at the top
// level is refused, since adding the table would define it twice.
func mergeTOMLTable(raw []byte, key, parent, block string) ([]byte, error) {
	lines := strings.SplitAfter(string(raw), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	dotted := regexp.MustCompile(`^\s*"?` + regexp.QuoteMeta(parent) + `"?\s*[.=]`)
	start, end := -1, len(lines)
	inTop := true
	for i, ln := range lines {
		m := tomlHeaderRE.FindStringSubmatch(strings.TrimRight(ln, "\r\n"))
		if m == nil {
			if inTop && dotted.MatchString(ln) {
				return nil, fmt.Errorf("the file defines %s with a top-level key; add the server by hand", parent)
			}
			continue
		}
		inTop = false
		name := normalizeTOMLKey(m[1])
		switch {
		case start < 0 && (name == key || strings.HasPrefix(name, key+".")):
			start = i
		case start >= 0 && name != key && !strings.HasPrefix(name, key+"."):
			end = i
		}
		if start >= 0 && end != len(lines) {
			break
		}
	}
	block = strings.TrimRight(block, "\n") + "\n"
	if start < 0 {
		out := strings.Join(lines, "")
		if out != "" {
			out = strings.TrimRight(out, "\n") + "\n\n"
		}
		return []byte(out + block), nil
	}
	// Keep the blank lines that separated the old table from the next.
	tail := lines[end:]
	sep := ""
	if len(tail) > 0 {
		sep = "\n"
		for end > start && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
	}
	return []byte(strings.Join(lines[:start], "") + block + sep + strings.Join(tail, "")), nil
}

// normalizeTOMLKey drops quotes and spaces from a dotted table key.
func normalizeTOMLKey(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// tomlString encodes s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlText encodes a long text readably: a multi-line literal string when
// it can hold it, a basic string otherwise.
func tomlText(s string) string {
	ok := !strings.Contains(s, "'''")
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			ok = false
		}
	}
	if ok && strings.Contains(s, "\n") && !strings.HasSuffix(s, "'") {
		return "'''\n" + s + "'''"
	}
	return tomlString(s)
}

func tomlStrings(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = tomlString(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ── diff ────────────────────────────────────────────────────────────

// unifiedDiff renders the change from a to b as a unified diff with three
// lines of context.
func unifiedDiff(name string, a, b []byte) string {
	al, bl := splitLines(a), splitLines(b)
	// Trim the common ends, so a small change in a large file stays cheap.
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	ops := diffLines(al[pre:len(al)-suf], bl[pre:len(bl)-suf])
	const ctx = 3
	var out strings.Builder
	fromName := "a/" + name
	if a == nil {
		fromName = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ b/%s\n", fromName, name)
	lo := max(0, pre-ctx)
	hiA := min(len(al), len(al)-suf+ctx)
	hiB := min(len(bl), len(bl)-suf+ctx)
	start := func(n int) int {
		if n == 0 {
			return lo
		}
		return lo + 1
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", start(hiA-lo), hiA-lo, start(hiB-lo), hiB-lo)
	for _, l := range al[lo:pre] {
		out.WriteString(" " + l + "\n")
	}
	for _, op := range ops {
		out.WriteString(string(op.kind) + op.line + "\n")
	}
	for _, l := range al[len(al)-suf : hiA] {
		out.WriteString(" " + l + "\n")
	}
	return out.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

type diffOp struct {
	kind byte
	line string
}

// diffLines is a longest-common-subsequence line diff. Inputs beyond a
// few thousand lines are shown as a removal and an addition.
func diffLines(a, b []string) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > 4_000_000 {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
