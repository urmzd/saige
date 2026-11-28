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
A truncated turn that kept no answer also ends the run with `ResponseTruncatedError`, with `Empty` set: it kept nothing at all, or only thinking, run metadata, blank text and the calls and results of tools the provider ran.
It is not a text-only turn: an empty answer would read as a finished one, and there is nothing for `WithAutoContinue` to resume, so it is never continued.
Nothing of it is committed, its thinking included, and the delta names no node. A thinking part with no answer after it is not a turn a later request can carry, so the branch is left ending where the failed call began.
The call of an automatic continuation is the exception: it has a partial answer behind it. When it is cut short with no answer added, the run ends without an error on that partial answer. The continue prompt it was asked with joins the branch only with an answer, so the branch ends with the partial answer and `Continue` can resume it.
A forced final answer that is cut short this way ends the run with an error that matches both the limit's error and `ErrResponseTruncated`.
