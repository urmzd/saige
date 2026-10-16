package eval

import (
	"github.com/urmzd/saige/agent/provider/wrapper"
	topeval "github.com/urmzd/saige/eval"
)

// Revision identifies the judge's model, reported provider configuration,
// system prompt and conversion policy, including converter versions.
// Pin opaque provider/deployment changes with eval.WithJudgeRevision.
func (g *Generator) Revision() string {
	converters := make([]string, 0, len(g.conversion.Converters))
	for _, c := range g.conversion.Converters {
		converters = append(converters, c.Name()+"@"+c.Version())
	}
	info := wrapper.Describe(g.provider)
	if info.Offering == nil {
		info.Offering = info.Capabilities.Offering
	}
	return topeval.RevisionTag("generator/v1", info, g.system,
		g.conversion.Dial, converters, g.conversion.MaxCost, g.conversion.Scope, g.conversion.Thinking)
}
