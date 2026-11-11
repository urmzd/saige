# sandbox

Part of [saige](../README.md). Import path: `github.com/urmzd/saige/sandbox`.

sandbox is a library first. Compose it in code: a workspace, a tool
registry, a node factory and a control plane, with an access policy per
deployment. It starts no server and reads no configuration.

## Features

- **Sandboxed tools**: every tool sees the workspace only through a view its policy grants; anything not granted is denied, so a misbehaving tool cannot read or write outside its areas
- **NGAC access control**: access is defined in a NIST-style policy machine (users, attributes, associations, prohibitions) and enforced by the sandbox; flat per-tool policies compile into the same graph
- **Bounded in-process pools**: each deployment runs its tasks on a fixed number of nodes, one task per node at a time, with a bounded backlog; resizing never cancels work
- **Shared workspace**: every node reads and writes one backend; the local backend is confined to its directory with `os.Root`, so a symbolic link cannot lead a key outside it
- **Self-referential agents**: a tool can spawn sub-tasks into its own deployment (an agent calling another agent), gated by its policy
- **Isolated instances**: `isolate` runs a run's commands in a sandbox instance instead of on the host. The Docker driver runs each under gVisor (runc only with an explicit development opt-in) with no network, a read-only root, no capabilities, a seccomp profile stricter than Docker's default, a non-root user, and CPU, memory, process and scratch limits; a second compartment can hold credential-bearing tool servers that share only a socket directory with the workload
- **Egress gate**: `isolate/egress` is an HTTP forward proxy that is an instance's only way out: a default-deny host allowlist per lease, and credential injection, so the sandbox holds a placeholder and the gate swaps in the real value for an allowed host
- **Warm pools, one run per instance**: `isolate.Pool` keeps instances warm per spec and leases each to exactly one run; releasing destroys it and a fresh one starts in the background
- **Command runners**: `runner.Shell` turns an executor into a tool; `isolate.Executor` runs it inside a leased instance
- **Metrics seam**: node counts, backlog, throughput and latency recorded behind a transport-agnostic `metrics.Recorder`

**Early development:** pre-1.0, so the API may change between minor versions. Tasks are not durable, the only workspace backend is a local directory, and metrics have no exporter yet. See [Limits](#limits).

## Installation

The sandbox packages import nothing outside the Go standard library:

```sh
go get github.com/urmzd/saige
```

## Quick Start

Compose a control plane from its pieces, deploy one service with a policy, and submit tasks to it:

<!-- fsrc src="../examples/sandbox/main.go" fence="auto" -->
```go
// Basic example: compose a control plane from the orthogonal pieces
// (a local workspace, a sandboxed tool, an in-process node factory, and an
// in-memory metrics recorder), then deploy a service, size its pool, submit
// tasks, and print the resulting metrics.
//
// Prerequisites: none. Run with:
//
//	go run ./examples/sandbox/
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

func main() {
	ctx := context.Background()

	// Shared workspace: every node reads and writes the same backend.
	dir, err := os.MkdirTemp("", "playscape-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		log.Fatal(err)
	}

	// One tool. It sees only the workspace view its policy grants below.
	registry := tool.NewRegistry()
	err = registry.Register(tool.Func("greet", func(ctx context.Context, rt tool.Runtime, in []byte) ([]byte, error) {
		out := []byte("hello, " + string(in))
		// Allowed: "greetings/..." is inside this tool's area.
		if err := rt.Workspace().Write(ctx, "greetings/last", bytes.NewReader(out)); err != nil {
			return nil, err
		}
		// Denied by the sandbox: "secrets/..." is outside the area.
		if err := rt.Workspace().Write(ctx, "secrets/steal", bytes.NewReader(out)); err != nil {
			fmt.Printf("sandbox blocked the leak: %v\n", err)
		}
		return out, nil
	}))
	if err != nil {
		log.Fatal(err)
	}

	// Compose the control plane and deploy one service.
	rec := metrics.NewMemory()
	plane := controlplane.NewMemory(inproc.NewFactory(registry, ws), rec)
	err = plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     "greeter",
		Replicas: 1,
		Policies: []sandbox.Policy{{Tool: "greet", Areas: []sandbox.Area{{Prefix: "greetings"}}}},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Grow the pool to three nodes; each runs one task at a time.
	if err := plane.Scale(ctx, "greeter", 3); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"ada", "grace", "alan"} {
		res, err := plane.Submit(ctx, "greeter", task.Task{Tool: "greet", Input: []byte(name)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s ran on %s: %s\n", name, res.NodeID, res.Output)
	}

	// Metrics accumulated along the way.
	fmt.Println("\nmetrics:")
	snap := rec.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s %g\n", k, snap[k])
	}
}
```
<!-- /fsrc -->

`Submit` blocks until the task's result arrives; `SubmitAsync` returns its ID for `Result` to poll. A deployment's `Replicas` is how many of its tasks run at once; further tasks wait in its backlog for a free node.

## Usage

### Isolated instances

```go
drv, err := docker.New(docker.Config{}) // runsc (gVisor); the daemon must have it registered
if err != nil {
	return err
}
pool := isolate.NewPool(drv, isolate.PoolConfig{Warm: 4, Max: 500})
defer pool.Close(ctx)

lease, err := pool.Lease(ctx, isolate.Spec{Image: "python:3.13-alpine"})
if err != nil {
	return err
}
defer lease.Release(ctx) // destroys the instance; it is never reused
// /project and /me start empty in every instance; the host fills them.
if err := lease.Project().Write(ctx, "README.md", strings.NewReader(snapshot)); err != nil {
	return err
}
if err := registry.Register(runner.Shell("shell", isolate.Executor{Instance: lease}, runner.ShellConfig{})); err != nil {
	return err
}
```

To give an instance a way out, serve an `egress.Gate` on an `--internal` Docker network, name that network in `docker.Config.EgressNetwork`, and set `Spec.Network.Proxy` to the gate's address. Each lease gets its own grant:

```go
token, err := gate.Grant(lease.ID(), egress.Policy{Allow: []egress.Rule{{
	Host:     "github.route", // the sandbox calls http://github.route/...
	Upstream: "https://api.github.com",
	Inject:   []egress.Injection{{Header: "Authorization", Placeholder: "PLACEHOLDER", Credential: "github"}},
}}})
if err != nil {
	return err
}
defer gate.Revoke(lease.ID())
env := egress.ProxyEnv("gate:3128", token) // add to every command of this lease
```

To keep the Docker socket off the machine your program runs on, drive a daemon on a sandbox host instead. `Config.Host` names it (`tcp://`, `ssh://` or `unix://`), and a `tcp://` host needs `Config.TLSDir` with `ca.pem`, `cert.pem` and `key.pem`:

```go
drv, err := docker.New(docker.Config{
	Host:   "tcp://sandbox.internal:2376",
	TLSDir: "/etc/playscape/docker-tls",
})
```

With a remote daemon, `/work`, `/project` and `/me` are Docker volumes on the sandbox host, and `Files()`, `Project()` and `Me()` reach them through `docker exec` in the workload container, so nothing outside the sandbox opens the run's files.

Inside an instance, the run's files are at `/work` (where commands start, and what `lease.Files()` reads and writes from the host), the project at `/project` (read only), the person volume at `/me`, and the socket directory shared with the connectors compartment at `/run/playscape`. Every zero `Spec` field is the strictest choice: no network, no extra mounts, a non-root user, small default limits. `docker.New` refuses a runtime that shares the host kernel (`ErrInsecureRuntime`) unless `Config.InsecureDevelopmentRuntime` is set, for machines without gVisor; it then warns on every `New`. `isolate/local` implements the same interface with no isolation at all, for tests. Every driver runs the `isolatetest` conformance suite, and CI runs it against Docker with both runc and runsc.

### Limits

- **Nothing is durable.** A task lives as long as the process that accepted it. Retried, replayable runs are the caller's job.
- **No per-task cancel.** Ending the context given to `Submit` stops the wait, not the task; `Close` cancels every running task at once.
- **Waiting on your own deployment can deadlock.** A tool that blocks on a sub-task it spawned holds its node; if every node waits, nothing runs. Size `Replicas` for it, or run sub-tasks on the parent's node.
- **Results are kept up to a bound** (1024 per deployment by default); `Result` reports `ErrUnknownTask` for one that has been forgotten.
- **The workspace policy is not process isolation.** It confines what a tool reaches through the workspace. `runner.Local` and `isolate/local` start host processes with your privileges; isolation needs `isolate/docker`.
- **The egress gate cannot inject into HTTPS it only tunnels.** A `CONNECT` is allowed or denied by host and port; what is inside is encrypted, so a placeholder sent through it arrives unchanged. Injection works for plain HTTP and for gate-terminated routes (the sandbox speaks HTTP to the gate, the gate speaks TLS upstream). The lease token is in the sandbox's environment; anyone on the egress network who learns it gets that lease's allowlist until `Revoke`. Instances on one egress network can reach each other, not only the gate.
- **Docker instances**: `/work` is a host directory and is not size limited by the driver; only `/tmp` is. Put `Config.WorkRoot` on a filesystem of its own (a sized tmpfs, a loop device or an XFS project quota) so a run cannot fill the host's disk. A command that times out restarts its compartment, which ends every other command running there. The daemon must be able to bind-mount `Config.WorkRoot` (with Docker Desktop or Colima, a directory shared with the VM). With runc (development only), the boundary is the host kernel's namespaces, which is not enough for hostile code. Register runsc with `sudo runsc install -- --oci-seccomp --host-uds=all`: without `--oci-seccomp` gVisor ignores the seccomp profile (its own kernel is still the boundary), and without `--host-uds=all` the compartments cannot connect over their socket directory. Under gVisor, Docker's embedded DNS is unreachable on hosts with nftables NAT, so name the egress gate by IP address in `Spec.Network.Proxy`. Under gVisor the driver sets the PIDs limit inside the sandbox with `docker exec --user 0 ... /bin/sh`, so the image needs `/bin/sh`. Debuggers and tracers (`ptrace`) do not work inside an instance.
- **Remote Docker daemons** (`Config.Host`): `/work`, `/project` and `/me` are volumes on the sandbox host, not size limited by the driver, so give the daemon's data root a filesystem of its own. `/project` is read only to the workload because root owns it, not through a read-only mount: the host writes it as root through the same container. `Files()`, `Project()` and `Me()` run a shell script per call, so the image needs `sh`, `cat`, `mkdir`, `dirname`, `rm`, `find` and `chmod` (busybox has them), and `Read` copies the file to a temporary file on your machine before returning it. Whoever holds the daemon's client certificate is root on the sandbox host, so give that host nothing else to lose.
- **Compartments** share one 1 MiB tmpfs directory, meant for sockets. Anything either side writes there, the other can read.
- **Pools** fix mounts at start, so a spec with a person volume warms only that person's instances. Warm instances are not health checked before a lease, and every spec a pool has seen stays warm until evicted.

## Development

```sh
go test -race ./sandbox/...       # the Docker driver's tests run where a daemon is reachable
SANDBOX_DOCKER_TEST_RUNTIME=runsc go test -race ./sandbox/isolate/docker/   # under gVisor, as CI does
go run ./examples/sandbox/
```

## Documentation

- [Sandbox architecture](../docs/architecture/sandbox.md)
- [AGENTS.md](../AGENTS.md): conventions and the sandbox rules
- [Contributing](../CONTRIBUTING.md)

## License

[Apache-2.0](../LICENSE)
