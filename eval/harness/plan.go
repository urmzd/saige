package harness

import (
	"context"
	"os"
	"path/filepath"
)

// Planned actions of a script in a [PlannedScript].
const (
	PlanRun    = "run"    // the script runs
	PlanSkip   = "skip"   // its metrics file exists and Force is off
	PlanResume = "resume" // its units are carried from the resumed run
)

// PlannedScript is what [Runner.Run] would do with one script.
type PlannedScript struct {
	ID     string `json:"id"`
	Turns  int    `json:"turns"`
	Action string `json:"action"`
	// Calls is the number of chat calls the script would make, counting
	// one per turn per built-in flow, less the synthesis call a stateless
	// flow reuses from an earlier base flow. Repairs and retries are not
	// counted. Exact is false when a flow is not built in, since its calls
	// cannot be known in advance; such flows add nothing to Calls.
	Calls int  `json:"calls"`
	Exact bool `json:"exact"`
}

// Plan reports what Run would do with each script without calling a model or
// writing anything: run it, skip it, or carry it from the resumed run. With
// Resume set it reads the resumed run from Results.
func (r *Runner) Plan(ctx context.Context, scripts []Script) ([]PlannedScript, error) {
	var carried map[string]bool
	if r.Resume != "" {
		if r.Results == nil {
			return nil, errNoResumeStore(r.Resume)
		}
		completed, err := r.completedScripts(ctx)
		if err != nil {
			return nil, err
		}
		carried = make(map[string]bool, len(completed))
		for id := range completed {
			carried[id] = true
		}
	}
	metricsFile := r.MetricsFile
	if metricsFile == "" {
		metricsFile = "metrics.json"
	}
	out := make([]PlannedScript, 0, len(scripts))
	for _, script := range scripts {
		p := PlannedScript{ID: script.ID, Turns: len(script.Turns), Action: PlanRun, Exact: true}
		switch {
		case carried[script.ID]:
			p.Action = PlanResume
		case !r.Force && r.Resume == "" && fileExists(filepath.Join(script.Dir, metricsFile)):
			p.Action = PlanSkip
		default:
			p.Calls, p.Exact = plannedCalls(r.Flows, len(script.Turns))
		}
		out = append(out, p)
	}
	return out, nil
}

// plannedCalls counts the chat calls of the built-in flows over a script
// with the given number of turns.
func plannedCalls(flows []Flow, turns int) (int, bool) {
	calls, exact, baseRan := 0, true, false
	for _, flow := range flows {
		switch flow.(type) {
		case BaseFlow, *BaseFlow:
			calls += turns
			baseRan = true
		case StatelessFlow, *StatelessFlow:
			calls += turns
			if baseRan && turns > 0 {
				calls--
			}
		default:
			exact = false
		}
	}
	return calls, exact
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
