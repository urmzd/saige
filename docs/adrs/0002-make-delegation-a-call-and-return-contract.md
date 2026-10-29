# 0002. Make delegation a call-and-return contract

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-02 in `DESIGN_DECISIONS.md`

## Decision

A subagent always returns to its caller. Its default output is the final assistant message.
The complete committed trace remains available through a result handle or sink.
A result policy selects and validates data for an upstream service.
This prevents intermediate thoughts from becoming an apparent final answer.
A failed child returns an error and partial trace, not successful output.
Spawn mode keeps the return but makes it asynchronous. D-29 records how.
