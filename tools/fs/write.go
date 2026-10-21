package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

type writeTool struct{ cfg *config }

func (t *writeTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:        "write",
		Capability:  types.ToolCapabilityWrite,
		Description: "Create or overwrite a file in the workspace with the given content. Parent directories are created as needed. Prefer edit for changes to an existing file.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPath, "content"},
			Properties: map[string]types.PropertyDef{
				argPath:   {Type: types.SchemaString, Description: descFilePath},
				"content": {Type: types.SchemaString, Description: "The complete new file content."},
			},
		},
	}
}

func (t *writeTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	content, ok := args["content"].(string)
	if !ok {
		return "", fmt.Errorf("write: content is required")
	}
	root, path, err := t.cfg.open(stringArg(args, argPath))
	if err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	defer func() { _ = root.Close() }()
	if int64(len(content)) > t.cfg.maxFileBytes {
		return "", fmt.Errorf("write: content is %d bytes, the limit is %d", len(content), t.cfg.maxFileBytes)
	}
	if info, err := root.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("write: %s is a directory", show(path))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
	}
	if err := writeAtomic(root, path, content); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), show(path)), nil
}

type editTool struct{ cfg *config }

func (t *editTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "edit",
		Capability: types.ToolCapabilityWrite,
		Description: "Replace exact text in an existing workspace file. old_string must match the file exactly, " +
			"including whitespace, and must be unique unless replace_all is true. Read the file first.",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPath, "old_string", "new_string"},
			Properties: map[string]types.PropertyDef{
				argPath:       {Type: types.SchemaString, Description: descFilePath},
				"old_string":  {Type: types.SchemaString, Description: "The exact text to replace."},
				"new_string":  {Type: types.SchemaString, Description: "The replacement text."},
				"replace_all": {Type: types.SchemaBoolean, Description: "Replace every occurrence instead of requiring exactly one."},
			},
		},
	}
}

func (t *editTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	oldStr := stringArg(args, "old_string")
	newStr, ok := args["new_string"].(string)
	if oldStr == "" || !ok {
		return "", fmt.Errorf("edit: old_string and new_string are required")
	}
	if oldStr == newStr {
		return "", fmt.Errorf("edit: old_string and new_string are identical")
	}
	root, path, err := t.cfg.open(stringArg(args, argPath))
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := openText(root, path, t.cfg.maxFileBytes)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	text := string(data)

	count := strings.Count(text, oldStr)
	replaceAll := boolArg(args, "replace_all")
	switch {
	case count == 0:
		return "", fmt.Errorf("edit: old_string not found in %s", show(path))
	case count > 1 && !replaceAll:
		return "", fmt.Errorf("edit: old_string occurs %d times in %s; add surrounding context to make it unique or set replace_all", count, show(path))
	}
	n := 1
	if replaceAll {
		n = -1
	}
	updated := strings.Replace(text, oldStr, newStr, n)
	if int64(len(updated)) > t.cfg.maxFileBytes {
		return "", fmt.Errorf("edit: result is %d bytes, the limit is %d", len(updated), t.cfg.maxFileBytes)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := writeAtomic(root, path, updated); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if !replaceAll {
		count = 1
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s", count, show(path)), nil
}

// writeAtomic replaces name, relative to root, with content through a
// temporary file in the same directory, so a reader never sees a partial
// file. An existing file keeps its permission bits.
func writeAtomic(root *os.Root, name, content string) error {
	mode := os.FileMode(0o644)
	if info, err := root.Stat(name); err == nil {
		mode = info.Mode().Perm()
	}
	dir, base := filepath.Split(name)
	var tmp string
	var f *os.File
	for attempt := 0; ; attempt++ {
		tmp = filepath.Join(dir, "."+base+".tmp"+strconv.FormatUint(rand.Uint64(), 36))
		var err error
		f, err = root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, iofs.ErrExist) || attempt >= 100 {
			return err
		}
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}
