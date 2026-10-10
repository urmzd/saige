// Package fs provides workspace file tools for agents: read, list, glob,
// grep, write, and edit.
//
// Every tool is confined to one workspace root: paths that escape it through
// "../", an absolute path, or a symlink are rejected. The pack is read-only by
// default. AllowWrites adds write and edit, each wrapped in a human_approval
// marker so the agent loop pauses for a decision before a file changes.
// Whether a marked call runs is decided by the consumer or a gate, not by the
// tool.
package fs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools/internal/safepath"
)

// Default limits. Each can be changed with an Option.
const (
	// DefaultMaxFileBytes is the largest file read, grep, write, or edit
	// touches.
	DefaultMaxFileBytes = 10 << 20
	// DefaultMaxLineChars caps each line returned by read and grep, so one
	// minified line cannot fill the model's context window.
	DefaultMaxLineChars = 2000
	// DefaultReadLimit is the number of lines read returns when the call
	// gives no limit.
	DefaultReadLimit = 500
	// DefaultMaxResults caps list entries, glob paths, and grep matches per
	// call.
	DefaultMaxResults = 200
)

// ApprovalKind is the marker kind on write and edit.
const ApprovalKind = "human_approval"

// Argument names and descriptions shared by the file tools.
const (
	argPath      = "path"
	argPattern   = "pattern"
	descFilePath = "File path relative to the workspace root."
)

// skipDirs are directory names glob and grep never descend into.
var skipDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true}

type config struct {
	root         string
	allowWrites  bool
	maxFileBytes int64
	maxLineChars int
	readLimit    int
	maxResults   int
}

// Option configures NewTools.
type Option func(*config)

// AllowWrites adds the write and edit tools. Both always carry an approval
// marker.
func AllowWrites() Option { return func(c *config) { c.allowWrites = true } }

// WithMaxFileBytes sets the largest file the tools read or write.
func WithMaxFileBytes(n int64) Option {
	return func(c *config) {
		if n > 0 {
			c.maxFileBytes = n
		}
	}
}

// WithMaxLineChars sets the per-line cap for read and grep output.
func WithMaxLineChars(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxLineChars = n
		}
	}
}

// WithReadLimit sets the default number of lines read returns.
func WithReadLimit(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.readLimit = n
		}
	}
}

// WithMaxResults sets the cap on glob paths and grep matches.
func WithMaxResults(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxResults = n
		}
	}
}

// ErrNoRoot is returned when NewTools gets no workspace root.
var ErrNoRoot = errors.New("fs: a workspace root is required")

// NewTools returns the file tools confined to root, which must be an
// existing directory. Without AllowWrites only read, list, glob, and grep
// are returned.
func NewTools(root string, opts ...Option) ([]types.Tool, error) {
	if root == "" {
		return nil, ErrNoRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fs: resolve root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fs: root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fs: root %s is not a directory", abs)
	}
	cfg := &config{
		root:         abs,
		maxFileBytes: DefaultMaxFileBytes,
		maxLineChars: DefaultMaxLineChars,
		readLimit:    DefaultReadLimit,
		maxResults:   DefaultMaxResults,
	}
	for _, o := range opts {
		o(cfg)
	}

	tools := []types.Tool{&readTool{cfg}, &listTool{cfg}, &globTool{cfg}, &grepTool{cfg}}
	if cfg.allowWrites {
		tools = append(tools, approval(&writeTool{cfg}), approval(&editTool{cfg}))
	}
	return tools, nil
}

// approval wraps a mutating tool in the approval marker.
func approval(t types.Tool) types.Tool {
	name := t.Definition().Name
	return types.WithMarkers(t, types.Marker{
		Kind:    ApprovalKind,
		Message: "File change requires approval: " + name,
		Meta:    map[string]any{"tool": name, "mutating": true},
	})
}

// resolve confines a requested path to the root.
func (c *config) resolve(requested string, requireExist bool) (string, error) {
	return safepath.Resolve(c.root, requested, requireExist)
}

// rel renders p relative to the root for tool output.
func (c *config) rel(p string) string {
	if r, err := filepath.Rel(c.root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

// stringArg returns a string argument, or "" when absent or not a string.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// intArg returns a positive integer argument, or def when it is absent,
// not a number, or not positive.
func intArg(args map[string]any, key string, def int) int {
	var f float64
	switch v := args[key].(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case json.Number:
		n, err := v.Float64()
		if err != nil {
			return def
		}
		f = n
	default:
		return def
	}
	if f < 1 {
		return def
	}
	return int(f)
}

// boolArg returns a boolean argument, or false when absent.
func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// truncateLine shortens s to at most max bytes, cut on a rune boundary, with
// a note saying how much was dropped.
func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s...(%d more chars)", s[:cut], len(s)-cut)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// isBinary reports whether the leading bytes of a file look binary.
func isBinary(head []byte) bool {
	return bytes.IndexByte(head, 0) >= 0
}
