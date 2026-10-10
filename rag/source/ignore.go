package source

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ignoreRule is one pattern line of a .gitignore-style file.
type ignoreRule struct {
	pattern  string // doublestar glob, relative to the ignore file's directory
	negate   bool   // a leading "!" re-includes a match
	dirOnly  bool   // a trailing "/" matches directories only
	anchored bool   // the pattern contains a "/", so it matches from the base, not any depth
	contents bool   // the pattern ends in "/**", which matches below a directory but not the directory itself
}

// ignoreSet holds the rules of every ignore file read so far, keyed by the
// slash-separated directory (relative to the walk root, "." for the root)
// that holds them.
type ignoreSet map[string][]ignoreRule

// load reads the ignore files of dir, whose path relative to the root is
// rel. A missing file is not an error.
func (s ignoreSet) load(dir, rel string, limit int64) error {
	for _, name := range IgnoreFiles {
		p := filepath.Join(dir, name)
		data, err := readFile(p, limit)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		s[rel] = append(s[rel], parseIgnore(data)...)
	}
	return nil
}

// ignored reports whether rel, a slash-separated path below the root, is
// ignored. As in git, the last matching rule decides, and rules in a deeper
// directory come after those of its parents. Rules of ignore files in
// directories the walk skipped were never loaded, which matches git: a file
// cannot be re-included when a parent directory is excluded.
func (s ignoreSet) ignored(rel string, isDir bool) bool {
	if len(s) == 0 {
		return false
	}
	dirs := []string{"."}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			dirs = append(dirs, rel[:i])
		}
	}
	ignored := false
	for _, base := range dirs {
		rules := s[base]
		if len(rules) == 0 {
			continue
		}
		sub := rel
		if base != "." {
			sub = rel[len(base)+1:]
		}
		for _, r := range rules {
			if r.matches(sub, isDir) {
				ignored = !r.negate
			}
		}
	}
	return ignored
}

// matches reports whether the rule matches sub, a path relative to the
// rule's ignore file.
func (r ignoreRule) matches(sub string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	target := sub
	if !r.anchored {
		target = path.Base(sub)
	}
	ok, err := doublestar.Match(r.pattern, target)
	if err != nil || !ok {
		return false
	}
	if r.contents {
		// doublestar lets "dir/**" match "dir" itself; git does not.
		if self, _ := doublestar.Match(strings.TrimSuffix(r.pattern, "/**"), target); self {
			return false
		}
	}
	return true
}

// parseIgnore parses gitignore syntax: blank lines and "#" comments are
// skipped, "\" escapes a leading "#" or "!" and trailing spaces, "!"
// negates, a trailing "/" matches directories only, and a "/" anywhere else
// anchors the pattern to the file's directory. Invalid patterns are dropped,
// as git drops them.
func parseIgnore(data []byte) []ignoreRule {
	var rules []ignoreRule
	for _, line := range bytes.Split(data, []byte("\n")) {
		text := strings.TrimSuffix(string(line), "\r")
		text = trimTrailingSpaces(text)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var r ignoreRule
		switch {
		case strings.HasPrefix(text, "!"):
			r.negate = true
			text = text[1:]
		case strings.HasPrefix(text, `\!`), strings.HasPrefix(text, `\#`):
			text = text[1:]
		}
		if strings.HasSuffix(text, "/") {
			r.dirOnly = true
			text = strings.TrimRight(text, "/")
		}
		if text == "" {
			continue
		}
		if strings.Contains(text, "/") {
			r.anchored = true
			text = strings.TrimPrefix(text, "/")
		}
		r.contents = strings.HasSuffix(text, "/**")
		if text == "" || !doublestar.ValidatePattern(text) {
			continue
		}
		r.pattern = text
		rules = append(rules, r)
	}
	return rules
}

// trimTrailingSpaces drops trailing spaces that are not escaped with "\".
// An escaped space stays escaped, which doublestar reads as a literal space.
func trimTrailingSpaces(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}
