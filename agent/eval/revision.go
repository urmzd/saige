package eval

import (
	"encoding/json"
	"github.com/urmzd/saige/agent/provider/wrapper"
	topeval "github.com/urmzd/saige/eval"
)

// Revision identifies the judge's model, reported provider configuration,
// system prompt and conversion policy, including converter versions.
// Pin opaque provider/deployment changes with eval.WithJudgeRevision.
func (g *Generator) Revision() string { return topeval.RevisionTag("generator/v2", g.Configuration()) }

// Configuration exposes reported judge settings for experiment setup comparisons.
func (g *Generator) Configuration() json.RawMessage {
	converters := make([]string, 0, len(g.conversion.Converters))
	for _, c := range g.conversion.Converters {
		converters = append(converters, c.Name()+"@"+c.Version())
	}
	info := wrapper.Describe(g.provider)
	if info.Offering == nil {
		info.Offering = info.Capabilities.Offering
	}
	raw, _ := json.Marshal(map[string]any{"provider": info, "system": g.system, "modality": g.conversion.Dial, "converters": converters, "max_cost": g.conversion.MaxCost, "scope": g.conversion.Scope, "thinking": g.conversion.Thinking})
	return raw
}
