# 0027. Disclose tools by search, permit them by gate

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-27 in `DESIGN_DECISIONS.md`

## Decision

Sending every schema on every turn costs tokens and lowers tool-choice accuracy once an agent has many tools.
`selector.DeferredTools` sends pinned tools and `tool_search`. A search returns the full schemas of its matches and discovers them for the rest of the conversation, keyed by owner, conversation, and the branch the run started on. The conversation is the tree's root node ID, which is random per tree, so agents that share a name and a policy, and repeated delegations to one sub-agent, never share discovery.
Without `tool_search` in the turn's tools nothing is hidden, because a tool with no route to it is unreachable.
Disclosure is not permission. A discovered tool passes the `ToolGate` on every call.
`Selector[T]` is the shared ranking seam. The default BM25 is lexical, local, and deterministic, so the same catalog and query always disclose the same tools.
`ToolDef.Capability` lets `CapabilityGate` decide by class. Reads run, writes ask, destructive calls are denied, and undeclared tools ask, so a tool that says nothing about itself fails closed.
`Budget.ToolQuota` caps calls per tool. A call is charged at dispatch, after the gate and any approval, so a denied call never uses the allowance. An exhausted quota is the call's error result, and sub-agents sharing the budget share the quota.
