# 0003. Treat handoff as ownership transfer

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-03 in `DESIGN_DECISIONS.md`

## Decision

A handoff recipient can finish, continue elsewhere, or transfer back when it cannot answer.
There is no mandatory return to a waiting caller.
The audit tree retains all messages. The default provider view contains only the owner's context and transfer briefs.
A returning owner resumes its earlier view.
This supports triage without copying every specialist's transcript into every request.
