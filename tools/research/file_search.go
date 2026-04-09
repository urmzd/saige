package research

import (
	"bufio"
	"context"
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
		Description: "Search local file contents for a regex pattern. Returns matching lines with file paths and line numbers. Use this to explore codebases, config files, logs, or any local text files.",
		Parameters: types.ParameterSchema{
			Type:     "object",
			Required: []string{"pattern"},
			Properties: map[string]types.PropertyDef{
				"pattern": {Type: "string", Description: "Regex pattern to search for"},
				"path":    {Type: "string", Description: "Directory to search in (default: working directory)"},
				"glob":    {Type: "string", Description: "File glob filter, e.g. '*.go' or '*.md' (default: all text files)"},
			},
		},
	}
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"__pycache__": true, ".venv": true, "dist": true, "build": true,
}

func (t *FileSearchTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("file_search: pattern is required")
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("file_search: invalid regex: %w", err)
	}

	root := t.root
	if p, ok := args["path"].(string); ok && p != "" {
		root = p
		if !filepath.IsAbs(root) {
			root = filepath.Join(t.root, root)
		}
	}

	globPattern, _ := args["glob"].(string)

	const maxMatches = 50
	var b strings.Builder
	matches := 0

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return err
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

		// Skip binary/large files
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 { // 1MB
			return nil
		}

		f, err := os.Open(path) //nolint:gosec // path comes from filepath.WalkDir of a user-scoped root
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		relPath, _ := filepath.Rel(root, path)
		if relPath == "" {
			relPath = path
		}

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", relPath, lineNum, line)
				matches++
				if matches >= maxMatches {
					break
				}
			}
		}
		return nil
	})

	if b.Len() == 0 {
		return "No matches found.", nil
	}

	if matches >= maxMatches {
		fmt.Fprintf(&b, "\n(showing first %d matches)\n", maxMatches)
	}
	return b.String(), nil
}
