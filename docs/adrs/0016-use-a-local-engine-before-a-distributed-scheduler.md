# 0016. Use a local engine before a distributed scheduler

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #62 (commit 9d39891), recorded as D-16 in `DESIGN_DECISIONS.md`

## Decision

The local engine uses process locks, synced files, and atomic rename.
This gives one machine a testable crash boundary without a queue or database deployment.
An OS lock releases when its process exits. Pending approvals need no resident worker.
Snapshots use private files and contain versioned recovery data plus ordered event metadata.
They are not a portable trace format or an unbounded production journal.
The anchor engine is the distributed option. anchor supplies fenced leases, a queue, and a write-once record ledger on Postgres tables the host owns.
Retention of finished runs remains a host policy.
See [durable execution](../architecture/durable-execution.md) for the failure table and deployment limits.
