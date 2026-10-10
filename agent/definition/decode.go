package definition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/urmzd/saige/agent/provider/catalog"
)

// Parse strictly decodes one definition file: YAML frontmatter between a
// leading "---" line and the next "---" line, then the Markdown body, which
// becomes the system prompt. path names the file in errors and in
// Definition.Path.
//
// Decoding is strict: an unknown key, a duplicate key, a value of the wrong
// kind, null, and YAML anchors, aliases, tags and merge keys are errors, each
// with the path and line of the value. The result is then validated (see
// Validate). Errors are a *ValidationError.
func Parse(data []byte, path string) (*Definition, error) {
	raw := normalize(data)
	header, body, headerLine, err := splitFrontmatter(raw)
	if err != nil {
		var found issues
		found.add("", 1, CodeFrontmatter, "%v", err)
		return nil, found.asError(path)
	}
	var found issues
	value := decodeHeader(header, headerLine, &found)
	if len(found) > 0 {
		return nil, found.asError(path)
	}
	enc, err := json.Marshal(value)
	if err != nil {
		found.add("", 0, CodeSyntax, "%v", err)
		return nil, found.asError(path)
	}
	d := &Definition{}
	dec := json.NewDecoder(bytes.NewReader(enc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		// The walk checked every key and kind, so this is a defect in the
		// walk rather than in the file; report it all the same.
		found.add("", 0, CodeSyntax, "%v", err)
		return nil, found.asError(path)
	}
	d.Prompt = strings.TrimSpace(body)
	d.raw = raw
	d.Digest = digest(raw)
	d.Path = path
	d.Trusted = true
	if err := d.Validate(); err != nil {
		if ve, ok := err.(*ValidationError); ok {
			ve.Source = path
			addLines(ve.Issues, header, headerLine)
		}
		return nil, err
	}
	return d, nil
}

// normalize strips a byte order mark and turns CRLF into LF, so the digest
// does not depend on how a checkout writes line endings.
func normalize(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// splitFrontmatter returns the YAML header, the body and the file line the
// header starts on.
func splitFrontmatter(raw []byte) (header, body string, headerLine int, err error) {
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") && text != "---" {
		return "", "", 0, fmt.Errorf("a definition must start with a --- line followed by YAML frontmatter")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(text, "---"), "\n")
	lines := strings.SplitAfter(rest, "\n")
	offset := 0
	for _, ln := range lines {
		if strings.TrimRight(ln, " \t\n") == "---" {
			return rest[:offset], rest[offset+len(ln):], 2, nil
		}
		offset += len(ln)
	}
	return "", "", 0, fmt.Errorf("the frontmatter has no closing --- line")
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// decodeHeader parses the YAML and walks it against Definition.
func decodeHeader(header string, firstLine int, found *issues) any {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(header), &doc); err != nil {
		msg := strings.TrimPrefix(err.Error(), "yaml: ")
		line := 0
		msg = yamlLine.ReplaceAllStringFunc(msg, func(m string) string {
			n, _ := strconv.Atoi(strings.TrimPrefix(m, "line "))
			line = n + firstLine - 1
			return "line " + strconv.Itoa(line)
		})
		found.add("", line, CodeSyntax, "%s", msg)
		return nil
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		found.add("", firstLine, CodeMissing, "the frontmatter is empty; it needs at least apiVersion, name and version")
		return nil
	}
	w := walker{found: found, lineOffset: firstLine - 1}
	return w.value(doc.Content[0], reflect.TypeFor[Definition](), "")
}

// YAML core schema tags.
const (
	tagNull  = "!!null"
	tagBool  = "!!bool"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagMerge = "!!merge"
)

const anObject = "an object"

var (
	durationType  = reflect.TypeFor[catalog.Duration]()
	shorthandType = reflect.TypeFor[shorthand]()
)

type walker struct {
	found      *issues
	lineOffset int
}

func (w walker) line(n *yaml.Node) int { return n.Line + w.lineOffset }

func (w walker) errorf(n *yaml.Node, path, code, format string, args ...any) {
	w.found.add(path, w.line(n), code, format, args...)
}

func kindName(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return anObject
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case tagInt, tagFloat:
			return "a number"
		case tagBool:
			return "a boolean"
		case tagNull:
			return "null"
		}
		return "a string"
	}
	return "a value"
}

func wantName(t reflect.Type) string {
	if t == durationType {
		return "a duration string such as \"30s\""
	}
	switch t.Kind() {
	case reflect.Struct:
		if t.Implements(shorthandType) {
			return "a string or an object"
		}
		return anObject
	case reflect.Map:
		return anObject
	case reflect.Slice:
		return "a list"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int64:
		return "an integer"
	case reflect.Float64:
		return "a number"
	}
	return t.Kind().String()
}

// value checks n against t and returns it as a JSON value. A problem is
// recorded and nil returned.
func (w walker) value(n *yaml.Node, t reflect.Type, path string) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case n.Kind == yaml.AliasNode:
		w.errorf(n, path, CodeUnsupported, "YAML aliases are not supported; write the value out")
		return nil
	case n.Anchor != "":
		w.errorf(n, path, CodeUnsupported, "YAML anchors are not supported")
		return nil
	case n.Style&yaml.TaggedStyle != 0:
		w.errorf(n, path, CodeUnsupported, "YAML tags such as %s are not supported", n.Tag)
		return nil
	case n.Kind == yaml.ScalarNode && n.ShortTag() == tagNull:
		w.errorf(n, path, CodeNullNotAllowed, "null is not allowed; remove the key instead")
		return nil
	}
	if t == durationType {
		if n.Kind != yaml.ScalarNode {
			return w.wrongType(n, t, path)
		}
		if _, err := time.ParseDuration(n.Value); err != nil {
			w.errorf(n, path, CodeBadValue, "invalid duration %q: write it like \"30s\" or \"2m\"", n.Value)
			return nil
		}
		return n.Value
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind == yaml.ScalarNode && t.Implements(shorthandType) {
			field := reflect.Zero(t).Interface().(shorthand).shorthandField()
			return map[string]any{field: n.Value}
		}
		if n.Kind != yaml.MappingNode {
			return w.wrongType(n, t, path)
		}
		return w.object(n, t, path)
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return w.wrongType(n, t, path)
		}
		out := map[string]any{}
		w.pairs(n, path, func(key string, k, v *yaml.Node) {
			out[key] = w.value(v, t.Elem(), joinPath(path, key))
		})
		return out
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return w.wrongType(n, t, path)
		}
		out := make([]any, 0, len(n.Content))
		for i, item := range n.Content {
			out = append(out, w.value(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)))
		}
		return out
	}
	return w.scalar(n, t, path)
}

// scalar checks a scalar of a string, boolean or number type.
func (w walker) scalar(n *yaml.Node, t reflect.Type, path string) any {
	switch t.Kind() {
	case reflect.String:
		if n.Kind != yaml.ScalarNode {
			return w.wrongType(n, t, path)
		}
		return n.Value
	case reflect.Bool:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != tagBool {
			return w.wrongType(n, t, path)
		}
		var b bool
		if err := n.Decode(&b); err != nil {
			return w.wrongType(n, t, path)
		}
		return b
	case reflect.Int, reflect.Int64:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != tagInt {
			return w.wrongType(n, t, path)
		}
		var i int64
		if err := n.Decode(&i); err != nil {
			return w.wrongType(n, t, path)
		}
		return i
	case reflect.Float64:
		if n.Kind != yaml.ScalarNode || (n.ShortTag() != tagInt && n.ShortTag() != tagFloat) {
			return w.wrongType(n, t, path)
		}
		var f float64
		if err := n.Decode(&f); err != nil {
			return w.wrongType(n, t, path)
		}
		return f
	}
	w.errorf(n, path, CodeWrongType, "unsupported field type %s", t)
	return nil
}

func (w walker) wrongType(n *yaml.Node, t reflect.Type, path string) any {
	w.errorf(n, path, CodeWrongType, "want %s, got %s", wantName(t), kindName(n))
	return nil
}

// pairs calls fn for each key of a mapping, rejecting duplicate, non-string
// and merge keys.
func (w walker) pairs(n *yaml.Node, path string, fn func(key string, k, v *yaml.Node)) {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.ShortTag() == tagNull {
			w.errorf(k, path, CodeWrongType, "keys must be strings")
			continue
		}
		if k.ShortTag() == tagMerge || k.Value == "<<" {
			w.errorf(k, path, CodeUnsupported, "YAML merge keys are not supported")
			continue
		}
		if seen[k.Value] {
			w.errorf(k, joinPath(path, k.Value), CodeDuplicateKey, "duplicate key %q", k.Value)
			continue
		}
		seen[k.Value] = true
		fn(k.Value, k, v)
	}
}

// fieldNames lists a struct's JSON field names, in declaration order.
func fieldNames(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

func (w walker) object(n *yaml.Node, t reflect.Type, path string) any {
	fields := fieldNames(t)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	out := map[string]any{}
	w.pairs(n, path, func(key string, k, v *yaml.Node) {
		ft, ok := fields[key]
		if !ok {
			msg := fmt.Sprintf("unknown key %q", key)
			if s := suggest(key, names); s != "" {
				msg += fmt.Sprintf("; did you mean %q?", s)
			}
			w.errorf(k, joinPath(path, key), CodeUnknownKey, "%s", msg)
			return
		}
		out[key] = w.value(v, ft, joinPath(path, key))
	})
	return out
}

// addLines fills in the line of each issue whose path names a key in the
// header, for issues found after decoding.
func addLines(list []Issue, header string, firstLine int) {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(header), &doc) != nil || len(doc.Content) == 0 {
		return
	}
	for i := range list {
		if list[i].Line != 0 || list[i].Path == "" {
			continue
		}
		if n := lookupPath(doc.Content[0], list[i].Path); n != nil {
			list[i].Line = n.Line + firstLine - 1
		}
	}
}

var indexPart = regexp.MustCompile(`^([^\[]*)((?:\[\d+\])*)$`)

// lookupPath finds the node a path such as "skills[1].mode" names, or the
// nearest ancestor that exists.
func lookupPath(n *yaml.Node, path string) *yaml.Node {
	cur := n
	for part := range strings.SplitSeq(path, ".") {
		m := indexPart.FindStringSubmatch(part)
		if m == nil {
			return cur
		}
		if m[1] != "" {
			next := mappingValue(cur, m[1])
			if next == nil {
				return cur
			}
			cur = next
		}
		for _, idx := range regexp.MustCompile(`\d+`).FindAllString(m[2], -1) {
			i, _ := strconv.Atoi(idx)
			if cur.Kind != yaml.SequenceNode || i >= len(cur.Content) {
				return cur
			}
			cur = cur.Content[i]
		}
	}
	return cur
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
