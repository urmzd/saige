# 0036. Report malformed tool arguments to the model, truncated ones to the caller

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-36 in `DESIGN_DECISIONS.md`

## Decision

A tool call whose arguments the model finished writing but that do not decode ends with an argument error. The loop answers it with an "invalid tool arguments" result before any gate runs, so the model can correct the call.
A call cut off by the output token limit, or by a stream that ended early, is never closed. The turn fails as truncated or incomplete (D-23).
