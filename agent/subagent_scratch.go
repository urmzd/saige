package agent

import (
	"context"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// SubAgentScratch configures a sub-agent's private scratch workspace.
//
// By default each invocation of a sub-agent gets a fresh workspace.Memory.
// The child's workspace is then that scratch layered over a read-only view
// of the parent's (see workspace.Layers): its writes stay private, and it
// still reads what the parent shared. Siblings never see each other's
// scratch, and neither does a later invocation of the same definition.
//
// Lifetime: the scratch lives as long as something holds it. The parent's
// run keeps a read-only view of every child scratch it received a result
// from until the run ends, so its read_artifact and search_artifact calls
// can follow a child's references; SubAgentResult.Scratch and
// SubAgentHandle.Scratch give the host the same view. An in-memory scratch
// needs no cleanup: it is freed when the last of these is dropped. A scratch
// made by New belongs to the host, which removes it, for example a
// workspace.Dir directory, once it no longer needs the child's results.
type SubAgentScratch struct {
	// Off gives the child no scratch: it sees the parent's workspace
	// read-only, or nothing, and gets no scratch tools. Large inputs then
	// stay inline, since there is nowhere to put them.
	Off bool
	// NoTools keeps the scratch, for references, spilled results, and tools
	// the child already has, but does not add scratch_write, scratch_read,
	// and scratch_search. A child with no tools never gets them: it answers
	// in one turn and has no use for notes.
	NoTools bool
	// New creates the scratch for one invocation. name is the definition's
	// Name and id the delegating tool call's ID, which is unique within a
	// run. nil uses workspace.NewMemory. A host can return a workspace.Dir
	// under its own root to keep children's scratch on disk.
	New func(ctx context.Context, name, id string) (workspace.Workspace, error)
}

// open returns the scratch for one invocation, or nil when it is off.
func (s SubAgentScratch) open(ctx context.Context, name, id string) (workspace.Workspace, error) {
	if s.Off {
		return nil, nil
	}
	if s.New == nil {
		return workspace.NewMemory(), nil
	}
	return s.New(ctx, name, id)
}

// registerScratchTools adds the scratch tools to registry, keeping any tool
// of the same name the definition already registered. They act on the
// workspace attached to each call, which for a child is its layered view.
func registerScratchTools(registry *types.ToolRegistry) {
	registerMissing(registry, workspace.Tools(nil)...)
}

// registerArtifactTools adds read_artifact and search_artifact to registry,
// keeping any tool of the same name already there.
func registerArtifactTools(registry *types.ToolRegistry) {
	registerMissing(registry, workspace.ArtifactTools(nil)...)
}

// isArtifactTool reports whether name is read_artifact or search_artifact.
func isArtifactTool(name string) bool {
	return name == workspace.ReadArtifactToolName || name == workspace.SearchArtifactToolName
}

func registerMissing(registry *types.ToolRegistry, tools ...types.Tool) {
	for _, t := range tools {
		if _, ok := registry.Get(t.Definition().Name); !ok {
			registry.Register(t)
		}
	}
}

// Scratch returns a read-only view of the child's private scratch, or nil
// when the definition turned scratch off. It is safe to read while the
// child runs.
func (h *SubAgentHandle) Scratch() workspace.Workspace {
	return h.scratch
}

// attachScratch lets this run's tools read a child's scratch.
func (s *EventStream) attachScratch(ws workspace.Workspace) {
	if s.artifacts != nil && ws != nil {
		s.artifacts.Attach(ws)
	}
}
