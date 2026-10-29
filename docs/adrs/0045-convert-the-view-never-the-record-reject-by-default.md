# 0045. Convert the view, never the record; reject by default

- **Status:** Accepted
- **Date:** 2026-10-10
- **Source:** PR #97 (commit 8a1726a), recorded as D-45 in `DESIGN_DECISIONS.md`

## Decision

A part the serving model cannot take natively is never dropped or replaced by a placeholder. Each attempt plans every part against the offering that serves it: native, lowered from a tool result by the adapter, converted by an action the modality dial permits, or rejected. Reject is the default, and an action is permitted only by naming it, as a contractual dial is loosened only by naming it (D-12). Omitting a part is one of those actions, and it leaves a notice in its place.
The plan is pure: it reads the offering's modalities, limits and locators and asks converters for estimates, and does no I/O. A router plans each member while it filters candidates, so a member that would reject leaves the request and a member that would convert stays; failover plans again for the next member, whose modalities may differ.
Conversions change only the copy sent to the provider. The conversation keeps the original parts, so a later turn on a model that reads the media gets it as it is, and the record never holds text a converter made as if the user had written it.
Because history is sent every turn, conversions are memoized by scope, converter version and digest, run as durable steps so a replay never runs or bills them twice, and charged to the attempt's budget reservation (D-09). The executed report is streamed before the attempt's output and kept on failover, because its cost is real, and the serving attempt's report is saved with the turn.
The same planner keeps reasoning a target cannot verify out of the view: a thinking part signed by another vendor, or unsigned, is not replayed to an API that checks signatures.
