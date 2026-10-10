# tools/fs

Workspace file tools for agents: `read`, `list`, `glob`, `grep`, and, when enabled, `write` and `edit`. Every path is confined to one workspace root.

```go
import "github.com/urmzd/saige/tools/fs"

readOnly, err := fs.NewTools("/path/to/workspace")                   // read, list, glob, grep
all, err := fs.NewTools("/path/to/workspace", fs.AllowWrites())      // adds write, edit
```

## Tools

| Tool | Capability | Description |
|------|------------|-------------|
| `read` | read | Numbered lines, one page at a time (`offset`, `limit`). Lines of any length are read and cut to 2000 characters. Binary files are refused. |
| `list` | read | Entries of one directory: directories first, then files with their size; symlinks show their target |
| `glob` | read | Paths matching a pattern with `*`, `?`, `[...]`, and `**` across directories |
| `grep` | read | RE2 regular expression search with an optional `glob` filter and `ignore_case` |
| `write` | write | Create or overwrite a file, creating parent directories. Written atomically. |
| `edit` | write | Replace an exact, unique string, or every occurrence with `replace_all` |

## Safety

- **Root confinement**: `../` traversal, absolute paths outside the root, and symlinks whose target leaves the root are rejected. Walks do not follow symlinks and skip `.git`, `.hg`, `.svn`, and `node_modules`.
- **Read-only by default**: `write` and `edit` exist only with `AllowWrites()`, and each is wrapped in a `human_approval` marker, so the agent loop pauses for a decision before a file changes.
- **Limits**: files over 10 MB are not read or written; list, glob, and grep stop at 200 results. Change them with `WithMaxFileBytes`, `WithMaxLineChars`, `WithReadLimit`, and `WithMaxResults`.

## Related

- [Harness tools](../../docs/harness-tools.md): this pack, `execute_code`, and fetch as one curated toolset
- [`tools/exec`](../exec/README.md): the sandboxed bash tool
- [`tools/fetch`](../fetch/README.md): the URL fetch tool
- [Root README](../../README.md)
