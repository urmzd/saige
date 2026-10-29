# 0026. Escalate on outcomes through the tree

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-26 in `DESIGN_DECISIONS.md`

## Decision

Some failures are signals about the model, not the request: structured output that never validates, or a sub-agent that fails.
An `OutcomePolicy` observes these outcomes and may return a `Switch` to another model.
The agent records an accepted switch as `ConfigPart{Target, Reason}` on the branch and sends a `RouteDelta` with the reason.
Recording it in the tree makes the switch hold for later turns, survive a reload, and appear in the audit trail.
A sub-agent failure is observed after the turn's tool results are recorded, so a switch never separates a call from its result.
`Structured` observes `schema_invalid` after its repairs run out, then repairs again on the new model, at most three switches per call.
A `Switch` can also carry dials, recorded as `ConfigPart{Dials}`, so a policy can raise reasoning depth on the same model before it moves to another one.
A switch is ignored only when it changes neither the model nor the dials, so a ladder that has reached its top ends the escalation.
Cancellation is not a failure and is never reported. A policy should be deterministic, because a durable run replays its outcomes.
The policy is not inherited by sub-agents. The parent observes a child's failure and decides for itself.
