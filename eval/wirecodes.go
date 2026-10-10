package eval

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("eval.infra", ErrInfra)
	agenttypes.RegisterWireSentinel("eval.inline_input", ErrInlineInput)
	agenttypes.RegisterWireSentinel("eval.judge_media", ErrJudgeMedia)
	agenttypes.RegisterWireSentinel("eval.no_judge_score", ErrNoJudgeScore)
	agenttypes.RegisterWireSentinel("eval.not_applicable", ErrNotApplicable)
	agenttypes.RegisterWireSentinel("eval.output_not_json", ErrOutputNotJSON)
	agenttypes.RegisterWireSentinel("eval.unknown_scorer", ErrUnknownScorer)
	agenttypes.RegisterWireSentinel("eval.unlocated_media", ErrUnlocatedMedia)
}
