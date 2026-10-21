package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

type listTool struct{ cfg *config }

func (t *listTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "list",
		Capability: types.ToolCapabilityRead,
		Description: "List the entries of one workspace directory, not recursively. " +
			"Directories end in /, files show their size, and symlinks show their target.",
		Parameters: types.ParameterSchema{
			Type: types.SchemaObject,
			Properties: map[string]types.PropertyDef{
				argPath: {Type: types.SchemaString, Description: "Directory relative to the workspace root (default: the root)."},
			},
		},
	}
}

func (t *listTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	root, dir, err := t.cfg.open(dirArg(args))
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("list: %s is not a directory", show(dir))
	}
	f, err := root.Open(dir)
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool {
		// Directories first, then by name.
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	var b strings.Builder
	shown := 0
	for _, e := range entries {
		if shown >= t.cfg.maxResults {
			fmt.Fprintf(&b, "... %d more entries\n", len(entries)-shown)
			break
		}
		shown++
		switch {
		case e.IsDir():
			fmt.Fprintf(&b, "%s/\n", e.Name())
		case e.Type()&os.ModeSymlink != 0:
			// The target is shown as written; reading through it is still
			// confined by the other tools.
			target, _ := root.Readlink(filepath.Join(dir, e.Name()))
			fmt.Fprintf(&b, "%s -> %s\n", e.Name(), target)
		default:
			size := int64(-1)
			if fi, err := e.Info(); err == nil {
				size = fi.Size()
			}
			fmt.Fprintf(&b, "%s\t%d bytes\n", e.Name(), size)
		}
	}
	if shown == 0 {
		return "Directory is empty.", nil
	}
	return b.String(), nil
}
