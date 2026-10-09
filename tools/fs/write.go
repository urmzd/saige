package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	path, err := t.cfg.resolve(stringArg(args, argPath), false)
	if err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if int64(len(content)) > t.cfg.maxFileBytes {
		return "", fmt.Errorf("write: content is %d bytes, the limit is %d", len(content), t.cfg.maxFileBytes)
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("write: %s is a directory", t.cfg.rel(path))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if err := writeAtomic(path, content); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), t.cfg.rel(path)), nil
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
	path, err := t.cfg.resolve(stringArg(args, argPath), true)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	f, err := openText(path, t.cfg.maxFileBytes)
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
		return "", fmt.Errorf("edit: old_string not found in %s", t.cfg.rel(path))
	case count > 1 && !replaceAll:
		return "", fmt.Errorf("edit: old_string occurs %d times in %s; add surrounding context to make it unique or set replace_all", count, t.cfg.rel(path))
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
	if err := writeAtomic(path, updated); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if !replaceAll {
		count = 1
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s", count, t.cfg.rel(path)), nil
}

// writeAtomic replaces path with content through a temporary file in the
// same directory, so a reader never sees a partial file. An existing file
// keeps its permission bits.
func writeAtomic(path, content string) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
