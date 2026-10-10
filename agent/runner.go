package agent

import "context"

// Runner drives a multi-turn conversation with an Agent.
// Implementations own the interaction loop: reading input,
// invoking the agent, rendering deltas, resolving markers,
// and deciding when to stop.
type Runner interface {
	Run(ctx context.Context, agent *Agent) error
}

// NamedRunner is an optional extension for runners that have a name.
type NamedRunner interface {
	Runner
	Name() string
}

// RunFunc adapts a plain function into a Runner.
type RunFunc func(ctx context.Context, agent *Agent) error

// Run calls f.
func (f RunFunc) Run(ctx context.Context, agent *Agent) error { return f(ctx, agent) }

// Run starts an agent with the given Runner.
func Run(ctx context.Context, agent *Agent, runner Runner) error {
	return runner.Run(ctx, agent)
}
