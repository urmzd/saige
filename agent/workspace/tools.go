package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Tool names.
const (
	WriteToolName  = "scratch_write"
	ReadToolName   = "scratch_read"
	SearchToolName = "scratch_search"
)

// DefaultReadLimit and MaxReadLimit bound one scratch_read call, in bytes.
const (
	DefaultReadLimit = 8 << 10
	MaxReadLimit     = 64 << 10
)

// Tools returns scratch_write, scratch_read, and scratch_search over ws. Each
// call uses the workspace attached to its context by NewContext when there is
// one, so a sub-agent given a read-only view cannot write through tools built
// for its parent. scratch_write declares the write capability; the others
// declare read.
func Tools(ws Workspace) []types.Tool {
	return []types.Tool{WriteTool(ws), ReadTool(ws), SearchTool(ws)}
}

// ReadOnlyTools returns only scratch_read and scratch_search.
func ReadOnlyTools(ws Workspace) []types.Tool {
	return []types.Tool{ReadTool(ws), SearchTool(ws)}
}

func resolve(ctx context.Context, fallback Workspace) (Workspace, error) {
	if ws, ok := FromContext(ctx); ok {
		return ws, nil
	}
	if fallback == nil {
		return nil, fmt.Errorf("workspace: no workspace configured")
	}
	return fallback, nil
}

// WriteTool returns scratch_write.
func WriteTool(ws Workspace) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        WriteToolName,
			Description: "Save text to the scratch workspace under a name, replacing what the name held before. Returns a saige-artifact:// URI. Use it for notes and intermediate results you will need later in this task.",
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{"name", "content"},
				Properties: map[string]types.PropertyDef{
					"name":    {Type: types.SchemaString, Description: "Name to save under, for example notes/plan.md."},
					"content": {Type: types.SchemaString, Description: "Text to save."},
				},
			},
			Capability: types.ToolCapabilityWrite,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			name, _ := args["name"].(string)
			content, _ := args["content"].(string)
			ref, err := w.Put(ctx, name, []byte(content), nil)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("saved %s (%d bytes) as %s", ref.Name, ref.Size, ref.URI()), nil
		},
	}
}

// ReadTool returns scratch_read.
func ReadTool(ws Workspace) types.Tool {
	return readTool(ws, ReadToolName,
		fmt.Sprintf("Read from the scratch workspace by saige-artifact:// URI or by name. Returns up to %d bytes by default; use offset to page through larger artifacts.", DefaultReadLimit),
		"ref", "A saige-artifact:// URI or an artifact name.")
}

// readTool reads one artifact a page at a time. arg names the argument that
// holds the URI or name.
func readTool(ws Workspace, name, description, arg, argDescription string) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        name,
			Description: description,
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{arg},
				Properties: map[string]types.PropertyDef{
					arg:      {Type: types.SchemaString, Description: argDescription},
					"offset": {Type: types.SchemaInteger, Description: "Byte offset to start at. Default 0."},
					"limit":  {Type: types.SchemaInteger, Description: fmt.Sprintf("Bytes to read, at most %d.", MaxReadLimit)},
				},
			},
			Capability: types.ToolCapabilityRead,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			s, _ := args[arg].(string)
			ref, err := ParseRef(s)
			if err != nil {
				return "", err
			}
			ref, err = w.Stat(ctx, ref)
			if err != nil {
				return "", err
			}
			offset := intArg(args, "offset", 0)
			if offset < 0 {
				offset = 0
			}
			limit := intArg(args, "limit", DefaultReadLimit)
			if limit <= 0 || limit > MaxReadLimit {
				limit = MaxReadLimit
			}
			data, err := w.Read(ctx, ref, offset, limit)
			if err != nil {
				return "", err
			}
			text := string(data)
			if end := offset + int64(len(data)); end < ref.Size {
				// Back off to a word boundary so no value, such as an email
				// address, is split across pages where a redactor would see
				// only fragments.
				text = text[:cutAtBoundary(text, len(text))]
				end = offset + int64(len(text))
				return text + fmt.Sprintf("\n[%d of %d bytes shown; continue with offset %d]", end-offset, ref.Size, end), nil
			}
			return text, nil
		},
	}
}

// SearchTool returns scratch_search.
func SearchTool(ws Workspace) types.Tool {
	return &types.ToolFunc{
		Def: types.ToolDef{
			Name:        SearchToolName,
			Description: "Search the scratch workspace for lines containing every word of the query. Returns each match with its artifact URI and byte offset.",
			Parameters: types.ParameterSchema{
				Type:     types.SchemaObject,
				Required: []string{"query"},
				Properties: map[string]types.PropertyDef{
					"query": {Type: types.SchemaString, Description: "Words to find, case-insensitive."},
					"k":     {Type: types.SchemaInteger, Description: "Maximum matches. Default 10."},
				},
			},
			Capability: types.ToolCapabilityRead,
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			w, err := resolve(ctx, ws)
			if err != nil {
				return "", err
			}
			q, _ := args["query"].(string)
			hits, err := w.Search(ctx, q, int(intArg(args, "k", 10)))
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "no matches", nil
			}
			var b strings.Builder
			for _, h := range hits {
				fmt.Fprintf(&b, "%s:%d (%s offset %d): %s\n", h.Ref.Name, h.Line, h.Ref.URI(), h.Offset, h.Text)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// cutAtBoundary returns where to cut s at or before limit so that the cut
// does not split a word. A limit past the end of s keeps all of s; a limit
// at the end is treated as a page cut, since more text may follow. It cuts after the last separator (whitespace or one
// of ,;"'{}[]<>|) whose next byte is known, unless the separator sits inside
// a number written in groups, as card, phone, and account numbers often are.
// A newline is always a boundary. Without such a separator it falls back to
// the last whole UTF-8 character, so a long unbroken token still makes
// progress.
func cutAtBoundary(s string, limit int) int {
	if limit > len(s) {
		return len(s)
	}
	for i := limit; i > 0; i-- {
		c := s[i-1]
		if c == '\n' {
			return i
		}
		if !isSeparator(c) || i == len(s) {
			continue
		}
		if i >= 2 && groupedNumber(s[i-2], s[i]) {
			continue
		}
		return i
	}
	return len(cutToRuneStart(s[:limit]))
}

func isSeparator(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\f', '\v', ',', ';', '"', '\'', '{', '}', '[', ']', '<', '>', '|':
		return true
	}
	return false
}

// groupedNumber reports whether a separator between before and after likely
// joins groups of one identifier, such as "4111 1111" or "(555) 123".
func groupedNumber(before, after byte) bool {
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	joined := func(c byte) bool { return digit(c) || c >= 'A' && c <= 'Z' || c == '(' || c == ')' || c == '+' }
	return joined(before) && joined(after) && (digit(before) || digit(after))
}

// cutToRuneStart drops a trailing partial UTF-8 character.
func cutToRuneStart(s string) string {
	i := len(s)
	for back := 0; back < 4 && i > 0; back++ {
		c := s[i-1]
		if c < 0x80 {
			return s
		}
		i--
		if c&0xC0 != 0x80 {
			// s[i] starts a multi-byte character; keep it only if complete.
			need := 2
			switch {
			case c&0xF0 == 0xF0:
				need = 4
			case c&0xE0 == 0xE0:
				need = 3
			}
			if len(s)-i >= need {
				return s
			}
			return s[:i]
		}
	}
	return s
}

func intArg(args map[string]any, key string, def int64) int64 {
	switch v := args[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case interface{ Int64() (int64, error) }:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return def
}
