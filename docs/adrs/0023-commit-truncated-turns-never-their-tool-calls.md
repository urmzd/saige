# 0023. Commit truncated turns, never their tool calls

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-23 in `DESIGN_DECISIONS.md`

## Decision

A turn that stops at the output token limit can end with a tool call whose arguments are cut off.
Running it could act on partial input. The loop therefore never runs any tool call from a truncated turn.
The completed text is committed and the stream sends `TruncatedDelta` with the node and the finish reason.
The committed node carries a `TruncationPart` marker with the reason. The tree stores it, and the loop strips it before the next provider call.
A truncated text-only turn ends the run cleanly unless `WithAutoContinue` or a queued message resumes it (D-25). The delta shows the answer is partial.
A truncated turn that requested tools ends the run with `ResponseTruncatedError`.
A truncated turn that kept nothing, no text and no tool call, also ends the run with `ResponseTruncatedError`, with `Empty` set. Nothing is committed and the delta names no node.
It is not a text-only turn: an empty answer would read as a finished one, and there is nothing for `WithAutoContinue` to resume, so it is never continued.
