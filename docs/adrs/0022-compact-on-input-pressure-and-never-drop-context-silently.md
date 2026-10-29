# 0022. Compact on input pressure and never drop context silently

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-22 in `DESIGN_DECISIONS.md`

## Decision

A message count does not predict whether a request fits the context window. One large tool result can exceed it.
`CompactConfig.MaxInputTokens` compacts before a turn whose input would exceed the limit.
The size is the larger of the previous turn's reported prompt tokens and a local estimate of the current history.
Summaries use the active member's provider. Each summary call is reserved on the budget before it is sent and settled after, like a turn. Compaction does not use an iteration.
A context-length error compacts the branch and retries the turn, at most three times. Then the first error is returned.
A compaction that fails is discarded and the original branch stays active.
The summary boundary moves so a tool result stays with its call: past the results of the last summarized call, or back before that call when moving forward would summarize everything.
A summary that would still separate a tool result from its call, such as one whose shared prefix ends inside a pair, is detected before the summary call. It is not attempted again until the branch grows.
The `none` strategy never makes a summary call, also after a context-length error.
`clear_tool_results` replaces old results with a stub and makes no model call. The full results remain on the original branch.
