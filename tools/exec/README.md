# tools/exec

A `bash` tool that runs shell commands behind a `Sandbox`, under a command, environment, and network policy, and always behind an approval marker.

```go
import "github.com/urmzd/saige/tools/exec"

sb, err := exec.NewSubprocess(exec.NoNetwork())                 // sandbox-exec on macOS, unshare on Linux
bash, err := exec.NewBashTool(sb, "/path/to/workspace", exec.DefaultPolicy())
```

## Layers

| Layer | Role |
|-------|------|
| `Sandbox` | The isolation boundary. `Subprocess` runs a local child process in its own process group, killed as a group when the shell exits, on timeout, or on cancel, so background children never outlive the call. `WithWrapper` runs commands under a container or namespace launcher you supply. |
| `Policy` | Checks before anything runs. `Commands` allows or denies programs by name, `Env` builds the environment from nothing, `Network` says whether the network may be used, `Timeout` and `MaxTimeout` bound each call. |
| Marker | The tool is always wrapped in a `human_approval` marker. There is no option to remove it. |

`DefaultPolicy()` denies privilege escalation and disk or power management programs (`DefaultDeny`), passes only `PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR`, and `TZ`, denies the network, and limits commands to 2 minutes by default and 10 at most.

`NewBashTool` fails with `ErrNetworkUnenforced` when the policy denies the network but the sandbox cannot block it. Set `Policy.Network = exec.NetworkAllow` to run a plain `Subprocess` with network access.

The command check reads command positions (each pipeline stage, list item, and substitution) without a full shell parser. It is a guardrail, not a boundary: a shell can always build a program name at run time. Isolation comes from the sandbox and the decision from the approval.

## Tool

| Argument | Description |
|----------|-------------|
| `command` | The script, run with `bash -c` (or `/bin/sh -c` when bash is missing) |
| `cwd` | Working directory inside the workspace root |
| `timeout_seconds` | Per-call limit, capped by `MaxTimeout` |

A non-zero exit is reported in the result text with stdout and stderr, each capped at 64 KB (`WithMaxOutput`). Capability: `destructive`.

## Related

- [`tools/fs`](../fs/README.md): workspace file tools
- [Root README](../../README.md)
