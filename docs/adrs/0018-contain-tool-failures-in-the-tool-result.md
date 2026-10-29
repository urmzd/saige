# 0018. Contain tool failures in the tool result

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-18 in `DESIGN_DECISIONS.md`

## Decision

A tool, gate, or handoff tool that panics produces an error result for that call. The process and the other runs continue.
Under a durable runner the panic fails the step instead, so the engine records an uncertain effect rather than a completed result.
Arguments that are not valid JSON, or that miss a required property or have the wrong top-level type, never reach the gate or the tool.
The model receives the reason and can correct the call.
A stream that ends with a tool call still open fails the turn with `ErrResponseTruncated`. Its incomplete arguments never run.
A file that cannot be resolved or extracted becomes a text notice and a logged warning, never silent omission or raw bytes.
