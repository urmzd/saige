package types

import "context"

// StepRunner durably memoizes the result of an expensive, non-deterministic
// operation. On first execution it runs fn and records the result; on workflow
// replay (after a crash/restart) it returns the recorded result WITHOUT
// re-executing fn. This is the seam that lets the agent loop run inside a durable
// workflow engine (e.g. DBOS) without the core package depending on it.
//
// The default behavior is provided by NoopStepRunner, which simply calls fn
// inline — preserving non-durable, streaming behavior exactly.
type StepRunner interface {
	// RunStep executes (or replays) a named step returning a serializable
	// result. name must be stable and unique within a single loop run so the
	// runner can correlate replays to recorded results. fn takes a plain
	// context.Context (NOT a durable context) to match the dbos.Step shape and
	// to keep this interface free of any engine type.
	RunStep(ctx context.Context, name string, fn func(ctx context.Context) (StepResult, error)) (StepResult, error)
}

// StepKind discriminates the payload carried by a StepResult.
type StepKind string

const (
	StepKindLLM  StepKind = "llm"
	StepKindTool StepKind = "tool"
)

// StepResult is the serializable payload a durable step records. It is a
// gob/JSON-encodable envelope that carries either an aggregated assistant
// message (Kind == StepKindLLM) or a tool-execution outcome (Kind ==
// StepKindTool). Using one concrete struct (rather than an `any`) keeps
// serializer registration trivial in the durable layer.
type StepResult struct {
	Kind       StepKind          // discriminator
	Message    *AssistantMessage // populated when Kind == StepKindLLM
	ToolCallID string            // populated when Kind == StepKindTool
	ToolResult string            // tool text projection / aggregated sub-agent text
	ToolBlocks []ToolResultBlock // rich tool output; survives durable replay
	ToolError  string            // non-empty => tool errored (recorded, not retried)
}

// NoopStepRunner runs steps inline with no memoization. It is the default,
// preserving today's streaming behavior and keeping all existing tests unchanged.
type NoopStepRunner struct{}

var _ StepRunner = NoopStepRunner{}

func (NoopStepRunner) RunStep(ctx context.Context, _ string, fn func(ctx context.Context) (StepResult, error)) (StepResult, error) {
	return fn(ctx)
}
