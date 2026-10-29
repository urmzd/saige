# 0039. Give hooks one seam and record what they change

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #79 (commit 718402e), recorded as D-39 in `DESIGN_DECISIONS.md`

## Decision

`agent.Hooks` is the single place host code observes a run: run start and stop, user input, model calls, tools, compaction, turns, sub-agents, and interrupts. Sets run in the order they were added and sub-agents inherit them, like the gate (D-08).
Most points only observe. A point may change its event or abort the run only where the loop can absorb it: a changed message, arguments, result, or task, a skipped compaction, or an abort at a safe point. An aborted tool call still gets a result, so pairing holds (D-18), and hooks never add or remove messages.
Each call is bounded by a timeout, and a panic or a late return is a failure. A failure aborts at an abortable point and is logged at an observing one. The agent waits for a hook rather than abandon it, because a hook still running would race with the run.
Under a durable runner the outcome of every changing or aborting point is a recorded step, so a replay applies it without calling the hook (D-14). Observing hooks run again on replay.
Post-run work, such as the memory extraction D-33 calls for, runs in `RunStop`, after the branch is released and on a context the run's cancellation does not reach.
