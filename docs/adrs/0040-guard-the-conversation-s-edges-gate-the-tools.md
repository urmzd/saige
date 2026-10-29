# 0040. Guard the conversation's edges, gate the tools

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #79 (commit 718402e), recorded as D-40 in `DESIGN_DECISIONS.md`

## Decision

Guardrails check what enters and leaves a conversation: the user's message and the final answer. Each returns pass, block, or rewrite, and an error fails closed. They run on the hook seam (D-39) and are recorded the same way, with the usage and receipts of any model call they make, so a replay neither calls nor charges a classifier twice.
A `ToolGate` decides whether a tool call runs (D-08, D-19). A guardrail never sees tool calls, and a gate never sees the user's message. An answer that arrives through a stop tool is a tool result, checked with an `AfterTool` hook.
Privacy redaction swaps values for placeholders that tools can restore (D-31). A guardrail rewrite is permanent and changes what is recorded. Redact input with a sequential guardrail when the model must never see a value, and with a vault when tools still need it.
A block is a tripwire: a typed `GuardrailTrippedError`, a `GuardrailDelta`, and a record in the tree. A blocked input is not recorded, and a blocked answer is not committed.
A parallel input guardrail races the first model call to save latency. When it blocks, the call is cancelled, its turn discarded, its usage charged, and the cancellation recorded. It cannot rewrite, because the model already has the text, so a rewrite counts as a block.
Output guardrails run after the answer streamed. A rewrite is announced with its replacement text; hosts that must never show raw output put `privacy.Provider` in front of the model.
Model calls a guardrail makes go through the run's budget, admitted and charged like a turn (D-09).
