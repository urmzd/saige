# Context management

Long runs outgrow the context window. Compaction moves a run onto a new, shorter branch of its conversation tree before a turn, and the original branch keeps every message. This guide covers the strategies, how to combine them, when they run, what each compaction records, and how the policy works across subagents and handoff groups.

- [Quick start](#quick-start)
- [Strategies](#strategies)
- [Chains](#chains)
- [When compaction runs](#when-compaction-runs)
- [Summaries and the budget](#summaries-and-the-budget)
- [Records and deltas](#records-and-deltas)
- [Policy per agent](#policy-per-agent)
- [Presets](#presets)

## Quick start

```go
a := agent.NewAgent(agent.AgentConfig{
	Provider:     model,
	SystemPrompt: "You are a research assistant.",
	CompactCfg: &types.CompactConfig{
		Strategy:       types.CompactChain,
		MaxInputTokens: 150_000,
		TargetTokens:   100_000,
		Chain: []types.CompactConfig{
			{Strategy: types.CompactClearToolResults, KeepToolResults: 3},
			{Strategy: types.CompactRelevantPlusSummary, KeepTurns: 4, SelectK: 3, SummaryModel: "claude-haiku-5-5"},
		},
	},
})
```

Before a turn whose input exceeds 150,000 tokens, the chain clears old tool results. If the history is still over 100,000 tokens, it keeps the last four turns and the three older spans most relevant to the latest user message, and replaces the rest with a summary written by claude-haiku-5-5.

## Strategies

A **turn** is an assistant message with the tool results that answer it, together with the user messages directly before it. The **head** is the system prompt and the original task (the first user message). No strategy separates a tool call from its result, and every compacted history is checked again before it is used: one that would separate them is discarded and the run continues on the original branch.

| Strategy | Config | Keeps | Model call |
| --- | --- | --- | --- |
| `keep_recent` | `KeepTurns` (default 4) | The head, earlier summaries and the last `KeepTurns` turns. The rest is dropped | No |
| `summary` | `KeepTurns` (default 4), `Threshold` | The head and the last `KeepTurns` turns. Everything between them, earlier summaries included, becomes one summary | Yes, one |
| `relevant_plus_summary` | `KeepTurns` (default 4), `SelectK` (default 3), `Threshold` | The head, the last `KeepTurns` turns, and the `SelectK` older spans that best match the latest user message by BM25. The rest becomes one summary | Yes, one |
| `clear_tool_results` | `KeepToolResults` (default 3), `ExcludeTools` | Every message. Older tool results are replaced by a stub naming the tool and call | No |
| `chain` | `Chain`, `TargetTokens` | Whatever its steps keep | When a step makes one |
| `sliding_window` | `WindowSize` | The system prompt and the last `WindowSize` messages | No |
| `summarize` | `Threshold`, `KeepLast` | Under `MaxInputTokens`, the newer half of the branch after a summary of the older half. Otherwise the last `KeepLast` messages after a summary | Yes, one |
| `none` | | Everything. Compaction is off | No |

A summary written by `summary` or `relevant_plus_summary` is a system message that starts with `types.CompactionSummaryPrefix`. Providers read it as context, not as something the user or the model said. A later compaction folds it into the next summary, so a branch holds at most one.

`relevant_plus_summary` ranks with the BM25 ranker of `agent/selector`. A span is one message with the tool results that answer it, so a selected tool call keeps its results. Selected spans are kept verbatim, in their original order, after the summary. The query is the latest user message that is not only tool results.

Each strategy is also a Go value that implements `types.CompactionStrategy` and `types.Compactor`: `types.NewKeepRecent(n)`, `&types.Summary{...}`, `&types.RelevantPlusSummary{...}`, `types.NewClearToolResultsCompactor(...)` and `types.Chain{...}`. `CompactConfig.ToStrategy()` builds one from a config.

## Chains

A chain applies its steps in order. With a target, it measures the history before each step and stops as soon as it fits. The target is `TargetTokens`, or `MaxInputTokens` when that is zero. Without either, the chain applies every step. A step that fails is skipped and the next one runs on the history the earlier steps left.

Put cheap steps first: `clear_tool_results` makes no model call and often recovers most of the space in a tool-heavy run. End with a step that always shrinks the history, such as `summary` or `keep_recent`.

## When compaction runs

| Trigger | When it fires | `CompactionContent.Trigger` |
| --- | --- | --- |
| `MaxInputTokens` set | Before a turn whose input exceeds it. Input is the larger of the last reported prompt tokens and the tokenizer's estimate (`AgentConfig.Tokenizer`). The turn is compacted again while it is still over and the last compaction shrank it, up to 5 times | `input_pressure` |
| No `MaxInputTokens` | Before every turn, by the strategy's own rule: `keep_recent` past `KeepTurns` turns, `summary` and `relevant_plus_summary` past `Threshold` messages (never, when `Threshold` is 0), `sliding_window` past `WindowSize`, `summarize` past `Threshold`, `clear_tool_results` past `KeepToolResults` results | `rule` |
| `ConfigContent.CompactNow` | Before the next turn, whatever the size or rule | `requested` |
| Context-length error | The provider rejected the turn. The branch is compacted and the turn retried, up to 3 times, then the first error is returned | `context_length` |

Without `MaxInputTokens`, `keep_recent` and `clear_tool_results` compact again on every turn once the history passes their rule, each time onto a new branch. Set a token limit for long runs. A `BeforeCompaction` hook runs before every strategy and can skip the compaction; `AfterCompaction` reports whether it happened and the new branch ([hooks](hooks.md)). Compaction never counts as an iteration. A `ConfigContent` with `Compact` set changes the strategy from that point in the branch.

## Summaries and the budget

The summary is written by, in order of precedence:

1. `AgentConfig.CompactProvider` (or `agent.WithCompactProvider(p)`), for example a cheaper adapter;
2. the active provider switched to `CompactConfig.SummaryModel`, through `ModelSwitcher` as a `ConfigContent.Model` switch would be (a router built from a preset accepts another preset or profile name);
3. the active provider.

Every summary call is reserved on `AgentConfig.Budget` before it is sent and settled when it ends, like a turn, and its usage is streamed as a `UsageDelta`. A budget that refuses the call ends the run, and nothing is sent. `Budget.Breakdown()` reports summary spend under the summary model.

## Records and deltas

Every compaction writes a `types.CompactionContent` record onto the branch it created and streams a `types.CompactionDelta` (wire kind `compaction`) with the new branch and the record's node.

| Field | Meaning |
| --- | --- |
| `Strategy` | The configured strategy, for example `chain(clear_tool_results,relevant_plus_summary)` |
| `Steps` | The strategies that changed the history, in order |
| `Trigger` | `rule`, `input_pressure`, `requested` or `context_length` |
| `TokensBefore`, `TokensAfter` | The model-visible history measured with the agent's tokenizer |
| `FromBranch` | The branch that was compacted. It keeps every message |
| `Kept`, `Selected`, `Cleared` | Nodes of `FromBranch` carried over verbatim, kept because relevance selection chose them, or kept with tool results cleared |
| `Summarized`, `Dropped` | Nodes of `FromBranch` covered by the summary, or removed without one |
| `SummaryNode` | The node on the new branch that holds the summary |

The record is metadata: it is persisted with the tree and stripped before every provider call. Messages kept verbatim are copied with their metadata, and the approval policy's state is written onto the new branch as one snapshot record ([approval policy](approval-policy.md)), so grants and denial counts survive compaction.

```go
for d := range stream.Deltas() {
	if c, ok := d.(types.CompactionDelta); ok {
		log.Printf("%s: %d -> %d tokens, %d summarized, now on %s",
			c.Record.Strategy, c.Record.TokensBefore, c.Record.TokensAfter, len(c.Record.Summarized), c.Branch)
	}
}
```

## Policy per agent

Compaction is configured per agent with `CompactCfg` (or `agent.WithCompactConfig`), and turned off with `agent.WithoutCompaction()`, which is `Strategy: types.CompactNone`. With compaction off, no strategy runs before a turn, `CompactNow` does nothing, and a context-length error is returned instead of compacted.

A subagent inherits its parent's `CompactCfg` and `CompactProvider` unless its `SubAgentDef.Options` set its own. Each subagent compacts its own tree, so an orchestrator can keep every message while its workers compact:

```go
orchestrator := agent.NewAgent(agent.AgentConfig{
	Provider: model,
	SubAgents: []agent.SubAgentDef{
		{
			Name: "researcher", Description: "Reads many documents",
			Options: []agent.AgentOption{agent.WithCompactConfig(&types.CompactConfig{
				Strategy: types.CompactKeepRecent, KeepTurns: 6, MaxInputTokens: 60_000,
			})},
		},
		{Name: "writer", Description: "Drafts the report"}, // inherits: off
	},
}, agent.WithoutCompaction())
```

A child's compaction deltas reach the parent's stream inside `ToolExecDelta`, like its other deltas.

**Handoff groups.** Members of a handoff group share one tree, and a shared summary would erase the boundaries between owners and could expose one owner's context to another (D-11 in [design decisions](../DESIGN_DECISIONS.md)). An agent with handoffs therefore rejects every active strategy, including an empty one, before its first provider call with `handoff context compaction requires per-owner checkpoints; automatic compaction is unsupported`. A disabled policy (`none`, or `WithoutCompaction()`) is accepted, so an orchestrator that turns compaction off can also own a handoff group. A context-length error in a handoff group is returned at once. The same rule holds under a durable approval runner: only a disabled policy is accepted.

## Presets

A catalog preset can name the compaction strategy for the agents built from it ([catalog](catalog.md#presets)). `agent.WithPreset` applies it unless the agent already has a `CompactCfg`.

```json
"research": {
  "compaction": {
    "strategy": "chain", "max_input_tokens": 150000, "target_tokens": 100000,
    "chain": [
      { "strategy": "clear_tool_results", "keep_tool_results": 3 },
      { "strategy": "relevant_plus_summary", "keep_turns": 4, "select_k": 3, "summary_model": "claude-haiku-5-5" }
    ]
  },
  "chain": [ { "provider": "anthropic", "model": "claude-sonnet-5-5" } ]
}
```

Keys: `strategy`, `max_input_tokens`, `target_tokens`, `keep_turns`, `select_k`, `threshold`, `keep_last`, `window_size`, `keep_tool_results`, `exclude_tools`, `summary_model` and `chain`. An unknown strategy, a `chain` without steps, steps on another strategy, and negative counts are rejected at load time with their path.
