# Tool calling

How a tool call travels from the model to your code and back, which calls run at the same time, and which knob controls each step. File references point at the code that implements each rule.

- [Lifecycle of one turn](#lifecycle-of-one-turn)
- [Client tools and server tools](#client-tools-and-server-tools)
- [Parallel and sequential execution](#parallel-and-sequential-execution)
- [Model-side and saige-side parallelism](#model-side-and-saige-side-parallelism)
- [Tool choice](#tool-choice)
- [Other controls](#other-controls)
- [Where each knob lives](#where-each-knob-lives)

## Lifecycle of one turn

1. **The model asks.** The adapter streams the assistant turn. One turn can hold several tool calls (`tool_use` blocks on Anthropic, function calls on OpenAI and Gemini). The loop collects them in request order (`assistantToolCalls`, `agent/agent.go`).
2. **Guards run first.** A turn that repeats the previous turn's calls exactly is answered without running them once `MaxRepeatIterations` is exceeded. More than one handoff call in a turn fails every call in it with `ambiguous handoff` (`executeToolsConcurrently`).
3. **Each call is checked, then run** (`executeOneTool`, `agent/agent.go`), in this order:

   | Step | What happens on failure |
   | --- | --- |
   | Look up the tool in the active registry | `tool not found: <name>` |
   | Arguments the adapter could not parse (`ArgumentsError`), then `types.ValidateToolArgs` against the declared schema | Error result wrapping `types.ErrInvalidToolArguments`. The tool and the gate never see the call. |
   | `ToolGate` (plus the `ApprovalPolicy` capability defaults when `RiskDefaults` is set) may allow, rewrite the arguments, deny, or require approval | Deny: `refused: <reason>`. Arguments a person edits while approving are validated and gated again. |
   | Markers on a `types.MarkedTool` (`agent.Approval`, `agent.Markers`) wait for a decision. An approval policy grant can answer it. | Denial is a refused result |
   | Per-tool quota (`Budget.ToolQuota`), charged only once the call is cleared | `types.ErrToolQuotaExceeded` as the result |
   | Execute the tool, bounded by `ToolTimeout` | The error text becomes the result |

4. **All results go back in one message.** `persistToolResults` writes a single tool-result message with one `ToolResultPart` per call, in request order, each paired to its call by `ToolCallID` (`agent/agent.go`). Failures carry `IsError`. Every call gets a result, including when the run stops early, so the transcript stays valid for the next request. A suspended durable run is the exception: its pending calls resume later.
5. **The next model turn** reads those results, unless a stop tool ended the run (see [StopAtTools](#other-controls)) or a handoff moved control.

A failed call never ends the run on its own. The model sees the error and can correct the call. `MaxConsecutiveErrors` (default 2, `agent/forcing.go`) stops the run with `ErrToolErrorLimit` after that many turns in a row where every call failed. A refused call does not count as a failure.

## Client tools and server tools

| | Client tools | Server tools |
| --- | --- | --- |
| Runs in | Your process | The vendor's infrastructure |
| Declared with | `agent.Config.Tools`, `agent.Func`, MCP imports | `agent.WithServerTools(types.ServerTool{Kind: ...})` or catalog `server_tools` |
| Kinds | Anything you write | `web_search`, `code_execution`, `remote_mcp` (`agent/types/servertool.go`) |
| Gates, approval, quotas | Yes | No: there is no local execution to gate |
| Stream events | A tool-call part (`PartStart`, `PartDelta.Args`, `PartEnd` with the `ToolCallPart`) from the model, then `ToolExecStartDelta` and `ToolExecEndDelta` from saige | `ServerToolCallPart` and `ServerToolResultPart` parts (`agent/types/part.go`) |

saige does not execute server tools. The model calls them and answers from the result inside the same vendor turn. Anthropic (web search, code execution) and Google (search grounding, code execution) send them. The OpenAI adapters and Ollama do not, and `provider.Build` rejects server tools for them (`catalog.ExpressibleServerTools`). Anthropic remote MCP is rejected because it needs the MCP connector. Server tool calls emit no tool spans or tool metrics.

## Parallel and sequential execution

`executeToolsConcurrently` (`agent/agent.go`) decides how one turn's calls run:

| Setting | Behavior |
| --- | --- |
| `MaxParallelTools` 0 (default) | Every call in the turn runs in its own goroutine, with no cap. |
| `MaxParallelTools` n > 1 | At most n calls execute at once. A counting semaphore admits them, so which call wins a free slot is up to the scheduler. |
| `MaxParallelTools` 1, or `agent.WithSequentialTools()` | Calls run one at a time in the order the model requested them, on the run's goroutine. `WithSequentialTools` is sugar for `WithMaxParallelTools(1)`. Use it when tools share mutable state or their order matters: a semaphore of one serializes execution but not order. |

Results are returned in request order in every mode.

Only tool execution holds a slot. Approval waits and delegated sub-agents do not, so a call waiting on a person never blocks other calls from running. Because calls run concurrently by default, every `Tool`, `ToolGate`, `ToolPolicy`, result policy and result sink must be safe for concurrent use, and two calls to the same tool in one turn run on the same instance at once (`Agent` doc comment, `agent/agent.go`).

**Durable runs.** The step runner decides:

| Step runner | Tool calls in one turn |
| --- | --- |
| None (`types.NoopStepRunner`) | Parallel, as above |
| Local engine (`agent/durable/local`) | Parallel. Its runner reports `ConcurrentSteps() == true`, so `MaxParallelTools` applies. |
| duraturo engine (`agent/durable/duraturo`) | Always sequential, in request order. duraturo derives each step's record key from call order inside one workflow, so steps must run one at a time for a replay to find the same keys. Its runner reports `ConcurrentSteps() == false`. The cost is latency on turns with many calls. |
| Any other durable runner | Sequential, unless it implements `types.ConcurrentStepRunner` and returns true |

**Approvals with parallel calls.** With parallel execution, two marked calls in one turn each emit a `MarkerDelta` and wait at the same time, and each is decided on its own. A consumer answers them in any order; the CLI chat UI queues them and asks in arrival order, showing how many more are waiting (`TestRunnerParallelMarkersAreAnsweredInOrder`, `agent/tui/runner_test.go`). Approving one and denying the other runs only the approved call. With sequential execution, the next call's approval is not requested until the previous call finished. See [approval policy](approval-policy.md).

**Sub-agents.** A `delegate_to_<name>` call runs the child inside the tool call, so several delegations in one turn run concurrently under the parent's rules, without holding a parent slot. The child inherits `MaxParallelTools` and applies it to its own tools. `spawn_<name>` starts the child in the background and returns a handle at once; spawning needs an inline loop and fails with `ErrSpawnUnsupported` under a durable step runner (`agent/spawn.go`). See [delegation](delegation.md).

## Model-side and saige-side parallelism

Two independent controls:

- **Model side:** whether the model may put several calls in one turn. Set it with the `parallel` dial, the raw `parallel_tools` option, or an adapter's `WithParallelToolCalls`. OpenAI sends `parallel_tool_calls`; Anthropic sends `disable_parallel_tool_use` on the tool choice. The Gemini API and Ollama have no such control: a raw `parallel_tools` option is rejected there, and `parallel: false` is a contractual dial, so it fails the attempt (a router skips that member). `parallel: true` is advisory and dropped where it cannot apply (`agent/types/dials_compile.go`).
- **saige side:** whether saige runs the calls it received at the same time: `MaxParallelTools`.

| | saige parallel (`MaxParallelTools` 0 or > 1) | saige sequential (`MaxParallelTools` 1) |
| --- | --- | --- |
| **Model may emit several calls** | Several calls per turn, run concurrently. Fastest for independent lookups. | Several calls per turn, run one at a time in request order. One model round trip, deterministic order. |
| **Model limited to one call** (`parallel: false`) | One call per turn, so nothing overlaps. Each extra call costs a model turn. | Same as the left cell. |

## Tool choice

`types.ToolChoice` has four modes: `auto`, `none`, `required` (call some tool) and `named` (call this tool).

- **`auto` and `none` apply to every turn.**
- **`required` and `named` apply to one turn.** Set through `agent.WithToolChoice` or a preset's `tool_choice`, a forced choice applies to the first turn of each run and then reverts to auto, so a forced call cannot loop (`toolChoice`, `agent/forcing.go`). A `ConfigPart.ToolChoice` in the conversation wins over the agent's setting; a forced one is cleared by the next assistant turn.
- **`none` without option support** withholds the tools from the request, which has the same effect. A forced choice must reach the adapter: an adapter that takes no per-request options fails with `ErrInvalidModelConfig` (`toolChoiceRequest`).

Vendor and model limits, from the catalog (`agent/provider/catalog/data/default.json`):

| Model or runtime | Limit |
| --- | --- |
| claude-sonnet-5-5, claude-opus-5-5, claude-fable-5-1, claude-mythos-5-1 | `reasoning.forced_tool_choice: false`: the API rejects `required` and `named`. saige rejects them locally, and structured output uses `output_config.format`. |
| gpt-6-luna, gpt-6-sol (`chat_completions_tools: no_reasoning`) | On Chat Completions, tools work only at reasoning effort `none`. With tools and no effort, the adapter sends `none`; another raw effort fails locally. `provider.Build` serves the model on the Responses API instead when a reasoning dial is on and that API can send the request. |
| gpt-6.1-sol, gpt-6-astra (`responses_only`) | Tools need the Responses API. `provider.Build` serves these models through `openai.NewResponses` for every request. |
| Ollama | The native API has no `tool_choice`. The adapter emulates it by filtering the tools it sends: `none` sends none, `named` sends only that tool (the model may still answer without calling it). `required` cannot be emulated and is rejected (`agent/provider/ollama/tools.go`). |

## Other controls

| Control | Behavior |
| --- | --- |
| `agent.WithStopAtTools(names...)` | Ends the run when one of these tools succeeds; its result is the run's output and the model is not called again. A failed call does not stop the run (`stopToolCall`, `agent/forcing.go`). |
| `agent.WithToolTimeout(d)` | Bounds each tool execution. Time waiting for an approval is not counted. A delegation is bounded by `SubAgentDef.Timeout`, not by this; the child applies `ToolTimeout` to its own calls. Default 0, no timeout. |
| Panics | A panicking tool is recovered and answered with `tool <name> panicked: <value>`; the run continues, and the stack is logged (`toolPanic`, `agent/agent.go`). |
| Invalid arguments | Answered as an error result wrapping `types.ErrInvalidToolArguments`, so the model can retry with corrected arguments. |
| `Budget.ToolQuota(name, n)` | Caps calls to one tool for the budget's lifetime. Counted after the gate and approval, so denied calls are free. Sub-agents sharing the budget share the quota (`agent/types/tool_quota.go`). |
| `selector.DeferredTools` | A `ToolPolicy` that sends only pinned tools plus `tool_search`. The model searches (BM25 by default) and discovered tools are sent from the next turn. Register `policy.Tool()` next to the hidden tools and pass the policy with `agent.WithToolPolicy`. Discovery is disclosure, not permission: gates still apply (`agent/selector/deferred.go`). |
| `agent.Func` | Typed tool: schema from a struct, strict decoding, typed dependencies. See [typed function tools](func-tools.md). |
| `agent.Idempotent()` / `types.IdempotentTool` | After a crash mid-step, the local durable engine runs an idempotent tool again instead of waiting for `Reconcile`. See [durable execution](durable-execution.md). |
| `MaxRepeatIterations` | Stops the run with `ErrRepeatedToolCalls` when the model repeats the same calls with the same arguments more than n turns in a row. 0 disables it. |

## Where each knob lives

| Knob | Agent option | Dial | Raw option | Catalog |
| --- | --- | --- | --- | --- |
| Tool choice | `WithToolChoice` | `tools` (contractual) | `tool_choice` (catalog options take `auto` or `none` only) | preset `tool_choice` |
| Model may emit several calls | | `parallel` | `parallel_tools` | entry `options` or `dials` |
| saige runs calls concurrently | `WithMaxParallelTools`, `WithSequentialTools` | | | |
| Stop after a tool | `WithStopAtTools` | | | |
| Per-tool timeout | `WithToolTimeout` | | | |
| Tool quota | `WithBudget` with `Budget.ToolQuota` | | | |
| Tools sent per turn | `WithToolPolicy` | | | |
| Gate and approval | `WithToolGate`, `WithApprovalPolicy` | | | |
| Server tools | `WithServerTools` | | `server_tools` | entry `options.server_tools` |

See [dials](dials.md) for how a dial compiles per model and [model catalog and presets](catalog.md) for entry options.
