# 0024. Force behavior for one turn and bound unproductive loops

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-24 in `DESIGN_DECISIONS.md`

## Decision

A forced tool choice that stays in effect makes the model call the same tool on every turn.
A required or named choice therefore applies to one turn and then reverts to auto. Auto and none stay in effect.
A forced choice must reach the provider through `OptionsProvider`; otherwise the request is rejected. None is emulated by withholding tools.
Three guards stop a run that does not progress: the iteration cap, `MaxConsecutiveErrors` (default 2), and `MaxRepeatIterations`.
A call that a gate or a person refuses is a policy outcome, not a tool fault. It does not count toward `MaxConsecutiveErrors`.
Repeated calls are answered with an error result and not run, so each call keeps a result.
`OnMaxIter: MaxIterForceFinal` replaces the guard's error with one tool-free call that asks for a final answer.
That instruction is sent with the request only. The tree records the answer, not the instruction.
