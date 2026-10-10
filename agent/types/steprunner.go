package types

import "context"

// StepRunner durably memoizes the result of an expensive, non-deterministic
// operation. On first execution it runs fn and records the result; on workflow
// replay (after a crash/restart) it returns the recorded result WITHOUT
// re-executing fn. This is the seam that lets the agent loop run inside a durable
// workflow engine without the core package depending on it.
//
// The default behavior is provided by NoopStepRunner, which simply calls fn
// inline: preserving non-durable, streaming behavior exactly.
type StepRunner interface {
	// RunStep executes (or replays) a named step returning a serializable
	// result. name must be stable and unique within a single loop run so the
	// runner can correlate replays to recorded results. fn takes a plain
	// context.Context to keep this interface free of any engine type.
	RunStep(ctx context.Context, name string, fn func(ctx context.Context) (StepResult, error)) (StepResult, error)
}

// StepKind discriminates the payload carried by a StepResult.
type StepKind string

const (
	StepKindLLM  StepKind = "llm"
	StepKindTool StepKind = "tool"
	// StepKindApproval records an approval policy's verdict on one call.
	StepKindApproval StepKind = "approval"
	// StepKindHook records the outcome of hooks or guardrails at one point.
	StepKindHook StepKind = "hook"
	// StepKindConvert records one media conversion: the parts it produced
	// and what it was billed, so a replay reuses them without calling the
	// converter's model again.
	StepKindConvert StepKind = "convert"
)

// StepFormatVersion is the format the durable engines record steps in. A
// step's message and tool output are stored through the shared part codec.
// Version 1 is the format of releases before typed parts, which the engines
// still read.
const StepFormatVersion = 2

// StepResult is the serializable payload a durable step records. It is a
// gob/JSON-encodable envelope that carries either an aggregated assistant
// message (Kind == StepKindLLM) or a tool-execution outcome (Kind ==
// StepKindTool). Using one concrete struct (rather than an `any`) keeps
// serializer registration trivial in the durable layer.
type StepResult struct {
	// V is the format a durable engine read the step from: StepFormatVersion,
	// or 1 for a step an earlier release recorded, upgraded on replay. It is
	// zero on a result that was not read from a record, and engines record
	// every step in the current format whatever it holds.
	V          int
	Receipt    *BudgetReceipt    // exact settlement, including uncertain charges
	Usage      *UsageDelta       // normalized usage retained for budget replay
	Kind       StepKind          // discriminator
	Message    *AssistantMessage // populated when Kind == StepKindLLM
	ToolCallID string            // populated when Kind == StepKindTool
	ToolResult string            // tool text projection / aggregated sub-agent text
	ToolParts  []ToolOutputPart  // rich tool output; survives durable replay
	// ToolCitations are the sources a rich tool attributed its output to,
	// tokenized and not yet numbered: the loop numbers them after the
	// step, so a replay numbers them the same way.
	ToolCitations []Citation
	ToolError     string           // non-empty => tool errored (recorded, not retried)
	Approval      *ApprovalVerdict // populated when Kind == StepKindApproval
	Hook          *HookRecord      // populated when Kind == StepKindHook
	Conversion    *ConversionEntry // populated when Kind == StepKindConvert
	// ConversionReceipts are the settlements of the conversions an LLM
	// step ran, so replaying the step restores their spend too.
	ConversionReceipts []BudgetReceipt
}

// NoopStepRunner runs steps inline with no memoization. It is the default,
// preserving today's streaming behavior and keeping all existing tests unchanged.
type NoopStepRunner struct{}

var _ StepRunner = NoopStepRunner{}

func (NoopStepRunner) RunStep(ctx context.Context, _ string, fn func(ctx context.Context) (StepResult, error)) (StepResult, error) {
	return fn(ctx)
}

type idempotentStepKey struct{}

// WithIdempotentStep marks the step run under ctx as idempotent: running it
// twice has the same effect as running it once. The agent loop sets it for a
// tool that declares itself idempotent (IdempotentTool). A durable engine may
// then repeat an attempt whose outcome a crash left unknown instead of
// waiting for the host to reconcile it.
func WithIdempotentStep(ctx context.Context) context.Context {
	return context.WithValue(ctx, idempotentStepKey{}, true)
}

// IdempotentStep reports whether ctx marks its step as idempotent.
func IdempotentStep(ctx context.Context) bool {
	v, _ := ctx.Value(idempotentStepKey{}).(bool)
	return v
}
