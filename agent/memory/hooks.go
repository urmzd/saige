package memory

import (
	"context"
	"strings"

	"github.com/urmzd/saige/agent"
)

// ExtractionHook returns a RunStop hook that calls Policy.ExtractAfterRun
// after each run that finishes normally, with the agent that ran as owner
// and the run's branch as messages. The run ID and call path name the run,
// so the hook firing again for the same run stores nothing new.
//
// The hook runs after the run's branch is released and before its stream
// reports completion, bounded by the agent's hook timeout. A failure is
// logged; it never fails the run.
func ExtractionHook(store Store, policy Policy, ex Extractor) agent.Hooks {
	return agent.Hooks{
		Name: "memory-extraction",
		RunStop: func(ctx context.Context, ev *agent.RunStopEvent) error {
			if ev.Reason != agent.RunStopCompleted && ev.Reason != agent.RunStopTool {
				return nil
			}
			run := strings.Join(append([]string{ev.RunID}, ev.Path...), "/")
			_, err := policy.ExtractAfterRun(ctx, store, ex, ev.Agent, run, ev.Messages)
			return err
		},
	}
}
