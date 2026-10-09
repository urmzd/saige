package types

import "context"

// ToolRedactor keeps sensitive values out of everything the model sees while
// tools still receive real values. The agent loop calls RestoreArgs just
// before a tool executes, after gates and approvals have seen the
// placeholders, and TokenizeResult on the result before it is recorded,
// streamed, or sent back to the provider. The provider, the tree, telemetry,
// and durable snapshots therefore hold only placeholders.
//
// It is a separate policy from ToolGate: a gate decides whether a call runs,
// a redactor decides what the call and its result look like on each side of
// the tool boundary.
type ToolRedactor interface {
	// RestoreArgs returns a copy of args with placeholders replaced by the
	// values they stand for. It must not modify args.
	RestoreArgs(ctx context.Context, def ToolDef, args map[string]any) map[string]any
	// TokenizeResult returns r with sensitive values replaced by placeholders.
	// An implementation that cannot redact a result must withhold it rather
	// than return it unredacted.
	TokenizeResult(ctx context.Context, def ToolDef, r ToolResult) ToolResult
}

// ToolCallInfo identifies the tool call a context belongs to. The agent loop
// attaches it before a tool executes, so a tool can derive a stable
// idempotency key from the call ID (the durable step for a call is named after
// it) and know which agent invoked it.
type ToolCallInfo struct {
	ID    string // tool call ID assigned by the provider
	Name  string // tool name
	Agent string // name of the agent that owns the turn
	// RunID identifies the root run of the delegation tree the call belongs
	// to, and Branch the conversation branch the run extends.
	RunID  string
	Branch BranchID
}

type toolCallInfoKey struct{}

// WithToolCallInfo returns ctx carrying info.
func WithToolCallInfo(ctx context.Context, info ToolCallInfo) context.Context {
	return context.WithValue(ctx, toolCallInfoKey{}, info)
}

// ToolCallInfoFromContext returns the tool call attached to ctx.
func ToolCallInfoFromContext(ctx context.Context) (ToolCallInfo, bool) {
	info, ok := ctx.Value(toolCallInfoKey{}).(ToolCallInfo)
	return info, ok
}
