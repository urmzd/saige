package fs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// sniffBytes is how much of a file is checked for NUL bytes before it is
// treated as text.
const sniffBytes = 8 << 10

type readTool struct{ cfg *config }

func (t *readTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "read",
		Capability: types.ToolCapabilityRead,
		Description: "Read a text file in the workspace. Returns numbered lines. " +
			"Long files are returned one page at a time; pass offset to read the next page.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPath},
			Properties: map[string]types.PropertyDef{
				argPath:  {Type: types.SchemaString, Description: descFilePath},
				"offset": {Type: types.SchemaInteger, Description: "First line to return, starting at 1 (default 1)."},
				"limit":  {Type: types.SchemaInteger, Description: fmt.Sprintf("Maximum lines to return (default %d).", t.cfg.readLimit)},
			},
		},
	}
}

func (t *readTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	path, err := t.cfg.resolve(stringArg(args, argPath), true)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	offset := intArg(args, "offset", 1)
	limit := intArg(args, "limit", t.cfg.readLimit)

	f, err := openText(path, t.cfg.maxFileBytes)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	defer func() { _ = f.Close() }()

	var b strings.Builder
	sc := newScanner(f, t.cfg.maxFileBytes)
	line, written := 0, 0
	for sc.Scan() {
		line++
		if line%4096 == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
		if line < offset || written >= limit {
			continue
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, truncateLine(sc.Text(), t.cfg.maxLineChars))
		written++
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	switch {
	case line == 0:
		return "File is empty.", nil
	case written == 0:
		return fmt.Sprintf("offset %d is past the end of the file (%d lines).", offset, line), nil
	}
	last := offset + written - 1
	if last < line {
		fmt.Fprintf(&b, "... lines %d-%d of %d; use offset=%d to continue\n", offset, last, line, last+1)
	}
	return b.String(), nil
}

// openText opens path for reading after checking that it is a regular file
// within the size cap and does not look binary.
func openText(path string, maxBytes int64) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file is %d bytes, the limit is %d", info.Size(), maxBytes)
	}
	f, err := os.Open(path) //nolint:gosec // the path is confined to the workspace root
	if err != nil {
		return nil, err
	}
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		_ = f.Close()
		return nil, err
	}
	if isBinary(head[:n]) {
		_ = f.Close()
		return nil, fmt.Errorf("%s looks like a binary file", path)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// newScanner returns a line scanner whose buffer can hold any line of a file
// within the size cap. The default 64 KB token limit would fail the whole
// read on one long line, such as minified JavaScript or a lockfile.
func newScanner(r io.Reader, maxBytes int64) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), int(maxBytes)+1)
	return sc
}
