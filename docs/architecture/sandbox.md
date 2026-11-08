# Sandbox

`github.com/urmzd/saige/sandbox` is the library for where an agent's turn runs. A program composes it in code to run tools on bounded pools of execution nodes, over one shared workspace, and to trust that every tool is confined to the workspace areas it was granted. It is not a service: nothing in it listens on a port or reads configuration. This document describes the beta architecture; interfaces are stabilizing but pre-1.0.

## Design Goals

1. **A library, chosen in code.** The unit a program deploys is a service spec, and the program decides every backend by constructing it. A process embedding sandbox owns the lifecycle; sandbox owns no listener and no configuration.
2. **Default-deny sandbox.** Tools are locked into declared areas of the workspace and declared spawn targets. Anything not granted is denied, so a misbehaving or compromised tool cannot leak state outside its area.
3. **One shared workspace.** Every node mounts the same storage backend. State lives in exactly one place regardless of how many agents run or where.
4. **Orthogonality.** Each package owns one concern and composes through small interfaces. The dependency graph is a strict DAG; changing a backend or substrate touches one package.
5. **No second durability layer.** Tasks run in process. Durable, retried runs belong to durable, which the layers above wrap around sandbox tasks.

## Package Graph

```text
task        (leaf: Task, Result)
workspace   (leaf: shared blob store; Local backend on os.Root)
metrics     (leaf: Recorder; Nop, Memory)
ngac        (leaf: NGAC policy machine: Graph, Spec, decisions)
   |
sandbox     -> ngac, workspace    (enforcement point: ScopePDP, Policy sugar)
tool        -> task, workspace    (Tool, Runtime, Registry, Provider)
runner      -> tool, task         (Executor, Local, Shell tool adapter)
isolate     -> workspace, runner  (Driver, Instance, Spec, Pool, Executor)
isolate/local  -> isolate         (no isolation: host processes, for tests)
isolate/docker -> isolate         (containers through the docker CLI)
isolate/isolatetest -> isolate    (the conformance suite every driver runs)
isolate/egress (leaf: the egress gate, an http.Handler forward proxy)
node        -> task, sandbox      (Node, Spec, Factory, Spawner)
node/inproc -> node, tool, ...    (in-process execution substrate)
   |
controlplane -> everything above  (composition root: pools, Deploy/Scale/Submit/Status)
```

Arrows point at dependencies. Nothing but `controlplane` depends on `controlplane`, and the leaves depend on nothing but the standard library; no sandbox package imports anything outside it. `sandbox/wirecodes` gives the sentinels their saige wire codes, so the packages themselves stay free of saige's types.

## Execution: Bounded In-Process Pools

Every deployment owns a pool. `Replicas` nodes from the `node.Factory` each run one task at a time; tasks submitted while every node is busy wait in a FIFO backlog:

```text
submitters                     backlog              pool
----------                     -------              ----
program   --Submit/Async-->  [ task task task ]  --> node 1 (busy)
tools     --Spawn---------->   (oldest first)    --> node 2 (busy)
                                                 --> node 3 (free longest: takes the next task)
```

- **Sizing**: `Replicas` (zero means one) is how many of the deployment's tasks run at once. A negative value starts with no nodes, so tasks wait until `Scale` adds some.
- **Backlog**: bounded per deployment (`WithBacklog`, default 1024). A full backlog fails `SubmitAsync` with `ErrFull` rather than blocking the submitter.
- **Scaling**: growing starts nodes at once and hands them waiting tasks. Shrinking closes free nodes at once; a busy node finishes its task and then retires, so `Scale` never cancels work.
- **Goroutines**: one exists only while a node has work; an idle deployment costs nothing but its nodes.
- **Contexts**: tasks run under the control plane's own context, never the submitter's. Ending the context given to `Submit` stops the wait, not the task. `Close` refuses new work, fails waiting tasks with `ErrClosed`, cancels running ones, waits for them, and closes every node.
- **Results**: `Submit` blocks on a task's result; `SubmitAsync` returns its ID for `Result` to poll. Each deployment keeps finished results up to a bound (`WithResultRetention`, default 1024), and `Result` fails with `ErrUnknownTask` for an ID it never had or has forgotten, so a poller can tell a lost task from a running one.

Caveat: nothing is persisted. A task lives as long as the process; a crash loses waiting and running tasks and their results. That is deliberate. A caller that needs retries wraps each task in a run on saige's [durable engine](durable-engine.md), whose ledger and queue retry a lost attempt. sandbox does not keep a second, weaker queue underneath.

## Access Control: NGAC Definition, Sandbox Enforcement

Access is *defined* in an NGAC policy machine (`ngac`, after NIST SP 800-178) and *enforced* by the sandbox (`sandbox`). The policy graph relates:

- **Users** (agent and tool names) grouped by **user attributes** (e.g. `agents`, `trusted`)
- **Objects** (workspace keys, spawn targets like `tool:auditor`) contained in **object attributes**; an attribute with a key prefix contains every workspace key under it, path-segment aware
- **Policy classes** as top-level containers: access is granted only if *every* policy class containing the object is satisfied by some association
- **Associations** granting operations (`read`, `write`, `delete`, `spawn`) from a user attribute over an object attribute
- **Prohibitions** as overriding denials (e.g. a group may write, this one member may not)

Deployments define access one of two ways in their `ServiceSpec`:

1. **Flat policies** (`policies`): per-tool workspace areas plus a spawn allowlist. `sandbox.FromPolicies` compiles them into an NGAC graph, so both paths share one decision engine. This covers most deployments.
2. **Full NGAC** (`access`): a declarative `ngac.Spec` (JSON-able) with attributes, associations, and prohibitions, for relationships flat policies cannot express.

Enforcement: nodes never hand tools the raw workspace. `sandbox.ScopePDP` wraps it in a decision-checking decorator (the enforcement point); every read/write/delete is decided under the executing tool's name, `List` filters out keys the user cannot read, traversal (`..`, absolute keys) is rejected before any decision, and spawn attempts check the `spawn` operation against `tool:<target>` objects. Unknown users, ungoverned objects, empty graphs: all deny. The sandbox and the policy machine are a security boundary; a bypass is a vulnerability (see SECURITY.md).

## Tool Providers

A `tool.Provider` builds a family of tools by one mechanism, labelled by a `tool.Kind`; `Registry.RegisterProvider` registers all of a provider's tools or none.

## Sandboxed Runs

`runner` is process isolation, distinct from `sandbox` (the NGAC enforcement point over the workspace). A `runner.Executor` runs one command to completion, streams its combined output, and returns its exit code; `runner.Shell` adapts an executor into a tool taking `{"script": ...}`. `runner.Local` runs host processes and is for development and tests only.

## Isolated Instances

`isolate` is where a run's tools execute: an `Instance` started by a `Driver` from a `Spec`, private to one run and destroyed when it ends. The agent loop stays in the trusted program; only commands run in the instance. An instance has a fixed layout:

| Path | Holds | Access |
|------|-------|--------|
| `/work` | The run's own files; commands start here; `Files()` on the host | Read and write, removed at `Close` |
| `/project` | The project's base snapshot; `Project()` on the host | Read only inside, written by the host, removed at `Close` |
| `/me` | The requester's volume; `Me()` on the host | Read and write, removed at `Close` |
| `/run/playscape` | The directory shared with the connectors compartment | Read and write |

Each instance has its own `/project` and `/me`, created empty by the driver. They are not part of the `Spec`, so they never change its `Key` or which warm instance a lease gets: the host fills them through `Project()` and `Me()` after the lease and reads `/me` back before `Release`. No host directory is ever mounted into an instance.

Every zero `Spec` field is the strictest choice: no network (loopback only), no host directories, the driver's non-root user, and the `Default*` resource limits. There is no way to ask for unlimited. A driver that cannot enforce part of a valid spec fails with `ErrUnsupported` instead of starting something weaker.

**Compartments.** A spec with `Connectors` gets a second compartment for the person's tool servers, which hold credentials the workload must not read. It mounts none of the workload's directories and has its own processes, network and limits; the two share only the socket directory. `Command.In` picks the compartment a command runs in.

**Drivers.**

| Driver | Boundary | Use |
|--------|----------|-----|
| `isolate/docker` with `runsc` (default) | gVisor: a user-space kernel per container | Untrusted code |
| `isolate/docker` with `runc` | Namespaces, cgroups, seccomp, no capabilities | Development only, behind `InsecureDevelopmentRuntime` |
| `isolate/local` | None; every `Start` says so on its log | Tests only |

The Docker driver shells out to the `docker` CLI, so it adds no Go dependency. `New` accepts only a runtime that gives each container its own kernel (runsc today; a microVM runtime once CI runs the suite against it) unless `Config.InsecureDevelopmentRuntime` is set, so production cannot fall back to runc by leaving a field empty; with the opt-in, `New` warns on `Config.Log`. Each compartment is a container started with `--network none --read-only --cap-drop ALL --security-opt no-new-privileges --init`, a seccomp profile (`--security-opt seccomp=/dev/stdin`, the embedded `seccomp.json` on the CLI's stdin, so no file on disk can be swapped in), a numeric non-root `--user`, `--pids-limit`, `--memory` with equal `--memory-swap`, `--cpus`, and a size-limited tmpfs at `/tmp`. `/work`, `/project` and `/me` are host directories in the instance's own directory under `Config.WorkRoot`, bind-mounted into the workload (`/project` read only), so `Files()`, `Project()` and `Me()` are `workspace.Local`s over them. The socket directory is a 1 MiB tmpfs Docker volume mounted in both containers. Commands run through `docker exec`; their environment reaches the CLI through its own environment, never its argv. A command that times out restarts its compartment (`docker restart -t 0`), because ending a `docker exec` client does not end the process it started.

**Remote daemons.** `Config.Host` points the CLI at another daemon (`--host`, plus `--tlsverify` and the certificates in `Config.TLSDir`, which a `tcp://` host requires), so the program never holds a Docker socket, which is root on its machine. A remote daemon does not see the program's files, so the driver changes two things and nothing else: `/work`, `/project` and `/me` are labelled Docker volumes on the daemon's machine, with `/work` and `/me` opened to the instance's user by `docker exec --user 0 chmod 0777` (root in the container has no capabilities, but owns new volumes), and `Files()`, `Project()` and `Me()` run one `docker exec` of a short `sh` script per call, with the key as an argument, never in the script. `/work` and `/me` are reached as the instance's user. `/project` stays root's, mode 0755, and is mounted read and write so `Project()` can write it as root: the workload's non-root user, with no capabilities and no new privileges, can only read it, which the conformance suite's read-only check proves. A key must be a regular file, as with `workspace.Local`: a link the workload plants is not read, listed or deleted, and a write replaces the link instead of following it. Every path is resolved by the sandbox's kernel, never by the host or the daemon. The conformance suite runs against the local daemon through its endpoint, as if it were remote.

**Pools.** `Pool` keeps `Warm` started instances per spec it has been asked for and leases each to one run. `Release` (or the lease's `Close`) destroys the instance; the pool never hands it out again and starts a fresh one in the background. `Max` bounds every instance the pool holds, including ones starting and ones being destroyed. A lease that finds no warm instance starts one itself; when the pool is full it evicts a warm instance of another spec and waits for room, and refills hold back while any lease waits. Background start failures go to `OnError` and are retried with backoff.

**Runs.** `isolate.Executor` adapts an instance into a `runner.Executor`, so `runner.Shell` runs inside a lease. Every `Run` shares the instance's files and processes; the request's image and resources are fixed by the spec and ignored.

**Seccomp.** The profile is Docker's default with these removed from every allow rule, whatever capability the rule is conditioned on: `ptrace`, `process_vm_readv`/`writev` and `kcmp`; `mount`, `umount`/`umount2`, the new mount API, `pivot_root`, `unshare` and `setns`; `bpf`, `perf_event_open`, `userfaultfd`, `keyctl`, `add_key` and `request_key`; `name_to_handle_at` and `open_by_handle_at`; `kexec_*`, the module syscalls, `swapon`/`swapoff`, `reboot` and `acct`. Docker's argument filter on `personality` is kept. Anything not allowed fails with EPERM. With all capabilities dropped Docker's default already denies most of these; the profile keeps them denied if a capability is added back, and denies `ptrace`, `process_vm_*` and `name_to_handle_at`, which Docker allows without one. runc enforces it in the host kernel; runsc enforces it in gVisor's kernel when registered with `--oci-seccomp`, as CI does.

**Under gVisor.** CI registers runsc with `runsc install -- --oci-seccomp --host-uds=all`, and production should match. `--host-uds=all` lets a unix socket one sandbox creates in the shared socket directory be connected from the other; without it each sandbox's sockets exist only in its own kernel and the compartments cannot talk. gVisor's netstack does not reach Docker's embedded DNS server (127.0.0.11) on hosts whose NAT rules are in nftables, so a compartment on the egress network names the gate by IP address. Docker's `--pids-limit` is the host cgroup's, and under gVisor it bounds the sandbox's host threads (about 35 idle, 2 more per task), not its processes: a workload reaching it starves gVisor and the instance dies. So under runsc the driver gives the host cgroup room (`4 × PIDs + 64`) and, right after each container starts or restarts, writes the PIDs limit into the sandbox's own cgroupfs (`/sys/fs/cgroup/pids/pids.max`, via `docker exec --user 0` and the image's `/bin/sh`), which gVisor enforces with EAGAIN; the workload's non-root user cannot change it. The conformance suite checks the limit by forking past it and then running more commands, so it holds under both runtimes.

Caveats: `/work` and `/me` are not size limited by the Docker driver: they are host directories, so the host reads them directly, and capping it per instance would need root on the host (a mount per instance) or would hide it from the host (a tmpfs inside the container). Bound it by giving `Config.WorkRoot` a filesystem of its own. Warm instances are not health checked. `isolatetest` is run by both drivers; the boundary checks (read-only root and project, non-root, no capabilities, no new privileges, loopback only, pids and memory limits in the cgroup, bounded scratch, the denied syscalls failing with EPERM, connectors unable to see the workload's mounts and the workload unable to read the connectors' environment) run against Docker only. CI runs them under runc in the test job and under runsc in the gVisor job, which installs a pinned, checksummed gVisor release and fails rather than skips when Docker is missing.

## The Egress Gate

`isolate/egress` is an instance's only way out. It is an `http.Handler` forward proxy; the embedding program serves it where sandboxes can reach it. Each lease gets a grant: `Gate.Grant(id, policy)` returns a token, and `egress.ProxyEnv(addr, token)` is the environment (`HTTP_PROXY`, `HTTPS_PROXY`, an empty `NO_PROXY`) that sends a command's clients through the gate with it. `Revoke(id)` ends the grant when the lease is released. A request without a valid token gets 407.

A policy is a default-deny list of rules: a host (exact, or `*.domain` for its subdomains) and its ports (80 and 443 unless listed). A request for anything else gets 403 before the gate dials anything, so no part of it leaves. The gate also refuses to dial loopback, private, link-local and carrier-grade NAT addresses, checked on the address actually dialed after name resolution, unless `Config.AllowNonPublic` is set.

Credential injection: a rule's `Inject` names a header, the placeholder the sandbox holds, and a credential the caller's `Resolver` returns the real value of. What the gate can inject into depends on what it can see:

| Request from the sandbox | Gate sees | Allow or deny | Injection |
|--------------------------|-----------|---------------|-----------|
| `http://host/...` through the proxy | The whole request | Yes | Yes |
| `https://host/...` through the proxy (`CONNECT`) | Host and port only | Yes | No: the bytes are TLS, and the gate does not intercept it |
| `http://route/...` where the rule has an `Upstream` | The whole request | Yes | Yes, and the gate forwards over TLS to the upstream |

The third row is how a sandbox reaches an HTTPS API with a credential it never holds. `CONNECT` to a route is refused, since nothing could be injected. A resolver error fails the request with 502 before anything is sent. The proxy token (`Proxy-Authorization`) is never forwarded. Every decision goes to `Config.OnDecision` for an audit log, without credentials.

The Docker driver joins a compartment whose spec has `Network.Proxy` to `Config.EgressNetwork` instead of `none`, after checking once that the network was created with `--internal`; a network with a route out is refused (`ErrUnsupported`), since it would let a compartment around the gate. The driver sets no proxy variables itself: the token belongs to a lease, not to the spec the pool warms by.

Caveats: the token is in the sandbox's environment, so code there can read it; it is worth that lease's allowlist and injections to anyone on the egress network until revoked. Instances on one egress network can reach each other. A response can carry an injected credential back if the upstream echoes it.

## Self-Referential Agents

`tool.Runtime.Spawn` submits a sub-task back into the tool's own deployment and returns its task ID. The child waits for a node like any other task, so an agent calling another agent (or itself) is just another submitter. Recursion is bounded by policy: a tool can only spawn tools on its allowlist, and the default is none.

Caveat: a tool that blocks until its own sub-task finishes holds its node while it waits. If every node of a deployment is waiting on a child, no child can run. Size `Replicas` for the depth of waiting, or run the sub-task on the parent's node (flow does: `Spec` returns the deployment's policy so a caller can scope a sub-task's runtime itself).

## Metrics

Components record through `metrics.Recorder` (counters, gauges, distributions) and stay ignorant of transport. The beta ships an in-memory recorder (`metrics.NewMemory`, read with `Snapshot`) and a no-op one:

| Series | Meaning |
|--------|---------|
| `sandbox_nodes{deployment}` | nodes the deployment holds |
| `sandbox_tasks_pending{deployment}` | tasks waiting for a free node |
| `sandbox_tasks_submitted_total{deployment,tool}` | tasks accepted |
| `sandbox_tasks_total{deployment,tool,status}` | tasks finished, `ok` or `error` |
| `sandbox_task_seconds_{count,sum}{deployment,tool}` | execution latency |

A Prometheus or OpenTelemetry recorder replaces the in-memory one without any caller changing.

## Workspace Backends

`workspace.Workspace` is a minimal blob store (Read/Write/List/Delete over slash-separated keys). The beta ships `workspace.Local`, a directory on disk. Because the interface is the only thing the rest of the system sees, GCS/S3-style backends slot in without touching sandbox, nodes, or the control plane.

`Local` reaches its directory only through an `os.Root`. Key validation already rejects `..` and absolute keys; the root also refuses any path that a symbolic link would lead out of the directory, including a link swapped in while a call is in flight, which a check-then-open could not catch. Keys name regular files: `List` does not report links, `Read` and `Delete` of a directory or link report `ErrNotFound`, and `Write` (a temporary file renamed into place) replaces a link at its key instead of writing through it.

## Validated Paths

- `go test -race ./sandbox/...` runs the `isolatetest` suite against `isolate/local` and, where a Docker daemon is reachable, `isolate/docker` (runc; `SANDBOX_DOCKER_TEST_RUNTIME=runsc` runs it under gVisor, as CI does), and an end-to-end egress test (a sandbox on an internal network, the gate in a container, two upstreams on a second network). It covers sandbox confinement (traversal, list-leak), workspace confinement (symlink escapes through linked directories and files), NGAC semantics (attribute inheritance, multi-policy-class conjunction, prohibition override), the pools (bounded concurrency, FIFO backlog, `ErrFull`, drain on scale-down, retention, `Close`) and policy-gated spawning.
- [`examples/sandbox`](../../examples/sandbox/) composes a control plane, deploys a service with a policy, and shows a write outside the tool's area denied.

## Roadmap

- GCS/S3 workspace backends behind `workspace.Workspace`
- An OpenTelemetry recorder for `metrics.Recorder`
- Container-per-node execution substrate behind `node.Factory`
- `tools/exec` commands run through `runner.Executor`, so saige has one sandbox
