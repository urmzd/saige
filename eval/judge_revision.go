package eval

import (
	"encoding/json"
	"fmt"
)

// GeneratorRevision identifies a generator's declared configuration when it
// implements Revision() string, otherwise its implementation type. Pin opaque
// model/deployment settings with WithJudgeRevision or Revisioned.
func GeneratorRevision(gen Generator) string {
	if r, ok := gen.(interface{ Revision() string }); ok && r.Revision() != "" {
		return r.Revision()
	}
	return RevisionTag("generator/v1", fmt.Sprintf("%T", gen))
}

func judgeConfiguration(gen Generator, cfg *JudgeConfig) any {
	var generatorConfig json.RawMessage
	if c, ok := gen.(interface{ Configuration() json.RawMessage }); ok {
		generatorConfig = c.Configuration()
	}
	return map[string]any{"name": cfg.Name, "rubric": cfg.Rubric, "position_swap": cfg.PositionSwap, "revision": cfg.Revision,
		"generator_revision": GeneratorRevision(gen), "generator_config": generatorConfig,
		"prompt": judgePromptRaw, "pairwise_prompt": judgePairwisePromptRaw, "schema": JudgeSchema}
}
func judgeRevision(gen Generator, cfg *JudgeConfig) string {
	return RevisionTag("judge/v2", judgeConfiguration(gen, cfg))
}
