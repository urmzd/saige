package fs

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

type grepTool struct{ cfg *config }

func (t *grepTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "grep",
		Capability: types.ToolCapabilityRead,
		Description: "Search workspace text files for a regular expression (RE2 syntax). " +
			"Returns path:line: text for each matching line. Binary and oversized files are skipped.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPattern},
			Properties: map[string]types.PropertyDef{
				argPattern:    {Type: types.SchemaString, Description: "Regular expression to search for."},
				argPath:       {Type: types.SchemaString, Description: "File or directory to search, relative to the workspace root (default: the root)."},
				"glob":        {Type: types.SchemaString, Description: "Only search files whose path matches this glob, for example \"**/*.go\". A pattern without a slash matches the file name."},
				"ignore_case": {Type: types.SchemaBoolean, Description: "Match case-insensitively."},
			},
		},
	}
}

func (t *grepTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	expr := stringArg(args, argPattern)
	if expr == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}
	if boolArg(args, "ignore_case") {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	glob := stringArg(args, "glob")
	base, err := t.cfg.resolve(dirArg(args), true)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}

	var b strings.Builder
	matches, skipped := 0, 0
	search := func(p string) error {
		if glob != "" && !globSelects(glob, base, p) {
			return nil
		}
		n, ok, err := t.searchFile(ctx, re, p, &b, t.cfg.maxResults-matches)
		if err != nil {
			return err
		}
		if !ok {
			skipped++
		}
		matches += n
		if matches >= t.cfg.maxResults {
			return errStopWalk
		}
		return nil
	}

	info, err := os.Stat(base)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	if info.IsDir() {
		err = walkFiles(ctx, base, search)
	} else if err = search(base); err == errStopWalk {
		err = nil
	}
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}

	if matches == 0 {
		return "No matches found.", nil
	}
	if matches >= t.cfg.maxResults {
		fmt.Fprintf(&b, "... stopped at %d matches; narrow the pattern, path, or glob\n", t.cfg.maxResults)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "(%d binary, oversized, or unreadable files skipped)\n", skipped)
	}
	return b.String(), nil
}

// searchFile writes up to budget matching lines of p to b. It returns the
// number written and false when the file was skipped.
func (t *grepTool) searchFile(ctx context.Context, re *regexp.Regexp, p string, b *strings.Builder, budget int) (int, bool, error) {
	if ctx.Err() != nil {
		return 0, true, ctx.Err()
	}
	f, err := openText(p, t.cfg.maxFileBytes)
	if err != nil {
		return 0, false, nil
	}
	defer func() { _ = f.Close() }()

	rel := t.cfg.rel(p)
	sc := newScanner(f, t.cfg.maxFileBytes)
	n, line := 0, 0
	for sc.Scan() && n < budget {
		line++
		text := sc.Text()
		if re.MatchString(text) {
			fmt.Fprintf(b, "%s:%d: %s\n", rel, line, truncateLine(text, t.cfg.maxLineChars))
			n++
		}
	}
	if sc.Err() != nil {
		return n, false, nil
	}
	return n, true, nil
}

// globSelects reports whether file p under base matches glob. A pattern
// with no slash is matched against the file name alone.
func globSelects(glob, base, p string) bool {
	if !strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, filepath.Base(p))
		return ok
	}
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return matchGlob(glob, filepath.ToSlash(rel))
}
