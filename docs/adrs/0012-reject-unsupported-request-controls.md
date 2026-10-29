# 0012. Reject unsupported request controls

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #60 (commit b0790a3), recorded as D-12 in `DESIGN_DECISIONS.md`

## Decision

Adapters validate configured controls before they send a request.
For example, temperature on an `o3` request returns `ErrInvalidModelConfig`.
The adapter does not silently remove the requested value.
This makes a configuration error visible and prevents retrying the same invalid request.
Each route must have settings that its own model accepts.
Validation preserves prompt cache options and reported cache usage.

Raw request options are rejected, never stripped. Dials are intents, not vendor parameters: an advisory dial (creativity, reasoning, cache, parallel on) that a model cannot honor is mapped to the nearest value the model declares, or dropped. Max output is lowered to the model's limit, never raised. A contractual dial (tool choice, parallel off, seed) is rejected, and a response schema stays under D-21. Each decision is recorded as data on the route, the tree, the span and the eval, never only as a log line.
A raw option that sets the same parameter as a dial wins and is validated strictly. Reasoning outranks creativity: when the effective reasoning rules out sampling controls, creativity is dropped and recorded.
A `DialPolicy` can tighten any dial to reject, and `StrictDials` rejects every dial a model cannot honor exactly. A contractual dial is loosened only by naming it. There is no global lenient switch.
Dials compile per attempt, against the attempt's model and request, so failover re-targets them. In a router, a contractual rejection removes a member from the request; an advisory decision never does.
