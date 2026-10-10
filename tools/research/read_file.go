package research

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/urmzd/saige/agent/types"
)

const (
	// maxReadFileBytes is the largest file read_file opens.
	maxReadFileBytes = 5 << 20
	// maxReadFileLineChars caps each emitted line so one long line cannot fill
	// the model's context window.
	maxReadFileLineChars = 2000
)

// ReadFileTool implements types.Tool for reading local file contents.
type ReadFileTool struct {
	root string
}

// NewReadFileTool returns a tool that reads files under root.
func NewReadFileTool(root string) *ReadFileTool {
	return &ReadFileTool{root: root}
}

// Definition implements types.Tool.
func (t *ReadFileTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "read_file",
		Capability:  types.ToolCapabilityRead,
		Description: "Read the contents of a local file. Returns the file content with line numbers. Use this to examine specific files found via file_search or known paths.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPath},
			Properties: map[string]types.PropertyDef{
				argPath:  {Type: types.SchemaString, Description: "File path to read, relative to the configured root directory. Paths that escape the root are rejected."},
				"offset": {Type: types.SchemaNumber, Description: "Line number to start from (default: 1)"},
				"limit":  {Type: types.SchemaNumber, Description: "Max lines to return (default: 200)"},
			},
		},
	}
}

// Execute implements types.Tool.
func (t *ReadFileTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	rawPath, _ := args[argPath].(string)
	if rawPath == "" {
		return "", fmt.Errorf("read_file: path is required")
	}

	// Confine reads to the configured root. Rejects ../ traversal, absolute
	// paths outside root, and symlinks whose target escapes root.
	path, err := resolveWithinRoot(t.root, rawPath, true)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
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
	if info.Size() > maxReadFileBytes {
		return "", fmt.Errorf("read_file: file too large (%d bytes), max 5MB", info.Size())
	}

	f, err := os.Open(path) //nolint:gosec // intentional: tool's purpose is reading user-specified files
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	defer func() { _ = f.Close() }()

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	// The default 64KB token limit fails the whole read on one long line, such
	// as minified JS or a lockfile. The file cap bounds the buffer instead.
	scanner.Buffer(make([]byte, 0, 64*1024), maxReadFileBytes+1)
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
		fmt.Fprintf(&b, "%4d | %s\n", lineNum, truncateLine(scanner.Text(), maxReadFileLineChars))
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

// truncateLine shortens s to at most max bytes, cut on a rune boundary, with a
// marker saying how much was dropped.
func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(%d more chars)", s[:cut], len(s)-cut)
}
