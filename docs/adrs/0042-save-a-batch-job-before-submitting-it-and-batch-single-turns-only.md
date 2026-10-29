# 0042. Save a batch job before submitting it, and batch single turns only

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #82 (commit 1d6c7f3), recorded as D-42 in `DESIGN_DECISIONS.md`

## Decision

A batch job record (vendor batch ID, request manifest hash, request IDs, state) is saved before the vendor sees the batch, and Submit is idempotent by job ID: a restart resumes the batch instead of paying for it again.
A submit interrupted before its batch ID was saved is looked up by tag or request ID where the vendor allows. When the lookup cannot tell, the job stops as indeterminate until the host reconciles it, as D-14 requires, rather than risk a duplicate batch.
Batch requests are validated as interactive requests are, and a control the batch endpoint cannot take is rejected at submit (D-12). Budget is reserved per request at submit at the batch rate card and released for requests the vendor does not bill (D-09).
Only single-turn work is batched. Each turn of an agent loop depends on the last, so a batched loop would wait a batch per turn.
