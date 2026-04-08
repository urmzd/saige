package research

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// ReadFileTool implements types.Tool for reading local file contents.
type ReadFileTool struct {
	root string
}

func NewReadFileTool(root string) *ReadFileTool {
	return &ReadFileTool{root: root}
}

func (t *ReadFileTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "read_file",
		Description: "Read the contents of a local file. Returns the file content with line numbers. Use this to examine specific files found via file_search or known paths.",
		Parameters: types.ParameterSchema{
			Type:     "object",
			Required: []string{"path"},
			Properties: map[string]types.PropertyDef{
				"path":   {Type: "string", Description: "File path to read (absolute or relative to working directory)"},
				"offset": {Type: "number", Description: "Line number to start from (default: 1)"},
				"limit":  {Type: "number", Description: "Max lines to return (default: 200)"},
			},
		},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("read_file: path is required")
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(t.root, path)
	}

	offset := 1
	if o, ok := args["offset"].(float64); ok && o > 0 {
		offset = int(o)
	}

	limit := 200
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file: %s is a directory, not a file", path)
	}
	if info.Size() > 5<<20 { // 5MB
		return "", fmt.Errorf("read_file: file too large (%d bytes), max 5MB", info.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	lineNum := 0
	linesWritten := 0

	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if linesWritten >= limit {
			fmt.Fprintf(&b, "\n... (truncated at %d lines, use offset=%d to continue)\n", limit, lineNum)
			break
		}
		fmt.Fprintf(&b, "%4d | %s\n", lineNum, scanner.Text())
		linesWritten++
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	if b.Len() == 0 {
		return "File is empty.", nil
	}
	return b.String(), nil
}
