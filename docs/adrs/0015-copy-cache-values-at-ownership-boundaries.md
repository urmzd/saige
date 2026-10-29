# 0015. Copy cache values at ownership boundaries

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #62 (commit 9d39891), recorded as D-15 in `DESIGN_DECISIONS.md`

## Decision

A read must not let one caller alter another caller's future result.
Tool results and embedding vectors are copied before storage and before return.
Embedding keys include the full input, including binary data and MIME type.
Tool keys include a configuration revision, scope, policy, arguments, and declared context.
Invalid key data fails explicitly. No fallback string is treated as a stable identity.
Stale data stays in storage through its stale window, but only a successful old result can mask a refresh failure.
