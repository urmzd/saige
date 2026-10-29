# 0006. Separate cache contracts

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-06 in `DESIGN_DECISIONS.md`

## Decision

A response cache reuses output. A prompt cache reuses provider computation.
A durable step record prevents repeated work during recovery. A journal records authoritative changes.
These layers have different identity, expiry, billing, and correctness requirements.
No cache substitutes for a journal or a durable budget ledger.
The cache audit lists unsupported combinations and remaining defects.
