# 0007. Default to private response-cache identity

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-07 in `DESIGN_DECISIONS.md`

## Decision

The SDK cannot infer a tenant's authorization scope or the meaning of a deployment revision.
A cache wrapper therefore uses a private identity unless the host supplies scope and configuration keys.
Explicit sharing remains possible, but the host owns that contract.
A model name alone is insufficient because two configurations of one model can produce different results.
Agent stores take tenant scope the same way: `pgstore.ScopedConversationID` and `pgstore.Config.Scope` place the scope as a namespace inside `conversation_id`, so two tenants never read each other's nodes.
