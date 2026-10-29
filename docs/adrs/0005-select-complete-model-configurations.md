# 0005. Select complete model configurations

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #59 (commit 1d1a557), recorded as D-05 in `DESIGN_DECISIONS.md`

## Decision

A route refers to an immutable provider configuration, not only a model string.
Each configuration has its own reasoning, sampling, endpoint, and cache settings.
The routing policy acts after context and tool selection, before provider execution.
Sticky selection is the default because repeated prefixes benefit from stable provider affinity.
A transient failure permits failover before output commits the attempt. Usage reported before any output is held back and dropped on failover, so one call never carries usage from two rate cards.
After commitment, the request fails rather than mixing streams from two providers.
Failover is classified: transient errors and context-length errors move on, and a context-length failure moves only to a larger context window. A refused prompt does not move unless the host says so.
The affinity policy leaves a profile only after a sustained run of failures, not one error, and returns to the preferred profile only when the cost of rewriting a warm cache prefix allows it.
Route locks keep a conversation where its state lives. A soft lock (an open tool loop, a bound context cache) blocks voluntary switches but allows failover. A hard lock (signed reasoning that must return to its model) also blocks failover and outranks a pin.
