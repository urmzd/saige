package fs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// errStopWalk ends a directory walk early once enough results are found.
var errStopWalk = errors.New("stop walk")

type globTool struct{ cfg *config }

func (t *globTool) Definition() types.ToolDef {
	return types.ToolDef{
		Name:       "glob",
		Capability: types.ToolCapabilityRead,
		Description: "List workspace files whose path matches a glob pattern. " +
			"Supports *, ?, [...] within a path segment and ** across segments, for example \"**/*.go\" or \"cmd/*/main.go\".",
		Parameters: types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{argPattern},
			Properties: map[string]types.PropertyDef{
				argPattern: {Type: types.SchemaString, Description: "Glob pattern, matched against paths relative to path."},
				argPath:    {Type: types.SchemaString, Description: "Directory to search, relative to the workspace root (default: the root)."},
			},
		},
	}
}

func (t *globTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	pattern := stringArg(args, argPattern)
	if pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	if _, err := path.Match(strings.ReplaceAll(pattern, "**", "*"), ""); err != nil {
		return "", fmt.Errorf("glob: invalid pattern %q: %w", pattern, err)
	}
	base, err := t.cfg.resolve(dirArg(args), true)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}

	var matches []string
	truncated := false
	err = walkFiles(ctx, base, func(p string) error {
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		if !matchGlob(pattern, filepath.ToSlash(rel)) {
			return nil
		}
		if len(matches) >= t.cfg.maxResults {
			truncated = true
			return errStopWalk
		}
		matches = append(matches, t.cfg.rel(p))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	if len(matches) == 0 {
		return "No files matched.", nil
	}
	sort.Strings(matches)
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n... stopped at %d paths; narrow the pattern or path", t.cfg.maxResults)
	}
	return out, nil
}

// dirArg returns the optional path argument, defaulting to the root.
func dirArg(args map[string]any) string {
	if p := stringArg(args, argPath); p != "" {
		return p
	}
	return "."
}

// walkFiles calls fn for each regular file under base, skipping version
// control and dependency directories. Symlinks are not followed, so a link
// cannot lead the walk out of the workspace. Returning errStopWalk from fn
// ends the walk without an error.
func walkFiles(ctx context.Context, base string, fn func(path string) error) error {
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is skipped rather than failing the call.
			if d != nil && d.IsDir() && p != base {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != base && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return fn(p)
	})
	if errors.Is(err, errStopWalk) {
		return nil
	}
	return err
}

// matchGlob reports whether the slash-separated name matches pattern. A "**"
// segment matches zero or more path segments; other segments use path.Match.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
