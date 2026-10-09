package research

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// FileSearchTool implements types.Tool for searching local file contents.
type FileSearchTool struct {
	root string
}

func NewFileSearchTool(root string) *FileSearchTool {
	return &FileSearchTool{root: root}
}

func (t *FileSearchTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "file_search",
		Capability:  types.ToolCapabilityRead,
		Description: "Search local file contents for a regex pattern. Returns matching lines with file paths and line numbers. Use this to explore codebases, config files, logs, or any local text files.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"pattern"},
			Properties: map[string]types.PropertyDef{
				"pattern": {Type: types.SchemaString, Description: "Regex pattern to search for"},
				argPath:   {Type: types.SchemaString, Description: "Directory to search in (default: working directory)"},
				"glob":    {Type: types.SchemaString, Description: "File glob filter, e.g. '*.go' or '*.md' (default: all text files)"},
			},
		},
	}
}

const (
	// maxSearchFileBytes skips files larger than this; most are binary or
	// generated and would drown the result.
	maxSearchFileBytes = 1 << 20
	// maxSearchLineChars caps each printed match.
	maxSearchLineChars = 500
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"__pycache__": true, ".venv": true, "dist": true, "build": true,
}

//nolint:gocyclo // a single pass over a stream or loop state; splitting it would scatter shared state
func (t *FileSearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("file_search: pattern is required")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("file_search: invalid regex: %w", err)
	}

	// Confine the search root to the configured root. A "path" arg may scope
	// the search to a subdirectory, but it cannot escape the root via ../ or an
	// absolute path, and symlinks that escape the root are rejected.
	var root string
	if p, ok := args[argPath].(string); ok && p != "" {
		resolved, err := resolveWithinRoot(t.root, p, true)
		if err != nil {
			return "", fmt.Errorf("file_search: %w", err)
		}
		root = resolved
	} else {
		resolved, err := resolveWithinRoot(t.root, ".", false)
		if err != nil {
			return "", fmt.Errorf("file_search: %w", err)
		}
		root = resolved
	}

	globPattern, _ := args["glob"].(string)

	const maxMatches = 50
	var b strings.Builder
	matches := 0
	// skipped records files that could not be searched to the end, so an
	// empty or short result is not mistaken for proof that nothing matches.
	var skipped []string

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			skipped = append(skipped, relTo(root, path)+": "+err.Error())
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if matches >= maxMatches {
			return filepath.SkipAll
		}

		if globPattern != "" {
			if ok, _ := filepath.Match(globPattern, d.Name()); !ok {
				return nil
			}
		}

		// Reject files whose real path escapes root (e.g. a symlink inside root
		// pointing at /etc/passwd). WalkDir does not descend symlinked dirs, but
		// symlinked files would otherwise be opened.
		if d.Type()&os.ModeSymlink != 0 {
			if _, err := resolveWithinRoot(root, path, true); err != nil {
				return nil
			}
		}

		// Skip binary/large files
		info, err := d.Info()
		if err != nil || info.Size() > maxSearchFileBytes {
			return nil
		}

		relPath := relTo(root, path)
		f, err := os.Open(path) //nolint:gosec // path comes from filepath.WalkDir of a user-scoped root
		if err != nil {
			skipped = append(skipped, relPath+": "+err.Error())
			return nil
		}
		defer func() { _ = f.Close() }()

		scanner := bufio.NewScanner(f)
		// Files are capped at maxSearchFileBytes, so any line fits; the default
		// 64KB token limit would silently stop matching at the first long line.
		scanner.Buffer(make([]byte, 0, 64*1024), maxSearchFileBytes+1)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", relPath, lineNum, truncateLine(line, maxSearchLineChars))
				matches++
				if matches >= maxMatches {
					break
				}
			}
		}
		if err := scanner.Err(); err != nil {
			skipped = append(skipped, relPath+": "+err.Error())
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("file_search: %w", ctxErr)
		}
		skipped = append(skipped, walkErr.Error())
	}

	if b.Len() == 0 {
		b.WriteString("No matches found.\n")
	} else if matches >= maxMatches {
		fmt.Fprintf(&b, "\n(showing first %d matches)\n", maxMatches)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "(skipped %d files: %s)\n", len(skipped), summarizeSkipped(skipped, 5))
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// relTo returns path relative to root, or path itself when it is not under root.
func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" {
		return path
	}
	return rel
}

// summarizeSkipped lists up to max entries and counts the rest.
func summarizeSkipped(skipped []string, max int) string {
	if len(skipped) <= max {
		return strings.Join(skipped, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(skipped[:max], "; "), len(skipped)-max)
}
