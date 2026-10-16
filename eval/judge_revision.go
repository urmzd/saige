package eval

import "fmt"

// GeneratorRevision identifies a generator's declared configuration when it
// implements Revision() string, otherwise its implementation type. Pin opaque
// model/deployment settings with WithJudgeRevision or Revisioned.
func GeneratorRevision(gen Generator) string {
	if r, ok := gen.(interface{ Revision() string }); ok && r.Revision() != "" {
		return r.Revision()
	}
	return RevisionTag("generator/v1", fmt.Sprintf("%T", gen))
}

func judgeRevision(gen Generator, cfg *JudgeConfig) string {
	return RevisionTag("judge/v1", cfg.Name, cfg.Rubric, cfg.PositionSwap, cfg.Revision,
		GeneratorRevision(gen), judgePromptRaw, judgePairwisePromptRaw, JudgeSchema)
}
