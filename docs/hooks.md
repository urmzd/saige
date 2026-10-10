# Run hooks

`agent.Hooks` is one typed seam for host code that needs to see a run as it happens: auditing, metrics, policy checks, annotations, and work after a run such as memory extraction.
Every field is optional. A hook observes its event, and at the points where it is safe it may change the event or abort the run.

```go
a, err := agent.New(cfg, agent.WithHooks(agent.Hooks{
    Name: "audit",
    BeforeModelCall: func(ctx context.Context, ev *agent.BeforeModelCallEvent) error {
        log.Printf("%s calls %s with %d messages", ev.Agent, ev.Model, len(ev.Messages))
        return nil
    },
    BeforeTool: func(ctx context.Context, ev *agent.BeforeToolEvent) error {
        if ev.Call.Name == "deploy" && !changeWindowOpen() {
            return agent.Abort("deploys are frozen")
        }
        return nil
    },
    RunStop: func(ctx context.Context, ev *agent.RunStopEvent) error {
        log.Printf("run %s ended: %s", ev.RunID, ev.Reason)
        return nil
    },
}))
if err != nil {
    return err
}
```

## Events

Each event embeds `agent.HookRun`: the agent that owns the turn (the active handoff member, or the agent itself), the root run ID, the call path from the root run, and the branch.

| Event | When | Can change | Can abort |
|---|---|---|---|
| `RunStart` | Once, before the input is appended | | Yes |
| `UserInput` | Each user message: the run's input and every submitted message | `Message` | Yes |
| `BeforeCompaction` | Each time the run tries to compact | `Skip` | Yes |
| `AfterCompaction` | After that attempt, with `Compacted` and `NewBranch` | | |
| `BeforeModelCall` | Before each model call, with the messages, tools and request options | | Yes |
| `AfterModelCall` | After each model call, also a failed or replayed one, with the turn, usage and dial report | | |
| `BeforeTool` | After a call's gate and approvals, just before it runs | `Arguments` | Yes |
| `AfterTool` | After a tool ran, before its result is streamed and recorded | `Result`, `Error` | Yes |
| `SubagentStart` | Before a delegation or spawn starts its child | `Task` | Yes |
| `SubagentEnd` | When the child finished, with its output and error | | |
| `InterruptRaised` | When the run posts a decision it waits for | | |
| `InterruptResolved` | When the decision arrives, lapses or is cancelled | | |
| `TurnEnd` | After a model turn and its tool results are recorded | | Yes |
| `RunStop` | Once, after the branch is released and before the stream reports completion, with the reason | | |

With a message-count compactor the run tries to compact before every turn and the compactor decides whether anything changes, so `BeforeCompaction` fires each turn. `AfterCompaction` reports whether the history changed.

`RunStop.Reason` is one of `completed`, `stop_tool`, `canceled`, `suspended`, `limit`, `budget`, `aborted`, `guardrail` or `error`. `RunStop.Messages` holds the branch as the run left it.

## Contract

**Order.** Hook sets run in the order they were added, a sub-agent's inherited sets first. Within one point each hook sees the changes of the hooks before it, and the first abort stops the chain.

**Changes.** Only the fields in the "Can change" column are read back. Changed arguments are checked against the tool's schema again but not gated again; a gate decides whether a call runs, a hook only adjusts a call that was already allowed. Replace a map rather than mutate the values inside it.

**Abort.** At an abortable point any error stops the run with a `*agent.HookAbortError` that matches `agent.ErrHookAborted` and names the event, the hook set, and the reason. `agent.Abort(reason)` gives the reason without an error of your own. An aborted tool call still gets an error result, so every `tool_use` keeps its `tool_result`. At an observing point an error is logged and the run goes on.

**Timeouts and panics.** Each call gets a context bounded by `WithHookTimeout` (30 seconds by default, `agent.DefaultHookTimeout`; a negative value removes the bound). A hook that returns after its deadline fails with `agent.ErrHookTimeout`, and a panic is recovered as an error; either aborts at an abortable point and is logged at an observing one. The agent waits for a hook to return instead of abandoning it, because a hook that kept running would race with the run, so a hook must honor its context. `RunStop` hooks run on a context the run's cancellation does not reach.

**Concurrency.** Tool, sub-agent and interrupt hooks of one turn can run at the same time on the goroutines that run the calls. A hook set must be safe for concurrent use.

**Safe points.** Hooks never add or remove messages themselves. `UserInput` for a submitted message runs where the message is appended, which is always a safe point.

## Durable replay

Under a durable `StepRunner`, the outcome of every point that can change or abort the run is recorded as a step of kind `types.StepKindHook`: the changed message, arguments, result, task or skip flag, or the abort and its reason.
A replay applies the recorded outcome and does not call those hooks, so a hook that would now decide otherwise, or that is not deterministic, cannot change a recovered run.
Observing hooks are called again on replay. `AfterModelCallEvent.Replayed` marks a turn the runner returned from its record.

Step names derive from the point: `hook-before_tool-<call ID>` for tool points, `hook-before_model_call-<model step>` for model calls, and a counter, such as `hook-user_input-0`, for points the loop meets in order.
A point with no hooks records nothing.

## Sub-agents

A sub-agent inherits its parent's hook sets and hook timeout, the same way it inherits the tool gate. `SubAgentDef.Options` can add sets with `WithHooks`; they run after the inherited ones. To drop the inherited sets, add an option that clears them:

```go
Options: []agent.Option{func(c *agent.Config) { c.Hooks = nil }}
```

A child's events carry its own agent name and a non-empty `Path`. `SubagentStart` and `SubagentEnd` fire in the parent, around the delegation.

## Memory extraction after a run

Memories extracted automatically belong in a hook after the run, never inline. `memory.ExtractionHook` provides that hook: on a run that finished normally it calls `Policy.ExtractAfterRun` with the agent that ran as owner and the run's branch, so every record passes the policy's scope, kind and content rules.

```go
a, err := agent.New(cfg, agent.WithHooks(
    memory.ExtractionHook(store, policy, memory.ExtractorFunc(summarizeFacts)),
))
if err != nil {
    return err
}
```

The scope always comes from the policy, never from the extractor. Idempotency keys derive from the run ID and call path, so the hook firing again for the same run stores nothing new. See [memory](memory.md).

## Guardrails

Input and output guardrails use the same runtime: the same order, timeout, panic handling and durable records. See [guardrails](guardrails.md).
