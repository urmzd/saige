package skills

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrFrontmatter reports a SKILL.md whose frontmatter is missing or cannot
// be parsed.
var ErrFrontmatter = errors.New("skills: invalid frontmatter")

// Frontmatter is the parsed YAML header of a SKILL.md. Each value is a
// string, a []string, or a map[string]string.
type Frontmatter map[string]any

// ParseSkillFile splits a SKILL.md into its frontmatter and body.
//
// The frontmatter is the block between a leading "---" line and the next
// "---" line. It is parsed as the subset of YAML that skill headers use:
// top-level keys with plain, quoted, or block (| and >) scalars, flow
// ([a, b]) and block (- a) lists of scalars, and one level of nested
// key: value mappings such as metadata. Anchors, tags, and deeper nesting
// are rejected rather than guessed at, so a header means one thing.
func ParseSkillFile(data []byte) (Frontmatter, string, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("%w: SKILL.md must start with a --- line", ErrFrontmatter)
	}
	rest := text[len("---\n"):]
	var header, body string
	switch {
	case strings.HasPrefix(rest, "---\n") || rest == "---":
		header, body = "", strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
	default:
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			if !strings.HasSuffix(rest, "\n---") {
				return nil, "", fmt.Errorf("%w: no closing --- line", ErrFrontmatter)
			}
			end = len(rest) - len("\n---")
			header, body = rest[:end], ""
		} else {
			header, body = rest[:end], rest[end+len("\n---\n"):]
		}
	}
	fm, err := parseHeader(header)
	if err != nil {
		return nil, "", err
	}
	return fm, strings.TrimLeft(body, "\n"), nil
}

// String returns a scalar value, or "" when the key is absent or not a
// scalar.
func (f Frontmatter) String(key string) string {
	s, _ := f[key].(string)
	return s
}

type line struct {
	indent int
	text   string // without indentation
	num    int    // 1-based, counting the opening --- line
}

func parseHeader(header string) (Frontmatter, error) {
	var lines []line
	for i, raw := range strings.Split(header, "\n") {
		if lead := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]; strings.ContainsRune(lead, '\t') {
			return nil, fmt.Errorf("%w: line %d: tabs are not allowed in indentation", ErrFrontmatter, i+2)
		}
		trimmed := strings.TrimLeft(raw, " ")
		lines = append(lines, line{indent: len(raw) - len(trimmed), text: strings.TrimRight(trimmed, " "), num: i + 2})
	}
	fm := Frontmatter{}
	for i := 0; i < len(lines); {
		ln := lines[i]
		if ln.text == "" || strings.HasPrefix(ln.text, "#") {
			i++
			continue
		}
		if ln.indent != 0 {
			return nil, fmt.Errorf("%w: line %d: unexpected indentation", ErrFrontmatter, ln.num)
		}
		key, rest, err := splitKey(ln)
		if err != nil {
			return nil, err
		}
		if _, dup := fm[key]; dup {
			return nil, fmt.Errorf("%w: line %d: duplicate key %q", ErrFrontmatter, ln.num, key)
		}
		// The indented lines that follow belong to this key.
		j := i + 1
		for j < len(lines) && (lines[j].text == "" || lines[j].indent > 0) {
			j++
		}
		child := lines[i+1 : j]
		value, err := parseValue(ln, rest, child)
		if err != nil {
			return nil, err
		}
		fm[key] = value
		i = j
	}
	return fm, nil
}

func splitKey(ln line) (string, string, error) {
	key, rest, ok := strings.Cut(ln.text, ":")
	if !ok || key == "" || strings.ContainsAny(key, " \"'[]{}") {
		return "", "", fmt.Errorf("%w: line %d: expected \"key: value\"", ErrFrontmatter, ln.num)
	}
	if rest != "" && rest[0] != ' ' {
		return "", "", fmt.Errorf("%w: line %d: expected a space after %q", ErrFrontmatter, ln.num, key+":")
	}
	return key, strings.TrimSpace(rest), nil
}

func parseValue(ln line, rest string, child []line) (any, error) {
	switch {
	case strings.HasPrefix(rest, "&") || strings.HasPrefix(rest, "*") || strings.HasPrefix(rest, "!"):
		return nil, fmt.Errorf("%w: line %d: anchors, aliases, and tags are not supported", ErrFrontmatter, ln.num)
	case strings.HasPrefix(rest, "{"):
		return nil, fmt.Errorf("%w: line %d: flow mappings are not supported", ErrFrontmatter, ln.num)
	case isBlockIndicator(rest):
		return blockScalar(rest, child), nil
	case rest == "":
		return nested(child)
	}
	if hasContent(child) {
		// A plain scalar continued on indented lines folds into one line.
		if strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, "\"") || strings.HasPrefix(rest, "'") {
			return nil, fmt.Errorf("%w: line %d: multi-line quoted or flow values are not supported", ErrFrontmatter, ln.num)
		}
		parts := []string{stripComment(rest)}
		for _, c := range child {
			if c.text != "" {
				parts = append(parts, c.text)
			}
		}
		return strings.Join(parts, " "), nil
	}
	if strings.HasPrefix(rest, "[") {
		return flowList(ln, rest)
	}
	return scalar(ln, rest)
}

func hasContent(lines []line) bool {
	for _, l := range lines {
		if l.text != "" {
			return true
		}
	}
	return false
}

func isBlockIndicator(s string) bool {
	switch stripComment(s) {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

// blockScalar renders a literal (|) or folded (>) block with YAML's
// chomping indicators: "-" strips the final newline, "+" keeps trailing
// blank lines, and the default keeps exactly one newline.
func blockScalar(indicator string, child []line) string {
	indicator = stripComment(indicator)
	for len(child) > 0 && child[len(child)-1].text == "" && indicator[len(indicator)-1] != '+' {
		child = child[:len(child)-1]
	}
	base := -1
	for _, c := range child {
		if c.text != "" && (base < 0 || c.indent < base) {
			base = c.indent
		}
	}
	var rows []string
	for _, c := range child {
		if c.text == "" {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, strings.Repeat(" ", c.indent-base)+c.text)
	}
	var out string
	if indicator[0] == '|' {
		out = strings.Join(rows, "\n")
	} else {
		var b strings.Builder
		for i, r := range rows {
			// Adjacent lines fold into one with a space; each blank line
			// becomes one newline.
			switch {
			case i == 0:
			case r == "":
				b.WriteString("\n")
			case rows[i-1] != "":
				b.WriteString(" ")
			}
			b.WriteString(r)
		}
		out = b.String()
	}
	switch indicator[len(indicator)-1] {
	case '-':
		return out
	case '+':
		return out + "\n"
	default:
		if out == "" {
			return ""
		}
		return out + "\n"
	}
}

// nested parses the indented block under a key with no inline value: a
// list of scalars or a one-level mapping of scalars.
func nested(child []line) (any, error) {
	var items []line
	for _, c := range child {
		if c.text != "" && !strings.HasPrefix(c.text, "#") {
			items = append(items, c)
		}
	}
	if len(items) == 0 {
		return "", nil
	}
	indent := items[0].indent
	if strings.HasPrefix(items[0].text, "- ") || items[0].text == "-" {
		list := make([]string, 0, len(items))
		for _, it := range items {
			if it.indent != indent || (!strings.HasPrefix(it.text, "- ") && it.text != "-") {
				return nil, fmt.Errorf("%w: line %d: lists may contain only scalars", ErrFrontmatter, it.num)
			}
			v, err := scalar(it, strings.TrimSpace(strings.TrimPrefix(it.text, "-")))
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	}
	m := make(map[string]string, len(items))
	for _, it := range items {
		if it.indent != indent {
			return nil, fmt.Errorf("%w: line %d: only one level of nesting is supported", ErrFrontmatter, it.num)
		}
		key, rest, err := splitKey(it)
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("%w: line %d: duplicate key %q", ErrFrontmatter, it.num, key)
		}
		if rest == "" || isBlockIndicator(rest) {
			return nil, fmt.Errorf("%w: line %d: only one level of nesting is supported", ErrFrontmatter, it.num)
		}
		if strings.HasPrefix(rest, "[") {
			// A nested flow list is kept as written; nested values are
			// strings.
			m[key] = stripComment(rest)
			continue
		}
		v, err := scalar(it, rest)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	return m, nil
}

func flowList(ln line, s string) ([]string, error) {
	s = stripComment(s)
	if !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("%w: line %d: unterminated list", ErrFrontmatter, ln.num)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, nil
	}
	var (
		items []string
		cur   strings.Builder
		quote rune
	)
	for _, r := range inner {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ',':
			items = append(items, cur.String())
			cur.Reset()
		case r == '[' || r == ']' || r == '{' || r == '}':
			return nil, fmt.Errorf("%w: line %d: nested collections are not supported", ErrFrontmatter, ln.num)
		default:
			cur.WriteRune(r)
		}
	}
	items = append(items, cur.String())
	out := make([]string, 0, len(items))
	for _, it := range items {
		v, err := scalar(ln, strings.TrimSpace(it))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func scalar(ln line, s string) (string, error) {
	switch {
	case strings.HasPrefix(s, "\""):
		end := closingDoubleQuote(s)
		if end < 0 || strings.TrimSpace(stripComment(s[end+1:])) != "" {
			return "", fmt.Errorf("%w: line %d: malformed double-quoted value", ErrFrontmatter, ln.num)
		}
		v, err := strconv.Unquote(s[:end+1])
		if err != nil {
			return "", fmt.Errorf("%w: line %d: %w", ErrFrontmatter, ln.num, err)
		}
		return v, nil
	case strings.HasPrefix(s, "'"):
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			if strings.TrimSpace(stripComment(s[i+1:])) != "" {
				break
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("%w: line %d: malformed single-quoted value", ErrFrontmatter, ln.num)
	}
	return stripComment(s), nil
}

func closingDoubleQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// stripComment removes a trailing " #" comment from a plain value.
func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
