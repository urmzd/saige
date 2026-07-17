package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Runner executes flows over experiments and writes one metrics document per
// experiment into <Experiment.Dir>/<MetricsFile>.
//
// Flows run in order and share one [FlowContext] per experiment, so later
// flows can seed from earlier ones. When Assemble is nil the runner writes a
// [DefaultMetrics] document; set Assemble to produce a custom schema from
// the collected flow results.
type Runner struct {
	Client      *Client
	Flows       []Flow
	Force       bool
	MetricsFile string // default "metrics.json"
	Assemble    func(exp Experiment, results map[string]FlowResult) (any, error)
}

// Run executes all flows for each experiment. Experiments whose metrics file
// already exists are skipped unless Force is set. Progress is logged to
// stderr. The first flow error aborts the run.
func (r *Runner) Run(ctx context.Context, experiments []Experiment) error {
	metricsFile := r.MetricsFile
	if metricsFile == "" {
		metricsFile = "metrics.json"
	}
	for i, exp := range experiments {
		metricsPath := filepath.Join(exp.Dir, metricsFile)
		if !r.Force {
			if _, err := os.Stat(metricsPath); err == nil {
				fmt.Fprintf(os.Stderr, "[%d/%d] skip %s (%s exists; use --force)\n", i+1, len(experiments), exp.ID, metricsFile)
				continue
			}
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] running %s\n", i+1, len(experiments), exp.ID)

		fc := NewFlowContext()
		results := make(map[string]FlowResult, len(r.Flows))
		for _, flow := range r.Flows {
			fmt.Fprintf(os.Stderr, "  %s flow...\n", flow.Name())
			result, err := flow.Run(ctx, r.Client, exp, fc)
			if err != nil {
				return fmt.Errorf("%s: %w", exp.ID, err)
			}
			results[flow.Name()] = result
			fc.Artifacts[flow.Name()] = result.Artifact
			t0 := result.Turn0
			fc.Turn0[flow.Name()] = &t0
		}

		doc, err := r.assemble(exp, results)
		if err != nil {
			return fmt.Errorf("%s: %w", exp.ID, err)
		}
		if err := WriteJSON(metricsPath, doc); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  wrote %s\n", metricsPath)
	}
	return nil
}

func (r *Runner) assemble(exp Experiment, results map[string]FlowResult) (any, error) {
	if r.Assemble != nil {
		return r.Assemble(exp, results)
	}
	return AssembleDefault(r.Client.Model, r.Flows, exp, results), nil
}

// DefaultMetrics is the generic per-experiment metrics document written when
// [Runner.Assemble] is nil. Flows is keyed by flow name; Comparison compares
// each non-first flow against the first flow in the runner's order.
type DefaultMetrics struct {
	ExperimentID string                `json:"experiment_id"`
	Model        string                `json:"model"`
	Provider     string                `json:"provider"`
	Timestamp    string                `json:"timestamp"`
	Format       string                `json:"format"`
	Flows        map[string]FlowReport `json:"flows"`
	Comparison   map[string]Comparison `json:"comparison,omitempty"`
}

// FlowReport is one flow's entry in [DefaultMetrics]: the turn-0 metrics,
// the aggregated flow metrics, and any flow-level Extra values flattened
// into the same JSON object. Extra keys must not collide with turn0 or the
// FlowMetrics field names.
type FlowReport struct {
	Turn0 TurnMetrics
	FlowMetrics
	Extra map[string]any
}

type flowReportFixed struct {
	Turn0 TurnMetrics `json:"turn0"`
	FlowMetrics
}

var flowReportFixedKeys = map[string]bool{
	"turn0":                     true,
	"per_turn":                  true,
	"total_input_tokens":        true,
	"total_output_tokens":       true,
	"total_cached_input_tokens": true,
	"total_latency_ms":          true,
}

// MarshalJSON flattens Extra into the same JSON object as the fixed fields.
func (f FlowReport) MarshalJSON() ([]byte, error) {
	fixed, err := json.Marshal(flowReportFixed{Turn0: f.Turn0, FlowMetrics: f.FlowMetrics})
	if err != nil {
		return nil, err
	}
	return appendExtra(fixed, f.Extra, flowReportFixedKeys)
}

// UnmarshalJSON restores the fixed fields and collects unknown keys into
// Extra, inverting MarshalJSON.
func (f *FlowReport) UnmarshalJSON(data []byte) error {
	var fixed flowReportFixed
	if err := json.Unmarshal(data, &fixed); err != nil {
		return err
	}
	extra, err := splitExtra(data, flowReportFixedKeys)
	if err != nil {
		return err
	}
	*f = FlowReport{Turn0: fixed.Turn0, FlowMetrics: fixed.FlowMetrics, Extra: extra}
	return nil
}

// AssembleDefault builds the [DefaultMetrics] document for one experiment.
// flows supplies the ordering: the first flow is the comparison baseline.
func AssembleDefault(model string, flows []Flow, exp Experiment, results map[string]FlowResult) DefaultMetrics {
	doc := DefaultMetrics{
		ExperimentID: exp.ID,
		Model:        model,
		Provider:     "openai-compatible",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Format:       exp.Format,
		Flows:        make(map[string]FlowReport, len(results)),
	}
	var baseline *FlowMetrics
	for i, flow := range flows {
		result, ok := results[flow.Name()]
		if !ok {
			continue
		}
		metrics := ToFlowMetrics(result.Turns)
		doc.Flows[flow.Name()] = FlowReport{
			Turn0:       result.Turn0,
			FlowMetrics: metrics,
			Extra:       result.Extra,
		}
		switch {
		case i == 0:
			baseline = &metrics
		case baseline != nil:
			if doc.Comparison == nil {
				doc.Comparison = map[string]Comparison{}
			}
			doc.Comparison[flow.Name()] = Compare(*baseline, metrics)
		}
	}
	return doc
}
