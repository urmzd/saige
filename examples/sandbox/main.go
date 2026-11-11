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
