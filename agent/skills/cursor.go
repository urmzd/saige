package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Cursor rule locations, relative to a project directory.
const (
	CursorRulesDir    = ".cursor/rules"
	CursorRulesLegacy = ".cursorrules"
	cursorRuleExt     = ".mdc"
)

// Metadata keys a skill made from a Cursor rule carries, so the rule's
// activation survives in the skill and in an import.
const (
	MetaCursorRule        = "cursor-rule"         // the rule's path, relative to the project
	MetaCursorGlobs       = "cursor-globs"        // comma-separated globs the rule attaches to
	MetaCursorAlwaysApply = "cursor-always-apply" // "true" for a rule Cursor always applies
)

// CursorRulesSource lists a project's Cursor rules as skills: every .mdc
// file under .cursor/rules, at any depth, then the legacy .cursorrules
// file.
//
// A rule becomes a skill that loads on demand, like any other. Its name is
// its path under .cursor/rules without the extension, with each directory
// joined by a hyphen (frontend/react.mdc is frontend-react); .cursorrules
// is cursorrules. Its description is the rule's description, followed by
// when Cursor applies it: always, or for files matching its globs. A rule
// with no description is described by its path. The globs and alwaysApply
// fields are kept in the skill's metadata (MetaCursorGlobs,
// MetaCursorAlwaysApply).
//
// Files are read through os.Root, so a symbolic link that leaves the
// project rejects that rule.
type CursorRulesSource struct {
	project string
	cfg     sourceConfig
}

// NewCursorRulesSource returns a source for the rules in projectDir. Its
// default name is projectDir/.cursor/rules.
func NewCursorRulesSource(projectDir string, opts ...SourceOption) *CursorRulesSource {
	cfg := sourceConfig{name: filepath.Join(projectDir, filepath.FromSlash(CursorRulesDir))}
	for _, o := range opts {
		o(&cfg)
	}
	return &CursorRulesSource{project: projectDir, cfg: cfg}
}

// Name implements SkillSource.
func (s *CursorRulesSource) Name() string { return s.cfg.name }

// List implements SkillSource.
func (s *CursorRulesSource) List(ctx context.Context) ([]Skill, error) {
	root, err := os.OpenRoot(s.project)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill source %s: %w", s.cfg.name, err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	var paths []string
	err = fs.WalkDir(fsys, CursorRulesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, cursorRuleExt) {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("skill source %s: %w", s.cfg.name, err)
	}
	if _, err := fs.Stat(fsys, CursorRulesLegacy); err == nil {
		paths = append(paths, CursorRulesLegacy)
	}

	opts := SnapshotOptions{Source: s.cfg.name, Trusted: !s.cfg.untrusted, Bounds: s.cfg.bounds}
	bounds := opts.Bounds.withDefaults()
	var (
		skills []Skill
		errs   []error
	)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		skill, err := readCursorRule(fsys, p, bounds, opts)
		if err != nil {
			errs = append(errs, &SkillError{Source: s.cfg.name, Dir: p, Err: err})
			continue
		}
		skills = append(skills, skill)
	}
	return skills, errors.Join(errs...)
}

func readCursorRule(fsys fs.FS, p string, bounds Bounds, opts SnapshotOptions) (Skill, error) {
	info, err := fs.Stat(fsys, p) // follows a link, within the root
	if err != nil {
		return Skill{}, fmt.Errorf("%w: %w", ErrOutsideSkillRoot, err)
	}
	if !info.Mode().IsRegular() {
		return Skill{}, fmt.Errorf("%w: not a regular file", ErrInvalidSkill)
	}
	data, err := readBounded(fsys, p, bounds.MaxFileBytes)
	if err != nil {
		return Skill{}, err
	}
	return CursorRule(p, data, opts)
}

// CursorRule converts one Cursor rule file into a skill. rel is the file's
// slash-separated path relative to the project: a file under .cursor/rules
// or .cursorrules. The skill's hash covers the rule file as it is.
func CursorRule(rel string, data []byte, opts SnapshotOptions) (Skill, error) {
	rel = path.Clean(filepath.ToSlash(rel))
	name, err := cursorRuleName(rel)
	if err != nil {
		return Skill{}, err
	}
	rule := parseCursorRule(data)
	if rel == CursorRulesLegacy {
		rule.alwaysApply = true // Cursor sends .cursorrules with every request
	}

	desc := rule.description
	if desc == "" {
		desc = "Cursor rule " + rel
	}
	if !strings.HasSuffix(desc, ".") && !strings.HasSuffix(desc, "!") && !strings.HasSuffix(desc, "?") {
		desc += "."
	}
	meta := map[string]string{MetaCursorRule: rel}
	switch {
	case rule.alwaysApply:
		desc += " Cursor applies it to every request: load it before starting work."
		meta[MetaCursorAlwaysApply] = "true"
	case len(rule.globs) > 0:
		desc += " Cursor applies it to files matching " + strings.Join(rule.globs, ", ") + "."
	}
	if len(rule.globs) > 0 {
		meta[MetaCursorGlobs] = strings.Join(rule.globs, ",")
	}

	sm, err := metaFromFrontmatter(Frontmatter{"name": name, "description": desc, "metadata": meta})
	if err != nil {
		return Skill{}, err
	}
	return sealSkill(sm, rule.body, map[string][]byte{SkillFile: bytes.Clone(data)}, opts), nil
}

// cursorRuleName derives a skill name from a rule's path.
func cursorRuleName(rel string) (string, error) {
	var stem string
	switch {
	case rel == CursorRulesLegacy:
		stem = "cursorrules"
	case strings.HasPrefix(rel, CursorRulesDir+"/"):
		stem = strings.TrimSuffix(strings.TrimPrefix(rel, CursorRulesDir+"/"), cursorRuleExt)
	default:
		return "", fmt.Errorf("%w: %s is not under %s", ErrInvalidSkill, rel, CursorRulesDir)
	}
	name := SkillName(stem)
	if name == "" {
		return "", fmt.Errorf("%w: no skill name can be made from %q", ErrInvalidSkill, rel)
	}
	return name, nil
}

// SkillName folds s into a valid skill name: lowercase letters and digits,
// with every other run of characters replaced by one hyphen, at most
// MaxNameLength characters. It returns "" when nothing is left.
func SkillName(s string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			hyphen = false
			continue
		}
		hyphen = true
	}
	name := b.String()
	if len(name) > MaxNameLength {
		name = strings.TrimRight(name[:MaxNameLength], "-")
	}
	return name
}

type cursorRuleFile struct {
	description string
	globs       []string
	alwaysApply bool
	body        string
}

// parseCursorRule reads an .mdc header leniently, the way Cursor does: its
// globs are often unquoted values such as *.ts that strict YAML reads as
// an alias. Only description, globs and alwaysApply are read; a file with
// no header is all body.
func parseCursorRule(data []byte) cursorRuleFile {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	var out cursorRuleFile
	if !strings.HasPrefix(text, "---\n") {
		out.body = strings.TrimLeft(text, "\n")
		return out
	}
	rest := text[len("---\n"):]
	var header string
	switch end := strings.Index(rest, "\n---\n"); {
	case strings.HasPrefix(rest, "---\n") || rest == "---":
		out.body = strings.TrimPrefix(rest, "---")
	case end >= 0:
		header, out.body = rest[:end], rest[end+len("\n---\n"):]
	case strings.HasSuffix(rest, "\n---"):
		header = strings.TrimSuffix(rest, "\n---")
	default:
		// No closing line: Cursor shows the whole file.
		out.body = strings.TrimLeft(text, "\n")
		return out
	}
	out.body = strings.TrimLeft(out.body, "\n")

	key := ""
	for _, raw := range strings.Split(header, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if raw[0] == ' ' || raw[0] == '\t' {
			// A block list item continues the previous key.
			if key == "globs" && strings.HasPrefix(trimmed, "- ") {
				out.globs = append(out.globs, splitGlobs(strings.TrimPrefix(trimmed, "- "))...)
			}
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			key = ""
			continue
		}
		key, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch key {
		case "description":
			out.description = unquote(v)
		case "globs":
			v = strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
			out.globs = append(out.globs, splitGlobs(v)...)
		case "alwaysApply":
			out.alwaysApply = strings.EqualFold(unquote(v), "true")
		}
	}
	return out
}

func splitGlobs(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = unquote(strings.TrimSpace(g)); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		switch {
		case s[0] == '"' && s[len(s)-1] == '"':
			if u, err := strconv.Unquote(s); err == nil {
				return strings.TrimSpace(u)
			}
			return strings.TrimSpace(s[1 : len(s)-1])
		case s[0] == '\'' && s[len(s)-1] == '\'':
			return strings.TrimSpace(strings.ReplaceAll(s[1:len(s)-1], "''", "'"))
		}
	}
	return s
}
