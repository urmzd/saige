# Harness tools

`tools.Harness` builds one curated toolset for an agent that works in a directory: read and search files, change them, run code, fetch URLs, and keep notes in a scratch workspace. Every tool is confined to one root, and anything that changes files or runs code asks for approval first.

```go
import (
    "github.com/urmzd/saige/agent"
    "github.com/urmzd/saige/tools"
)

a := agent.NewAgent(cfg, agent.WithHarnessTools(tools.HarnessOptions{
    Root:   "/path/to/project",
    Groups: tools.AllGroups(), // default: tools.ReadOnly()
}))
```

An agent has no tools unless you add them. `WithHarnessTools` is the one-call way; `tools.Harness` returns the same toolset when you want to handle errors or inspect it first.

## Tools

| Tool | Group | Capability | Approval | Description |
|---|---|---|---|---|
| `read_file` | `read` | read | no | Numbered lines, one page at a time (`offset`, `limit`) |
| `list_dir` | `read` | read | no | Entries of one directory: directories first, file sizes, symlink targets |
| `glob` | `read` | read | no | Paths matching `*`, `?`, `[...]`, and `**` |
| `grep` | `read` | read | no | RE2 search with an optional `glob` filter and `ignore_case` |
| `write_file` | `write` | write | yes | Create or overwrite a file, atomically |
| `edit_file` | `write` | write | yes | Replace an exact, unique string, or every occurrence |
| `execute_code` | `exec` | write or destructive | yes | Run `shell`, `python`, or `go` code behind a sandbox |
| `fetch_url` | `web` | read | no | Fetch an http or https URL as text |
| `scratch_write`, `scratch_read`, `scratch_search` | always | write, read, read | no | The run's scratch workspace, separate from the root |

The file tools come from [`tools/fs`](../tools/fs/README.md), `execute_code` and the sandboxes from [`tools/exec`](../tools/exec/README.md), and `fetch_url` from [`tools/fetch`](../tools/fetch/README.md). The scratch tools are [`agent/workspace`](../agent/workspace) tools.

### execute_code

| Argument | Description |
|---|---|
| `language` | One of the languages the sandbox has: `shell` always, `python` when `python3` or `python` is present, `go` when `go` is present. The schema lists only those. |
| `code` | The program. It reaches the interpreter through a quoted heredoc, so the shell expands nothing in it. |
| `cwd` | Working directory inside the root |
| `timeout_seconds` | Per-call limit, capped by `MaxTimeout` |

Python reads the program from standard input. Go code must be a `main` package that imports only the standard library; it is built in a private directory with its own module and `GOTOOLCHAIN=local`, then run in `cwd`.

The command policy (`exec.Policy.Commands`) checks shell code, and checks the interpreter or compiler for the other languages. Programs in `exec.DefaultDeny`, such as `sudo`, are refused.

## Safety defaults

| Default | Detail |
|---|---|
| Read-only | Without `Groups`, only the `read` group is enabled. |
| Root confinement | `../` traversal, absolute paths outside the root, and symlinks that leave it are refused by every file tool and by `cwd`. |
| Approval | `write_file`, `edit_file`, and `execute_code` carry a `human_approval` marker. There is no option to remove it. |
| No network for code | `Network` defaults to `exec.NetworkDeny`. On the subprocess sandbox that needs `sandbox-exec` (macOS) or `unshare` (Linux); without them, building the exec group fails with `exec.ErrNetworkIsolationUnavailable` rather than running code with the network open. |
| Clean environment | Code runs with only `PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR`, and `TZ` from the parent. API keys in your environment do not reach it. |
| Time limits | 2 minutes per call by default, 10 at most. Set `Timeout` and `MaxTimeout`. |
| Private addresses | `fetch_url` refuses loopback, private, link-local, and cloud metadata addresses, checked after DNS on every connection. |
| Output limits | A built sandbox keeps up to 1 MiB of stdout and stderr each (`MaxOutputBytes`). |
| Spill | Any result over 16 KiB is stored in the workspace and replaced by a 2 KiB preview naming its `saige-artifact://` URI. The model pages through the rest with `scratch_read`. Tune with `Spill`, or turn it off with `NoSpill`. |

### Capability classes and grants

Each tool declares a capability class, which gates and the approval policy judge. `execute_code` takes its class from the sandbox:

| Sandbox | Network | Class | What an approval grant can do |
|---|---|---|---|
| `docker` | denied | write | A `tool` or `session` grant covers later calls |
| `subprocess` | any | destructive | Nothing: every call is asked about |
| any | allowed | destructive | Nothing: every call is asked about |

A subprocess can change files anywhere the user can, so it is never write-class. A Docker container mounts only the root, so its effects stay in the workspace. See [approval policy and grants](approval-policy.md).

## Sandboxes

| `SandboxKind` | Isolation |
|---|---|
| `subprocess` (default) | A child process in its own process group, killed as a group on exit, timeout, or cancel. Network denied by the platform wrapper. |
| `docker` | A fresh container per call: the root mounted read-write at `/workspace`, a read-only image filesystem with a private `/tmp`, all capabilities dropped, `no-new-privileges`, 1 GiB of memory, 256 processes, no network, and the caller's uid and gid. The container is removed on timeout or cancel. |

The Docker image defaults to `python:3.13-slim` (`DockerImage` changes it). Pull it before the first run, because a pull counts against the call's time limit. Building the Docker sandbox fails clearly when Docker cannot be used:

- `exec.ErrDockerUnavailable`: the `docker` CLI is missing or its daemon does not answer.
- `exec.ErrMountNotShared`: the Docker host cannot see the root. VM-based hosts often share only the home directory, so a workspace elsewhere would appear empty inside the container. Use a root inside a shared directory.

Pass your own `exec.Sandbox` in `Sandbox` to use another backend. Report `Isolation{Filesystem: true}` only when commands cannot change files outside the working directory and private temporary space.

## Options

| Field | Default | Meaning |
|---|---|---|
| `Root` | required | Directory every tool is confined to |
| `Groups` | `ReadOnly()` | Any of `GroupRead`, `GroupWrite`, `GroupExec`, `GroupWeb`; `AllGroups()` for all |
| `Sandbox` | built | An `exec.Sandbox` for `execute_code` |
| `SandboxKind` | `subprocess` | `subprocess` or `docker`, when `Sandbox` is nil |
| `DockerImage` | `python:3.13-slim` | Image for the Docker sandbox |
| `Network` | `deny` | `deny` or `allow` for `execute_code` |
| `Policy` | `exec.DefaultPolicy()` | Command, environment, and time policy |
| `Timeout`, `MaxTimeout` | 2m, 10m | Default and maximum per-call limit |
| `Languages` | detected | Limit the languages offered |
| `MaxOutputBytes` | 1 MiB | Output kept by a built sandbox, per stream |
| `Workspace` | in memory | Store for spilled results and scratch artifacts |
| `Spill` | 16 KiB, 2 KiB preview | `workspace.SpillOptions` |
| `NoSpill` | false | Return large results whole |
| `FS`, `Fetch` | none | Extra `fs.Option` and `fetch.Option` values |

The toolset's workspace becomes the agent's workspace unless `agent.WithWorkspace` sets one. Spilled results and the scratch tools always use the workspace attached to the call, so a sub-agent gets a read-only view.

## Examples

### Library: read and run code, with grants

```go
set, err := tools.Harness(ctx, tools.HarnessOptions{
    Root:        dir,
    Groups:      []tools.Group{tools.GroupRead, tools.GroupWrite, tools.GroupExec},
    SandboxKind: tools.SandboxDocker,
})
if err != nil {
    return err // for example exec.ErrDockerUnavailable
}
a := agent.NewAgent(cfg, agent.WithToolset(set), agent.WithApprovalPolicy(agent.ApprovalPolicy{}))

stream := a.Invoke(ctx, []types.Message{types.NewUserMessage("Which test fails, and why?")})
for d := range stream.Deltas() {
    if m, ok := d.(types.MarkerDelta); ok {
        // Ask the user. Approving with a session grant stops further
        // questions about write-class calls in this conversation.
        stream.ResolveMarkerErr(m.ToolCallID, agent.Resolution{
            Approved: true, Approver: "user:ada",
            Grant:    &types.GrantRequest{Scope: types.GrantSession},
        })
    }
}
```

`agent.WithHarnessTools(opts)` does the same in one option. When the toolset cannot be built, every run of that agent fails with the error.

### Disclose tools on demand

A large toolset costs context on every turn. Pin the core read tools and let the model find the rest with `tool_search`:

```go
deferred := selector.NewDeferredTools(set.Core()...) // read_file, list_dir, grep, scratch_read
a := agent.NewAgent(cfg,
    agent.WithToolset(set),
    agent.WithToolPolicy(deferred),
    agent.WithTools(deferred.Tool()),
)
```

Discovery is disclosure, not permission: a discovered tool still passes through gates and approvals.

### CLI

`saige chat` and `saige ask` take `--tools`:

| Value | Tools |
|---|---|
| `none` | No built-in tools. The default for `ask`. |
| `readonly` | `read_file`, `list_dir`, `glob`, `grep`, and the scratch tools. The default for `chat`. |
| `harness` | Every group |
| a group list | For example `read,exec` |

```bash
saige chat                                      # read-only tools over the current directory
saige chat --tools harness --workspace ./repo   # write_file, edit_file, and execute_code ask first
saige chat --tools read,exec --sandbox docker   # run code in a container
saige ask --tools harness --approve allow "Run the tests and summarize failures"
```

`--workspace` sets the root (default `.`), `--sandbox` picks `subprocess` or `docker`, and `--exec-network` sets `deny` or `allow`. In `chat`, write and exec calls show their arguments and ask `Approve? (y/n)`. `ask` cannot prompt, so `--approve` decides: `deny` by default.

## Related

- [Approval policy and grants](approval-policy.md)
- [`tools/exec`](../tools/exec/README.md), [`tools/fs`](../tools/fs/README.md), [`tools/fetch`](../tools/fetch/README.md)
- [Root README](../README.md)
