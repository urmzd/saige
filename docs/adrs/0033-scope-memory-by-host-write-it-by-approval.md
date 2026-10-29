# 0033. Scope memory by host, write it by approval

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-33 in `DESIGN_DECISIONS.md`

## Decision

The host maps the calling agent to a memory scope. The model never names a tenant, and a call without a resolvable scope fails.
Memory writes are tool calls that carry an approval marker by default. Automatic extraction belongs in a host hook after the run, through `Policy.Remember`, never inline.
Every write passes the policy's content check. Without `Policy.Redact`, content in which the detector finds anything is rejected, so a missing redactor fails closed.
A tool write uses the tool call ID as its idempotency key, so a replayed call stores one record.
Recall is external input. Records are wrapped in a tag whose name ends in a digest of the content, so stored text cannot close it, and injected recall is a user message, never system content.
A sub-agent should get a read-only, narrowed scope. A narrowed scope sees its own namespace and those below it, never its parent's.
