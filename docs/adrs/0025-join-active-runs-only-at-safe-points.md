# 0025. Join active runs only at safe points

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-25 in `DESIGN_DECISIONS.md`

## Decision

A message for an active run must never land between a `tool_use` and its `tool_result`.
`EventStream.Submit` and `Agent.Submit` hold it until a safe point: the top of an iteration, after tool results, or where the run would finish.
Queue waits for the finish and starts a new user turn on the same stream with fresh step limits. Steer joins at the next safe point and cancels nothing.
InterruptReplace cancels only the provider call in flight. Its completed text is committed as a truncated turn and its tool calls are dropped. Tools already running finish first. The replacing message starts a new user turn with fresh step limits, so it is always answered.
Side branches from the last safe point and runs separately, so it never writes to the original branch.
Every accepted message is either appended, with `InjectedDelta`, or returned by `Undelivered` after the run ends. A message sent after the final safe point fails with `ErrRunFinished`.
`Continue` resumes the last assistant turn with prefill when the provider declares it, and otherwise with a continue prompt. `WithAutoContinue` does the same for a text-only turn cut off by the output limit. A cut-off tool call still fails the run.
Runs without a consumer, and runs under a durable step runner, do not take submissions, because their transcript must replay without outside input.
A run that moves to a compacted branch extends its claim to it, so a message addressed to the active branch joins that run rather than starting a second one.
