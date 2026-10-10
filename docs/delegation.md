# Handoffs and subagents

An agent can pass work to another agent in two ways. A **handoff** moves ownership of the conversation to another member of a group: the recipient continues the same branch of the entry agent's tree, and control comes back only when some member transfers it back. A **subagent** is a fresh agent built for one task: it runs on its own tree, and its answer returns to the caller as a tool result. This page compares the two paths on what goes in, what comes out, where the transcript lives, and which limits apply. For the ownership model and the policy interfaces, see [orchestration policies](orchestration-policies.md).

- [Two paths, side by side](#two-paths-side-by-side)
- [Context in](#context-in)
- [Data out](#data-out)
- [Collecting transcripts after a run](#collecting-transcripts-after-a-run)
- [Size limits and truncation](#size-limits-and-truncation)
- [Iteration and time limits](#iteration-and-time-limits)
- [Subagent budgets and wrap-up](#subagent-budgets-and-wrap-up)
- [Private scratch](#private-scratch)
- [References instead of copies](#references-instead-of-copies)
- [Choosing a path](#choosing-a-path)

## Two paths, side by side

| | Handoff | Subagent (delegate) | Subagent (spawn) |
| --- | --- | --- | --- |
| Configure with | `WithHandoffs(HandoffDef...)` | `SubAgentDef{Mode: SubAgentDelegate}` (the zero value) | `SubAgentDef{Mode: SubAgentSpawn}` |
| Tool the model sees | `handoff_to_<name>(reason, message, context)` | `delegate_to_<name>(task)` | `spawn_<name>(task)`, plus `await_subagent`, `send_subagent`, `cancel_subagent`, `list_subagents`, `search_subagent`, `read_subagent` |
| Who owns the conversation | The recipient, on the entry agent's branch | The caller. The child owns only its own tree | The caller. The child owns only its own tree |
| Where control returns | Nowhere automatically. It returns only when a member calls `handoff_to_<previous-owner>` | To the caller, as the tool result of the `delegate_to_<name>` call | The spawn call returns a handle at once. The result arrives later as a message or through `await_subagent` |
| Runs | In the entry agent's loop, one owner at a time | A new `Agent` and tree per call | A new `Agent` and tree per call, in the background |
| Source | `agent/handoff.go`, `agent/handoff_context.go`, `agent/links.go` | `agent/subagent.go`, `agent/subagent_result.go` | `agent/spawn.go`, `agent/subagent_tools.go` |

A handoff tool is a signal, not a normal tool. Its `Execute` returns `Transferring control to <name>.` as the tool result, and the loop then appends a `HandoffPart` node to the branch (`applyHandoff` in `agent/agent.go`). A member that finishes with a text answer ends the run while it still owns the branch. The next `Invoke` on that branch resumes with the same owner, because the active member is resolved from the last `HandoffPart` on the branch.

Keep these rules in mind:

- Name the entry agent. `agent.New` returns an error wrapping `types.ErrInvalidConfig` for an invalid group: an empty or duplicate name, an unknown target, or no provider for a member when the entry agent has none.
- Request one transfer per turn. A turn with two or more handoff calls fails every call in that turn with `ambiguous handoff: request one control transfer per turn`.
- Expect return edges by default. `DirectReturnLinks` adds the reverse of each declared edge. Use `WithLinkPolicy(agent.DirectedLinks{})` to keep only the declared graph.
- Give each member its own tools. A member sees its `HandoffDef.Tools` plus its `handoff_to_*` tools. The entry agent's `delegate_to_*` tools are not added to members.
- Expect delegation to refuse cycles. A delegation or spawn whose target is the caller or one of its ancestors is refused with `ErrAncestorDelegation` (`agent/subagent_context.go`).

```go
triage, err := agent.New(agent.Config{
	Name:         "triage",
	SystemPrompt: "Route each request to the right specialist.",
	Provider:     model,
},
	agent.WithHandoffs(
		agent.HandoffDef{
			Name:         "billing",
			Description:  "Invoices, refunds, and payment methods.",
			SystemPrompt: "You handle billing questions.",
			Tools:        billingTools,
			MaxIter:      6,
		},
		agent.HandoffDef{
			Name:         "support",
			Description:  "Product questions.",
			SystemPrompt: "You answer product questions.",
			CanHandOffTo: []string{"billing"},
		},
	),
	agent.WithMaxHandoffs(4),
)
if err != nil {
	return err
}
```

## Context in

### Handoff

A member's `SystemPrompt` is added as a second text block on the root system message. The root itself is never changed, and every member sees it. Do not put owner-private data in the root.

Each turn, the `HandoffContextPolicy` selects what the active owner sees (`agent/handoff_context.go`):

| Policy | The owner sees |
| --- | --- |
| `OwnerContext` (default) | The root system message, its own earlier turns, and a transfer brief for each transfer to it. A new owner also receives the latest user task. Other owners' reasoning and tool traffic stay in the tree but are not sent |
| `FullHandoffContext` | Every message on the branch |
| Custom `HandoffContextPolicy` | What `Select(ctx, HandoffContext{Entry, Target, Messages})` returns. Keep tool calls paired with their results |

The transfer brief is a user message built from the handoff arguments:

```text
Handoff from <from> to <to>. Task brief or return data: <reason>
Handover note: <message>
Context: <context>
```

The `Handover note` and `Context` lines appear only when the caller filled `message` and `context`. All three arguments are stored on the `HandoffPart` node.

### Subagent

`SubAgentDef.Context` selects which parent messages the child starts with (`agent/subagent_context.go`):

| `SubAgentContext` | `String()` | The child starts with |
| --- | --- | --- |
| `ContextTaskOnly` (default) | `task_only` | The task alone |
| `ContextFork` | `fork` | The parent's branch up to the delegating turn, then the task |
| `ContextFiltered` | `filtered` | What `ContextFilter` (a `MessageSelector`) selects from that same history, then the task |

The copied history leaves out the root system message, metadata parts, thinking parts, and the delegating turn itself, whose tool calls have no results yet. A filter that breaks tool pairing fails the call before the child starts. `TextMessagesOnly` is a ready filter that keeps only user and assistant text. `InvokeSubAgent` always starts the child with the task alone, whatever the mode.

`ContextFork` copies the raw branch, not an owner view. When the entry agent of a handoff group forks a child, the child receives every owner's messages on that branch.

The child's first user message starts with a `<caller>` block, then the task:

```text
<caller>
You are a sub-agent. Call path: lead > researcher (depth 1).
Your final message is the entire result your caller receives. It does not see your other messages or tool results, so put everything it needs in that message.
Do not delegate back to lead.
</caller>
```

Set `OmitCallerBlock` to leave it out. The ancestor refusal applies either way.

### What each path inherits

A handoff member runs inside the entry agent's loop, so it uses the entry agent's config for everything except the fields `HandoffDef` sets: `SystemPrompt`, `Provider`, `Tools`, `MaxIter`, and `Dials`. A nil `Provider` or `Dials` and a zero `MaxIter` inherit the entry agent's.

A subagent gets its config from `inheritConfig` in `agent/subagent.go`. `SubAgentDef.Options` are applied after inheritance and override any inherited value.

| Setting | Subagent | Handoff member |
| --- | --- | --- |
| `Provider` | `SubAgentDef.Provider`, else the parent's. A `SessionProvider` gets a new session | `HandoffDef.Provider`, else the entry agent's. A `SessionProvider` gets a new session |
| `MaxIter` | `SubAgentDef.MaxIter`, else the parent's cap, else `DefaultSubAgentMaxIter` when the parent has none. See [subagent budgets](#subagent-budgets-and-wrap-up) | `HandoffDef.MaxIter`, else the entry agent's |
| `Dials`, `DialPolicy` | Inherited (dials cloned) | `HandoffDef.Dials`, else the entry agent's |
| `ToolGate`, `ToolPolicy`, `ApprovalPolicy` | Inherited | The entry agent's |
| `Deps`, `ToolContext`, `ToolRedactor`, `Tokenizer` | Inherited | The entry agent's |
| `LLMTimeout`, `ToolTimeout`, `MaxParallelTools` | Inherited | The entry agent's |
| `OnMaxIter` | `MaxIterForceFinal`, whatever the parent's. Override it in `Options` | The entry agent's |
| `ForceFinalPrompt`, `MaxConsecutiveErrors`, `MaxRepeatIterations` | Inherited | The entry agent's |
| `InterruptTTL`, `InterruptPolicy` | Inherited | The entry agent's |
| `CompactCfg`, `CompactProvider` | Inherited, unless `Options` set one | Only a disabled policy: a handoff group rejects every active strategy (see below) |
| `Budget` | Shared by pointer, not copied. Set `WithBudget` in `Options` to cap one child separately | The entry agent's |
| `Workspace` | A private scratch over a read-only view of the parent's. See [private scratch](#private-scratch) | The entry agent's |
| `StepRunner` | A durable parent runner is shared with step names prefixed `sub-<toolCallID>-`. An inline parent leaves the child inline | The entry agent's |
| `Logger`, `Metrics`, `Resolvers`, `Extractors` | Inherited | The entry agent's |
| `ResponseSchema` | `SubAgentDef.ResponseSchema` only. The parent's is not inherited | The entry agent's |
| `Tree`, `Store` | Not inherited. Each call builds a new in-memory tree | The entry agent's tree |
| `OutcomePolicy`, `Handoffs`, `MaxHandoffs`, `ServerTools`, `StopAtTools`, `ToolChoice` | Not inherited | The entry agent's |

Fields not listed in `inheritConfig`, such as `AutoContinue`, `RunTracer`, `HandoffContextPolicy`, and `LinkPolicy`, start at their zero values in a child. Set them through `Options` when a child needs them. A child's approvals reach the parent's consumer with tool call IDs of the form `<parentCallID>/<childCallID>`; see [approval policy](approval-policy.md).

## Data out

### Subagent results

Every child run ends in a `SubAgentResult` (`agent/subagent_result.go`):

| Field | Holds |
| --- | --- |
| `ID` | The invocation ID: the delegating tool call ID, or a new ID for `InvokeSubAgent` |
| `Name`, `Task` | The definition name and the task text |
| `Branch` | The child branch the result was read from |
| `StartedAt`, `CompletedAt` | UTC timestamps |
| `Trace` | The child's whole tree as `tree.Print` JSON, including on failure |
| `Output` | What the result policy selected. Empty on failure |
| `Error` | The failure text, if any |
| `StopToolCallID` | Set when a `StopAtTools` tool ended the child |
| `Iterations`, `MaxIter` | Model turns used, the forced final call included, and the child's cap |
| `Forced`, `ForcedReason` | Set when the answer was forced at a step limit, and which limit |
| `OutputRef` | The `saige-artifact://` URI of `Output` when it went to the parent by reference |
| `Scratch` | A read-only view of the child's private scratch. Not serialized |

`ParentText()` returns exactly what the parent's model received: `Output`, or a reference with a preview, behind a note when the answer was forced.

Read it with `Tree()`, `Messages()`, `Node(id)`, `FinalAssistant()`, and `StopToolResult()`. Each call returns independent data.

The result policy decides what the parent's model receives:

| Policy | Output |
| --- | --- |
| `FinalAssistantText` (default) | The text of the terminal assistant turn. Intermediate text and nested delegation output are excluded. A child stopped by a `StopAtTools` tool returns that tool's result text |
| `SchemaResult` (default when `ResponseSchema` is set) | The answer as compact JSON after it validates against the schema. No JSON, or JSON that does not match, fails the delegation |
| Custom `SubAgentResultPolicy` or `SubAgentResultFunc` | What `Select(SubAgentResult)` returns. An error fails the delegation |

A `SubAgentResultSink` saves every result, failures included, before the parent sees success. A save error fails the delegation. Without a sink, a model-driven delegation keeps only `Output` in the parent tree.

Set `SubAgentDef.ResponseSchema` for a typed result. The child uses native structured output when its provider supports it, and the `final_answer` tool otherwise. Decode with `DecodeOutput[T]`:

```go
type Summary struct {
	Title  string   `json:"title"`
	Points []string `json:"points"`
}

lead, err := agent.New(agent.Config{
	Name:     "lead",
	Provider: model,
	SubAgents: []agent.SubAgentDef{{
		Name:         "summarizer",
		Description:  "Summarize one document.",
		SystemPrompt: "Summarize the document you are given.",
		MaxIter:      4,
		Timeout:      2 * time.Minute,
		ResponseSchema: &types.ParameterSchema{
			Type:     types.SchemaObject,
			Required: []string{"title", "points"},
			Properties: map[string]types.PropertyDef{
				"title":  {Type: types.SchemaString},
				"points": {Type: types.SchemaArray, Items: &types.PropertyDef{Type: types.SchemaString}},
			},
		},
	}},
})
if err != nil {
	return err
}

stream, err := lead.InvokeSubAgent(ctx, "summarizer", doc)
if err != nil {
	return Summary{}, err
}
for range stream.Deltas() {
}
result, err := stream.SubAgentResult()
if err != nil {
	return Summary{}, err // result.Trace still holds the partial transcript
}
return agent.DecodeOutput[Summary](result)
```

A failed delegation becomes an error tool result in the parent, and the parent's model decides what to do next. Under a step runner that implements `types.ApprovalRunner`, a child run error also stops the parent run.

### Spawned subagents

`spawn_<name>` returns a receipt at once: `{"handle", "name", "status": "running", "note"}`. The handle is the spawn call's tool call ID. The child runs under the run's context, so it outlives the tool call.

A finished result reaches the parent in one of two ways:

- **`await_subagent(handle)`**: the tool result is the child's `ParentText()`, or an error. The result is then not delivered again.
- **Injection at a safe point**: at the start of each loop iteration, after every tool call has its result, each finished and undelivered child is appended as one user message, in completion order, and reported with `InjectedDelta{Mode: "subagent"}`:

```text
<subagent_result handle="<id>" name="<name>" status="completed" iterations="<n>">
<ParentText(), or "error: ..." for a failed child>
</subagent_result>
```

A forced child's tag also carries `forced="true"`.

A run does not finish while a child runs or a result is undelivered. Where it would finish, it waits for the next result and resumes with it. `cancel_subagent` stops a running child, and its result is not delivered. A run that ends by error, cancellation, or a stop tool cancels its children before the stream closes. Spawning needs the inline runner; under a durable runner `spawn_<name>` returns `ErrSpawnUnsupported` as its tool error.

`search_subagent(handle, query, k)` ranks a finished child's messages with BM25. `read_subagent(handle, index, window)` returns a window of its transcript. Both also accept the call ID of a finished `delegate_to_<name>` call. All six handle tools are registered only when at least one definition uses `SubAgentSpawn`.

### Handoffs

A handoff has no result to return. Every owner's turns, tool calls, and results stay on one branch of the entry agent's tree. A later owner reads earlier work through its `HandoffContextPolicy` and the transfer briefs. Pass return data in `reason` when handing back, as the tool's parameter description asks. The host reads the whole branch directly; see the next section.

## Collecting transcripts after a run

| Path | Where the transcript lives | How the host reads it |
| --- | --- | --- |
| Handoff | One branch of the entry agent's tree, persisted to its `Store` when one is set | `a.Tree().FlattenBranch(branch)`, or `tree.Print` for the JSON trace |
| Delegate | The child's in-memory tree, built per call and never written to a `Store` | A `ResultSink`, or `EventStream.SubAgents()` when the agent has a spawn definition |
| Spawn | Same as delegate | `EventStream.SubAgents()`, then `SubAgentHandle.Wait(ctx)` |
| `InvokeSubAgent` | Same as delegate | `stream.SubAgentResult()` on the returned stream |

On a handoff branch, each transfer is a system-message node holding `HandoffPart{From, To, Reason, Message, Context}`. A `HandoffPart` placed in a user message forces a transfer from the host side. Both are metadata: the loop strips them before every provider call. The stream reports each transfer as `HandoffDelta{From, To, Reason}`, and `From` names the entry agent for a transfer out of it.

Provenance comes from these records:

- **Owner of a turn**: the last `HandoffPart` before it on the branch. Tools read it as `RunScope.Agent` (`agent.RunScopeFromContext`) and `types.ToolCallInfo.Agent`.
- **Model that produced a turn**: a `RoutePart` on the assistant turn, recorded when the provider reports a route or dials were compiled for the call.
- **Child events**: forwarded live as `ToolExecDelta{ToolCallID, Inner}` under the delegating or spawning call ID.
- **Delegation path**: interrupts carry the run ID and the path of tool call IDs from the root run.

For tracing spans across parents and children, see [observability](observability.md).

## Size limits and truncation

| Mechanism | Default | Behavior |
| --- | --- | --- |
| `workspace.Spill` around a tool | `DefaultSpillMaxBytes` 16 KiB, `DefaultSpillPreviewBytes` 2 KiB | A larger result is stored in the workspace and replaced by a preview cut at a word boundary, with a `saige-artifact://<digest>` URI and a hint to page with `scratch_read` |
| Subagent output | `SubAgentReferences.ResultTokens`, 2000 estimated tokens | A larger result reaches the parent's model as a `saige-artifact://` URI and a preview. `Output` keeps all of it. See [references](#references-instead-of-copies) |
| Subagent task and forked text | `SubAgentReferences.InputTokens`, 2000 estimated tokens | A larger task or text block goes to the child's scratch, and the child gets a URI and a preview |
| `search_subagent` | 5 hits, at most 20; excerpts of 300 characters | Longer text ends in `...` |
| `read_subagent` | 5 messages, at most 20; 4000 characters per message | Longer text ends in `...` |

Do not wrap handoff, clarification, or sub-agent tools with `Spill`; the loop recognizes them by type. A spilled tool inside a child spills into the child's private scratch, even when it was built for the parent's workspace, and the parent reads the spill through `SubAgentResult.Scratch`. A child can still read artifacts the parent stored, so pass a `saige-artifact://` URI in the task instead of the content.

Compaction (`agent/overflow.go`, `agent/types/compactor.go`, `agent/types/compact_strategy.go`; full guide: [context management](context-management.md)):

| Trigger | When it fires |
| --- | --- |
| No `MaxInputTokens` | The strategy's own rule: `keep_recent` past `KeepTurns` turns, `summary` and `relevant_plus_summary` past `Threshold` messages, `summarize` past `Threshold` keeping `KeepLast` (default 4), `sliding_window` past `WindowSize`, `clear_tool_results` past `KeepToolResults` results (default 3) |
| `MaxInputTokens` set | Before a turn whose input exceeds it. Input is the larger of the last reported prompt tokens and the tokenizer's estimate. `summarize` and an empty strategy summarize the older half of the branch; a `chain` stops once the history fits `TargetTokens` (default `MaxInputTokens`). A turn is compacted again while it is still over and the last compaction shrank it, up to 5 times |
| Context-length error | The branch is compacted and the turn retried, up to 3 times. It needs a `CompactCfg` whose strategy is not `none`. Otherwise, or after 3 attempts, the run returns the first context-length error |

Compaction writes a new branch, makes it active, records a `CompactionPart` on it and streams a `CompactionDelta`. It does not count as an iteration.

**Handoff groups reject active compaction** (D-11 in `DESIGN_DECISIONS.md`). A shared summary would erase ownership boundaries. A handoff group with an active strategy, including an empty one, fails the run before its first provider call with `handoff context compaction requires per-owner checkpoints; automatic compaction is unsupported`. A disabled policy (`Strategy: types.CompactNone` or `agent.WithoutCompaction()`) is accepted. A context-length error in a handoff group is returned at once. A subagent inherits the parent's `CompactCfg` unless its `Options` set one, and compacts its own tree, so an orchestrator with compaction off can delegate to children that compact; under a durable approval runner only a disabled policy is accepted. See [durable execution](durable-execution.md).

Output truncation (`agent/forcing.go`, `agent/submit.go`):

- **`max_tokens` cut**: the completed text is committed with a `TruncationPart` marker and reported with `TruncatedDelta{NodeID, Reason}`. Its tool calls never run. A cut turn with tool calls fails the run with `*types.ResponseTruncatedError`. A text-only cut ends the run cleanly.
- **`WithAutoContinue(n)`**: resume a text-only cut turn up to n times. A provider that declares `CapAssistantPrefill` continues the partial turn; any other receives `DefaultContinuePrompt`.
- **`Agent.Continue(ctx, branch)`**: resume a branch that ends with a text-only assistant turn later.
- **Interrupt**: `SubmitInterruptReplace` stops the provider call, commits its completed text with reason `interrupted`, drops its tool calls, and sends `InterruptedDelta` and `TruncatedDelta`.
- **Child failure or cancellation**: the child has no `Output`, and its `Trace` keeps the messages committed before the failure.

## Iteration and time limits

| Limit | Default | Scope | At the limit |
| --- | --- | --- | --- |
| `agent.Config.MaxIter` | 10 | Completed model turns per user turn of a run. Compactions and retries do not count. One counter covers every handoff owner in the run | `types.ErrMaxIterations` when tool results are still unanswered; otherwise a clean finish |
| `HandoffDef.MaxIter` | 0, which uses the entry agent's | The cap while that member owns the turn, checked against the shared counter. A `ConfigPart.MaxIter` on the branch overrides it | Same as `MaxIter` |
| `agent.Config.MaxIter` set to `NoIterLimit` | | No cap. Suits an orchestrator whose children stay bounded | None |
| `SubAgentDef.MaxIter` | 0, which uses the parent's cap, or `DefaultSubAgentMaxIter` (10) under an uncapped parent | The child's own counter, independent of the parent's | A forced answer without tools. The delegation succeeds and the result is marked `Forced` |
| `SubAgentDef.WrapUpAt` | `MaxIter - 2`, at least 1 | The child's turns before its wrap-up note | The note says how many turns remain and to return the result now |
| `MaxHandoffs` | 8 | Transfers per run, not reset by queued user turns | `ErrHandoffLimitExceeded`. The over-limit transfer is neither streamed nor persisted |
| `MaxConsecutiveErrors` | 2 (`DefaultMaxConsecutiveErrors`); negative disables | Consecutive turns in which every tool call failed. Refused calls do not count | `ErrToolErrorLimit` |
| `MaxRepeatIterations` | 0 (off) | Identical tool calls more than n turns in a row | The repeated calls are not run, then `ErrRepeatedToolCalls` |
| `SubAgentDef.Timeout` | 0 (none) | The whole child run, delegate or spawn. The clock pauses while a child approval waits | `sub-agent <name> exceeded timeout <d>: context deadline exceeded` as the call's error |
| `ToolTimeout` | 0 (none) | Each tool step. Approval waits do not count. A delegation or spawn as a whole is not bounded; the child applies it to each of its own tool calls | A deadline error as the call's error. A tool that ignores its context gets `tool <name> exceeded timeout <d>` |
| `LLMTimeout` | 0 (none) | Each provider call | The call's error ends the run |

`OnMaxIter` decides what happens at `MaxIter`, `MaxConsecutiveErrors`, and `MaxRepeatIterations`:

| `MaxIterPolicy` | Result |
| --- | --- |
| `MaxIterError` (default) | The run returns the limit's error |
| `MaxIterForceFinal` | One more call with tools disabled and `DefaultForceFinalPrompt` (or `ForceFinalPrompt`). Its reply is recorded as the final answer and the run ends without an error. A failed call or an empty reply returns the limit's error |

`MaxHandoffs` is not covered by `OnMaxIter`: it always returns `ErrHandoffLimitExceeded`. A child runs under `MaxIterForceFinal` whatever the parent uses, so a child at its cap returns a forced answer as its result instead of failing.

`await_subagent` is an ordinary tool step, so `ToolTimeout` bounds it. When the wait times out, the tool returns `sub-agent <handle> is still running`, and the result is still delivered at a later safe point.

## Subagent budgets and wrap-up

Each child has an iteration budget, counted in model turns per user turn.

| Setting | Default | Effect |
| --- | --- | --- |
| `SubAgentDef.MaxIter` | The parent's cap, or `DefaultSubAgentMaxIter` (10) when the parent has none | The child's cap. `NoIterLimit` removes it. |
| `SubAgentDef.WrapUpAt` | `MaxIter - 2`, at least 1 | After this many turns the child gets a wrap-up note. A negative value sends none. |
| `SubAgentDef.WrapUpPrompt` | `DefaultWrapUpPrompt` | Replaces the instruction in the note. |
| Limit policy | `MaxIterForceFinal` | At the cap the child answers with its tools removed. |

The orchestrator follows its own config. Set `MaxIter: agent.NoIterLimit` on the parent to remove its cap. Its children stay bounded:

```go
lead, err := agent.New(agent.Config{
    Name:     "lead",
    Provider: model,
    MaxIter:  agent.NoIterLimit,
    SubAgents: []agent.SubAgentDef{{
        Name:        "researcher",
        Description: "Finds facts in the corpus.",
        Tools:       researchTools,
        MaxIter:     6, // wrap-up note after turn 4, forced answer at turn 6
    }},
})
if err != nil {
    return err
}
```

The wrap-up note is a system message. It joins the child's branch at the next safe point after the threshold, once per user turn, and only while the child still has tool results to answer. It states how many iterations remain and tells the child to return its result now. When the child answers through `final_answer`, the note tells it to call `final_answer`. The child's stream reports the note as an `InjectedDelta` with `Mode` `"wrap_up"`.

At the cap the child makes one more call with no tools and records that reply as its answer. The delegation succeeds. To fail the delegation at the cap instead, add `agent.WithOnMaxIter(agent.MaxIterError)` to `SubAgentDef.Options`.

Every result carries its budget metadata:

| Field | Meaning |
| --- | --- |
| `Iterations` | Model turns used, the forced final call included |
| `MaxIter` | The child's cap, or `NoIterLimit` |
| `Forced` | The answer was forced at a step limit |
| `ForcedReason` | Which limit: iterations, consecutive tool errors, or repeated calls |

The parent model sees the same facts. A forced delegation result starts with a note that the child reached its step limit after N iterations. A spawned child's `<subagent_result>` message carries `iterations="N"`, plus `forced="true"` when forced.

`agent.Config.WrapUpAt` and `WithWrapUpAt` give the same note to any agent, a top-level one included.

## Private scratch

Each invocation of a child gets a fresh in-memory workspace. The child's workspace is that scratch layered over a read-only view of the parent's (`workspace.Layers`):

- Writes go to the child's scratch only. The parent's workspace never changes.
- Reads look in the scratch first and then in the parent's workspace.
- Siblings, and later invocations of the same definition, never see each other's scratch.
- A spilling tool (`workspace.Spill`) built for the parent spills into the child's scratch when the child runs it.

A child that has tools also gets `scratch_write`, `scratch_read`, and `scratch_search`. A child with no tools answers in one turn and does not get them.

| `SubAgentScratch` field | Effect |
| --- | --- |
| `Off` | No scratch and no scratch tools. The child sees the parent's workspace read-only. Large inputs stay inline. |
| `NoTools` | Keep the scratch for references and spills, but add no scratch tools. |
| `New` | Build the scratch yourself, for example a `workspace.Dir` per call ID. |

The parent reads a child's scratch in three ways:

- `SubAgentResult.Scratch`, a read-only view, from `SubAgentResult()`, a `ResultSink`, or `SubAgentHandle.Wait`.
- `SubAgentHandle.Scratch()`, available as soon as a spawned child starts.
- The parent model's `read_artifact` and `search_artifact` tools, which cover every child scratch the run received a result from.

### Lifetime and cleanup

An in-memory scratch lives as long as something holds it: the parent run, until the run ends, and any `SubAgentResult` or handle the host keeps. It needs no cleanup. A scratch made by `New` belongs to the host. Remove it, for example the `workspace.Dir` directory, when you no longer need the child's results.

A result reference stored in a child's scratch stops resolving for the parent model after the parent's run ends. Give the parent a `Workspace` to keep result references readable across runs.

## References instead of copies

Sizes are estimated at four bytes per token (`workspace.EstimateTokens`), so a reference decision needs no tokenizer and gives the same result on every run and replay.

| `SubAgentReferences` field | Default | Effect |
| --- | --- | --- |
| `InputTokens` | 2000 | A task, or a text block of a forked or filtered message, above this goes to the child's scratch. |
| `ResultTokens` | 2000 | A result above this goes back to the parent as a reference. This is the cap on inline sub-agent output. |
| `PreviewTokens` | 200 | The size of each preview. |
| `Off` | false | Send everything inline. |

### Inputs

A large task is stored in the child's scratch as `inputs/task`. The child receives the artifact's `saige-artifact://` URI and a preview, never the full text. It also gets `read_artifact` and `search_artifact` to pull the parts it needs. A child also gets these tools when its task names an artifact URI, for example one a host created with `workspace.NewReference`. Large text blocks and text-only tool results in forked or filtered history are replaced the same way. Tool call IDs are kept, so tool pairing is unchanged. With scratch off, inputs stay inline.

### Results

A large result is stored in the parent's workspace when it accepts writes, under `subagents/<name>/<call id>/result`. Otherwise it is stored in the child's scratch. The parent model receives the URI and a preview, and reads more with `read_artifact`. An agent with sub-agents has that tool. `SubAgentResult.Output` still holds the whole result for the host, and `OutputRef` names the artifact. `ParentText()` returns exactly what the parent model received.

### Passing data by reference yourself

A tool returns a reference instead of a large value with `agent.Ref`. It stores the data in the call's workspace:

```go
func exportRows(ctx context.Context, args map[string]any) (string, error) {
    rows, err := loadRows(ctx)
    if err != nil {
        return "", err
    }
    return agent.Ref(ctx, "exports/rows.csv", rows) // URI and preview, not the rows
}
```

A host outside a tool call uses `workspace.NewReference` with its workspace, then puts the reference in a task:

```go
ref, err := workspace.NewReference(ctx, ws, "docs/contract.md", contract, workspace.ReferenceOptions{})
if err != nil {
    return err
}
stream, err := lead.InvokeSubAgent(ctx, "reviewer", "Review this contract.\n"+ref.String())
```

Copy helpers move content without passing it through a model:

| Helper | What it does |
| --- | --- |
| `workspace.Copy` | Copy an artifact to a new name, or into another workspace |
| `workspace.CopyArtifactTool` | `copy_artifact`: the model files an artifact under a new name |
| `workspace.CopyFileTool(ws, root)` | `copy_file`: store a file under `root` and return a reference. Paths resolve inside `root` through `os.Root`, so `..` and symbolic links cannot escape it. |

Content is addressed by its digest, so a copy shares its source's URI and the backend stores no new bytes.

## Choosing a path

| Need | Use |
| --- | --- |
| A specialist should take over the conversation and talk to the user | Handoff |
| A specialist should answer one bounded question and the caller continues | Delegate (`delegate_to_<name>`, `ContextTaskOnly`) |
| Several independent tasks should run while the caller keeps working | Spawn (`spawn_<name>`) |
| The child needs the conversation so far | Fork (`Context: ContextFork` on a delegate or spawn) |
| The child needs what was said, without tool traffic | `ContextFiltered` with `TextMessagesOnly` |
| A typed result for code, not prose for a model | Delegate with `ResponseSchema`, or `InvokeSubAgent` and `DecodeOutput` |
| Long sessions that must compact | Subagents. Handoff groups reject compaction |
| A durable step runner | Handoff or delegate. Spawn needs the inline runner |
| A hard boundary between agents' context | Delegate with `ContextTaskOnly`. Handoff members share the root system message and one tree |

For how tool calls run inside either path, see [tool calling](tool-calling.md).
