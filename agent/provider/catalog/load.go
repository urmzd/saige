package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ErrInvalidCatalog matches every *ValidationError.
var ErrInvalidCatalog = errors.New("invalid catalog")

// Severity says whether an issue rejects the catalog.
type Severity string

const (
	// SeverityError rejects the catalog.
	SeverityError Severity = "error"
	// SeverityWarning is reported but accepted. A strict validation run
	// treats warnings as errors.
	SeverityWarning Severity = "warning"
)

// Issue is one problem found while loading or validating a catalog. Path
// addresses the offending value, for example
// "presets.balanced.chain[2].options.reasoning.budget".
type Issue struct {
	Path     string   `json:"path"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
}

func (i Issue) String() string {
	if i.Path == "" {
		return i.Message
	}
	return i.Path + ": " + i.Message
}

// Issue codes. Tests and tools match on these rather than on messages.
const (
	CodeSyntax           = "syntax"
	CodeUnknownKey       = "unknown_key"
	CodeSecretKey        = "secret_key"
	CodeDuplicateKey     = "duplicate_key"
	CodeWrongType        = "wrong_type"
	CodeNullNotAllowed   = "null_not_allowed"
	CodeTrailingData     = "trailing_data"
	CodeVersion          = "unsupported_version"
	CodeBadDuration      = "bad_duration"
	CodeUnknownCap       = "unknown_capability"
	CodeUnknownMedia     = "unknown_media_type"
	CodeUnknownTool      = "unknown_server_tool"
	CodeBadValue         = "invalid_value"
	CodeMissing          = "missing_field"
	CodeUnknownTemplate  = "unknown_template"
	CodeExtendsCycle     = "extends_cycle"
	CodeExtendsDepth     = "extends_depth"
	CodeDuplicateRow     = "duplicate_row"
	CodeUnknownSuccessor = "unknown_successor"
	CodeSuccessorCycle   = "successor_cycle"
	CodePricing          = "invalid_pricing"
	CodeUnknownPreset    = "unknown_preset"
	CodePresetCycle      = "preset_cycle"
	CodeEmptyChain       = "empty_chain"
	CodeDuplicateEntry   = "duplicate_entry"
	CodeUnset            = "invalid_unset"
	CodeUnsupported      = "unsupported_option"
	CodeNotExpressible   = "not_expressible"
	CodeToolChoice       = "invalid_tool_choice"
	CodeOutputMode       = "invalid_output_mode"
	CodePromptCache      = "invalid_prompt_cache"
	CodeServerTool       = "invalid_server_tool"
	CodeNotDeclared      = "model_not_declared"
	CodeUntrusted        = "untrusted_field"
	CodeDial             = "invalid_dial"

	WarnInferredModel   = "inferred_model"
	WarnSmallerWindow   = "smaller_context_window"
	WarnSuperseded      = "superseded_model"
	WarnUnpriced        = "unpriced_model"
	WarnSignedReasoning = "signed_reasoning_failover"
	WarnExceedsWindow   = "output_exceeds_context_window"
	// WarnDialMapped and WarnDialDropped report an advisory dial an entry's
	// model cannot honor exactly; WarnDialOverridden a dial a raw option
	// overrides. WarnPreferDial suggests a dial for a raw option a preset
	// shares across vendors.
	WarnDialMapped     = "dial_mapped"
	WarnDialDropped    = "dial_dropped"
	WarnDialOverridden = "dial_overridden"
	WarnPreferDial     = "prefer_dial"
)

// ValidationError carries every issue found in one catalog. errors.Is(err,
// ErrInvalidCatalog) matches it.
type ValidationError struct {
	// Source names the file or layer, when known.
	Source string
	Issues []Issue
}

// maxReportedIssues bounds Error's length; Issues holds all of them.
const maxReportedIssues = 8

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString(ErrInvalidCatalog.Error())
	if e.Source != "" {
		b.WriteString(" " + e.Source)
	}
	errs := e.Errors()
	if len(errs) == 0 {
		errs = e.Issues
	}
	for i, is := range errs {
		if i == maxReportedIssues {
			fmt.Fprintf(&b, "; and %d more", len(errs)-i)
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

// Is matches ErrInvalidCatalog.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidCatalog }

// Errors returns the issues with error severity.
func (e *ValidationError) Errors() []Issue { return filterIssues(e.Issues, SeverityError) }

// Warnings returns the issues with warning severity.
func (e *ValidationError) Warnings() []Issue { return filterIssues(e.Issues, SeverityWarning) }

func filterIssues(in []Issue, s Severity) []Issue {
	var out []Issue
	for _, is := range in {
		if is.Severity == s {
			out = append(out, is)
		}
	}
	return out
}

// issues collects problems with their paths.
type issues []Issue

func (l *issues) errorf(path, code, format string, args ...any) {
	*l = append(*l, Issue{Path: path, Code: code, Message: fmt.Sprintf(format, args...), Severity: SeverityError})
}

func (l *issues) warnf(path, code, format string, args ...any) {
	*l = append(*l, Issue{Path: path, Code: code, Message: fmt.Sprintf(format, args...), Severity: SeverityWarning})
}

func (l issues) hasErrors() bool {
	for _, is := range l {
		if is.Severity == SeverityError {
			return true
		}
	}
	return false
}

// asError returns a *ValidationError when any issue is an error.
func (l issues) asError(source string) error {
	if !l.hasErrors() {
		return nil
	}
	return &ValidationError{Source: source, Issues: append([]Issue(nil), l...)}
}

// Load strictly decodes one catalog layer and validates what the layer can
// prove on its own: syntax, key names, value kinds, capability and media
// names, durations, template cycles, and the secret and null rules. A layer
// that sets inherit_default false is standalone and is validated fully.
// References to templates, rows and presets in lower layers are checked
// when the layers are merged. Errors are a *ValidationError.
func Load(r io.Reader) (*Catalog, error) {
	return load(r, "")
}

// LoadFile is Load for a file, naming the path in errors.
func LoadFile(path string) (*Catalog, error) {
	f, err := os.Open(path) //nolint:gosec // the caller names the catalog file to read
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return load(f, path)
}

func load(r io.Reader, source string) (*Catalog, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var found issues
	v1 := fileVersion(data) == SchemaVersionV1
	root := reflect.TypeFor[Catalog]()
	if v1 {
		root = reflect.TypeFor[CatalogV1]()
	}
	lintJSON(data, root, &found)
	if found.hasErrors() {
		return nil, found.asError(source)
	}
	var c *Catalog
	if v1 {
		old := &CatalogV1{}
		if err := decodeStrict(data, old); err != nil {
			found.errorf("", CodeSyntax, "%v", err)
			return nil, found.asError(source)
		}
		if err := attachRawV1(old, data); err != nil {
			found.errorf("", CodeSyntax, "%v", err)
			return nil, found.asError(source)
		}
		var upgradeIssues []Issue
		c, upgradeIssues = UpgradeV1(old)
		for _, is := range upgradeIssues {
			if is.Severity == SeverityError {
				found = append(found, is)
			}
		}
	} else {
		c = &Catalog{}
		if err := decodeStrict(data, c); err != nil {
			found.errorf("", CodeSyntax, "%v", err)
			return nil, found.asError(source)
		}
		if err := attachRaw(c, data); err != nil {
			found.errorf("", CodeSyntax, "%v", err)
			return nil, found.asError(source)
		}
	}
	standalone := c.InheritDefault != nil && !*c.InheritDefault
	found = append(found, c.check(standalone)...)
	if err := found.asError(source); err != nil {
		return nil, err
	}
	return c, nil
}

// fileVersion reads the version key without validating anything else.
func fileVersion(data []byte) int {
	var v struct {
		Version json.Number `json:"version"`
	}
	if json.Unmarshal(data, &v) != nil {
		return 0
	}
	n, _ := v.Version.Int64()
	return int(n)
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	return dec.Decode(v)
}

// attachRaw keeps each model, template, endpoint and offering as written,
// and records presets deleted with null.
func attachRaw(c *Catalog, data []byte) error {
	var raw struct {
		ModelTemplates    map[string]json.RawMessage           `json:"model_templates"`
		Models            map[string]json.RawMessage           `json:"models"`
		Endpoints         map[string]json.RawMessage           `json:"endpoints"`
		OfferingTemplates map[string]json.RawMessage           `json:"offering_templates"`
		Offerings         []json.RawMessage                    `json:"offerings"`
		Presets           map[types.PresetName]json.RawMessage `json:"presets"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for name, m := range c.ModelTemplates {
		m.raw = raw.ModelTemplates[name]
		c.ModelTemplates[name] = m
	}
	for name, m := range c.Models {
		m.raw = raw.Models[name]
		c.Models[name] = m
	}
	for name, e := range c.Endpoints {
		e.raw = raw.Endpoints[name]
		c.Endpoints[name] = e
	}
	for name, o := range c.OfferingTemplates {
		o.raw = raw.OfferingTemplates[name]
		c.OfferingTemplates[name] = o
	}
	for i := range c.Offerings {
		if i < len(raw.Offerings) {
			c.Offerings[i].raw = raw.Offerings[i]
		}
	}
	c.deletedPresets = deletedPresets(raw.Presets)
	for _, name := range c.deletedPresets {
		delete(c.Presets, name)
	}
	return nil
}

// attachRawV1 keeps each v1 row as written, and records presets deleted
// with null.
func attachRawV1(c *CatalogV1, data []byte) error {
	var raw struct {
		Models    []json.RawMessage                    `json:"models"`
		Templates map[string]json.RawMessage           `json:"templates"`
		Baselines map[string]json.RawMessage           `json:"baselines"`
		Presets   map[types.PresetName]json.RawMessage `json:"presets"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for i := range c.Models {
		if i < len(raw.Models) {
			c.Models[i].raw = raw.Models[i]
		}
	}
	for name, m := range c.Templates {
		m.raw = raw.Templates[name]
		c.Templates[name] = m
	}
	for name, m := range c.Baselines {
		m.raw = raw.Baselines[name]
		c.Baselines[name] = m
	}
	c.deletedPresets = deletedPresets(raw.Presets)
	for _, name := range c.deletedPresets {
		delete(c.Presets, name)
	}
	return nil
}

func deletedPresets(raw map[types.PresetName]json.RawMessage) []types.PresetName {
	var out []types.PresetName
	for name, p := range raw {
		if isNullRaw(p) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// jsonNull is the literal a merge patch uses to delete a key.
const jsonNull = "null"

// ── Pass 1: token walk ──────────────────────────────────────────────

var durationType = reflect.TypeFor[Duration]()

// nullContext says where JSON null is accepted. Nulls are merge-patch
// deletions, so they are accepted only inside rows, templates and baselines,
// and as a whole preset. Options objects never accept null: removing an
// inherited option is written with unset.
type nullContext int

const (
	nullNever nullContext = iota
	nullPatch             // inside a model row, template or baseline
	nullValue             // this value itself may be null, its contents may not
)

type linter struct {
	dec    *json.Decoder
	issues *issues
	failed bool
}

// lintJSON walks the document against a catalog type's key schema.
func lintJSON(data []byte, root reflect.Type, found *issues) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	l := &linter{dec: dec, issues: found}
	l.value("", root, nullNever)
	if l.failed {
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		found.errorf("", CodeTrailingData, "unexpected data after the catalog object")
	}
}

func (l *linter) token() (json.Token, bool) {
	if l.failed {
		return nil, false
	}
	tok, err := l.dec.Token()
	if err != nil {
		l.issues.errorf("", CodeSyntax, "offset %d: %v", l.dec.InputOffset(), err)
		l.failed = true
		return nil, false
	}
	return tok, true
}

// skip consumes the rest of a value whose first token was tok.
func (l *linter) skip(tok json.Token) {
	d, ok := tok.(json.Delim)
	if !ok || (d != '{' && d != '[') {
		return
	}
	depth := 1
	for depth > 0 {
		t, ok := l.token()
		if !ok {
			return
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
}

func kindName(tok json.Token) string {
	switch v := tok.(type) {
	case json.Delim:
		if v == '{' {
			return "object"
		}
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "value"
}

func wantName(t reflect.Type) string {
	if t == durationType {
		return "duration string"
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Slice:
		return "array"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "integer"
	case reflect.Float64, reflect.Float32:
		return "number"
	}
	return t.Kind().String()
}

// value checks one value of type t at path.
func (l *linter) value(path string, t reflect.Type, nulls nullContext) {
	tok, ok := l.token()
	if !ok {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if tok == nil {
		if nulls == nullNever {
			l.issues.errorf(path, CodeNullNotAllowed, "null is not allowed here; remove the key, or use unset to drop an inherited option")
		}
		return
	}
	if nulls == nullValue {
		nulls = nullNever
	}
	mismatch := func() {
		l.issues.errorf(path, CodeWrongType, "want %s, got %s", wantName(t), kindName(tok))
		l.skip(tok)
	}
	if t == durationType {
		s, ok := tok.(string)
		if !ok {
			mismatch()
			return
		}
		if err := new(Duration).UnmarshalJSON([]byte(strconv.Quote(s))); err != nil {
			l.issues.errorf(path, CodeBadDuration, "invalid duration %q: want a Go duration such as \"500ms\" or \"2m\"", s)
		}
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			mismatch()
			return
		}
		l.object(path, t, nulls)
	case reflect.Map:
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			mismatch()
			return
		}
		l.mapObject(path, t, nulls)
	case reflect.Slice:
		if t == reflect.TypeFor[json.RawMessage]() {
			l.skip(tok)
			return
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			mismatch()
			return
		}
		for i := 0; l.dec.More() && !l.failed; i++ {
			l.value(fmt.Sprintf("%s[%d]", path, i), t.Elem(), elemNulls(t.Elem(), nulls))
		}
		l.token() // ]
	case reflect.String:
		if _, ok := tok.(string); !ok {
			mismatch()
		}
	case reflect.Bool:
		if _, ok := tok.(bool); !ok {
			mismatch()
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		n, ok := tok.(json.Number)
		if !ok {
			mismatch()
			return
		}
		if _, err := n.Int64(); err != nil {
			l.issues.errorf(path, CodeWrongType, "want integer, got %s", n)
		}
	case reflect.Float64, reflect.Float32:
		if _, ok := tok.(json.Number); !ok {
			mismatch()
		}
	default:
		l.skip(tok)
	}
}

// elemNulls is the null context of a slice element or map value.
func elemNulls(t reflect.Type, parent nullContext) nullContext {
	if parent == nullPatch {
		return nullNever // arrays replace whole; their items are values
	}
	return parent
}

func (l *linter) mapObject(path string, t reflect.Type, nulls nullContext) {
	seen := map[string]bool{}
	for l.dec.More() && !l.failed {
		tok, ok := l.token()
		if !ok {
			return
		}
		key, _ := tok.(string)
		child := joinPath(path, key)
		if seen[key] {
			l.issues.errorf(child, CodeDuplicateKey, "duplicate key %q", key)
		}
		seen[key] = true
		l.value(child, t.Elem(), mapValueNulls(path, t.Elem(), nulls))
	}
	l.token() // }
}

// mapValueNulls lets a template, baseline or preset be removed with null.
func mapValueNulls(path string, elem reflect.Type, parent nullContext) nullContext {
	switch {
	case slices.Contains([]string{"templates", "baselines", "model_templates", "models", "endpoints", "offering_templates"}, path):
		return nullValue
	case path == "presets" && elem == reflect.TypeFor[PresetSpec]():
		return nullValue
	case parent == nullPatch:
		return nullPatch
	}
	return nullNever
}

// fieldsOf maps json names to field types.
func fieldsOf(t reflect.Type) (map[string]reflect.Type, []string) {
	fields := map[string]reflect.Type{}
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
		names = append(names, name)
	}
	return fields, names
}

// secretKeys are rejected with a pointer to secret references.
var secretKeys = map[string]bool{"api_key": true, "apikey": true, "api-key": true, "key_value": true, "secret": true, "token": true}

func (l *linter) object(path string, t reflect.Type, nulls nullContext) {
	fields, names := fieldsOf(t)
	seen := map[string]bool{}
	// A row patch may null any key, except inside its options defaults.
	rowLike := t == reflect.TypeFor[ModelSpec]() || t == reflect.TypeFor[ModelSpecV1]() ||
		t == reflect.TypeFor[OfferingSpec]() || t == reflect.TypeFor[EndpointSpec]()
	for l.dec.More() && !l.failed {
		tok, ok := l.token()
		if !ok {
			return
		}
		key, _ := tok.(string)
		child := joinPath(path, key)
		if seen[key] {
			l.issues.errorf(child, CodeDuplicateKey, "duplicate key %q", key)
		}
		seen[key] = true
		ft, known := fields[key]
		if !known {
			switch {
			case secretKeys[strings.ToLower(key)]:
				l.issues.errorf(child, CodeSecretKey, "secrets are not part of a catalog; reference one with an endpoint's auth.secret (\"env:NAME\") or an entry's api_key_env")
			default:
				msg := fmt.Sprintf("unknown key %q", key)
				if s := suggest(key, names); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				}
				l.issues.errorf(child, CodeUnknownKey, "%s", msg)
			}
			next, ok := l.token()
			if ok {
				l.skip(next)
			}
			continue
		}
		childNulls := nullNever
		if rowLike && key != "defaults" {
			childNulls = nullPatch
		} else if nulls == nullPatch && t != reflect.TypeFor[OptionsSpec]() {
			childNulls = nullPatch
		}
		l.value(child, ft, childNulls)
	}
	l.token() // }
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// suggest returns the closest candidate within a small edit distance.
func suggest(key string, candidates []string) string {
	best, bestD := "", len(key)/2+2
	for _, c := range candidates {
		if d := levenshtein(key, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
