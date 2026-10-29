# 0008. Keep policies separate

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-08 in `DESIGN_DECISIONS.md`

## Decision

Tool disclosure, execution permission, context selection, result selection, links, routing, and budgets use separate contracts.
A skill can supply content. It cannot grant permissions by itself.
A memory service can supply evidence. It cannot silently change system authority.
This allows a service to replace one policy without replacing the agent loop.
