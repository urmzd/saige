# 0041. Compose compaction strategies and record every compaction

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #76 (commit 0fe6898), recorded as D-41 in `DESIGN_DECISIONS.md`

## Decision

No single strategy fits every run. Clearing tool results is free but cannot shrink a conversation without tools; a summary always shrinks but costs a call and loses detail.
Strategies are therefore small values behind one interface (`types.CompactionStrategy`) and a `chain` applies them in order until the history fits. The turn-based strategies keep the system prompt and the original task and drop or summarize whole turns, so a tool call never loses its result. Every compacted history is still checked before it is used, and one that would separate a pair is discarded.
Relevance selection uses the same BM25 ranker as tool search: lexical, local and deterministic, so a replayed run selects the same messages.
A summary is a system message, so providers read it as context, not as words the user or the model said. It is written by a configurable provider or model and charged to the budget like a turn (D-09).
Compaction never deletes: the original branch keeps every message, and the new branch carries a record of the strategy, the trigger, the token counts and what happened to each message, by node. The same record is streamed as a delta.
Compaction is an operational setting, so subagents inherit it and may override it. Handoff groups keep rejecting every active strategy (D-11).
