# 0038. Let approvals grant scope, and record every grant

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #68 (commit 0521163), recorded as D-38 in `DESIGN_DECISIONS.md`

## Decision

Asking about every held call trains people to approve without reading. An `ApprovalPolicy` lets an approval carry a grant: once, the tool, matching arguments, or the conversation, with an optional expiry. Later calls the grant covers run without asking.
Only the host creates grants, from the decisions it delivers. The model's text and arguments, tool and skill output, and gates cannot (D-08, D-28).
Grants never cover a destructive tool, and neither does the opt-in approval ramp. A tool people denied too often is refused with the reason, or hidden from the model.
Each decision is recorded as metadata in the tree next to the call's result, and the policy's state is rebuilt from those records at the start of each run, so a restored conversation decides alike. Compaction writes the whole state onto its new branch as one snapshot record. A turn that ends before its results are written loses that turn's records, which at worst asks again.
Under a durable runner the verdict on each call is a recorded step, so a replay decides the same way after a grant expired. The decision that created a grant is saved with the approval.
State is per conversation. A sub-agent starts with none, so a grant never crosses a delegation.
