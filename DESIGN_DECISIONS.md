# Design decisions

These decisions describe Saige's orchestration and cache boundaries.
See [the API guide](docs/orchestration-policies.md) and [the cache audit](docs/cache-contracts.md) for examples and known limits.

## D-01: Keep branches separate from workers

A branch records an alternate conversation path. It does not establish a process, tool, or database boundary.
Each subagent invocation gets a new tree. The host isolates external state when required.
This avoids a false isolation guarantee from a thread-safe tree.

## D-02: Make delegation a call-and-return contract

A subagent always returns to its caller. Its default output is the final assistant message.
The complete committed trace remains available through a result handle or sink.
A result policy selects and validates data for an upstream service.
This prevents intermediate thoughts from becoming an apparent final answer.
A failed child returns an error and partial trace, not successful output.
Spawn mode keeps the return but makes it asynchronous. D-29 records how.

## D-03: Treat handoff as ownership transfer

A handoff recipient can finish, continue elsewhere, or transfer back when it cannot answer.
There is no mandatory return to a waiting caller.
The audit tree retains all messages. The default provider view contains only the owner's context and transfer briefs.
A returning owner resumes its earlier view.
This supports triage without copying every specialist's transcript into every request.

## D-04: Add direct return links by default

Every declared handoff edge gets a reverse edge by default.
This gives a specialist a route back when the task is outside its scope.
A strict directed policy can remove that behavior at a trust boundary.
The transfer limit remains necessary because a return edge can also create a loop.
Multiple transfers in one turn are rejected because one conversation cannot have two simultaneous owners.

## D-05: Select complete model configurations

A route refers to an immutable provider configuration, not only a model string.
Each configuration has its own reasoning, sampling, endpoint, and cache settings.
The routing policy acts after context and tool selection, before provider execution.
Sticky selection is the default because repeated prefixes benefit from stable provider affinity.
A transient failure permits failover before output commits the attempt. Usage reported before any output is held back and dropped on failover, so one call never carries usage from two rate cards.
After commitment, the request fails rather than mixing streams from two providers.
Failover is classified: transient errors and context-length errors move on, and a context-length failure moves only to a larger context window. A refused prompt does not move unless the host says so.
The affinity policy leaves a profile only after a sustained run of failures, not one error, and returns to the preferred profile only when the cost of rewriting a warm cache prefix allows it.
Route locks keep a conversation where its state lives. A soft lock (an open tool loop, a bound context cache) blocks voluntary switches but allows failover. A hard lock (signed reasoning that must return to its model) also blocks failover and outranks a pin.

## D-06: Separate cache contracts

A response cache reuses output. A prompt cache reuses provider computation.
A durable step record prevents repeated work during recovery. A journal records authoritative changes.
These layers have different identity, expiry, billing, and correctness requirements.
No cache substitutes for a journal or a durable budget ledger.
The cache audit lists unsupported combinations and remaining defects.

## D-07: Default to private response-cache identity

The SDK cannot infer a tenant's authorization scope or the meaning of a deployment revision.
A cache wrapper therefore uses a private identity unless the host supplies scope and configuration keys.
Explicit sharing remains possible, but the host owns that contract.
A model name alone is insufficient because two configurations of one model can produce different results.
Agent stores take tenant scope the same way: `pgstore.ScopedConversationID` and `NewScopedStore` place the scope as a namespace inside `conversation_id`, so two tenants never read each other's nodes.

## D-08: Keep policies separate

Tool disclosure, execution permission, context selection, result selection, links, routing, and budgets use separate contracts.
A skill can supply content. It cannot grant permissions by itself.
A memory service can supply evidence. It cannot silently change system authority.
This allows a service to replace one policy without replacing the agent loop.

## D-09: Reserve budget before dispatch

A shared `Budget` reserves request, token, and cost capacity under one lock.
This prevents concurrent children from spending the same available allowance.
The host supplies per-call upper bounds. Without a bound, a call reserves the remaining allowance for that limit.
For example, four reservations of $0.25 can occupy a $1 allowance. A fifth call receives `ErrBudgetBusy`.
Settlement releases unused capacity and records actual usage once. Missing usage consumes the reservation and remains uncertain.
Local durable runs save reservations before dispatch and restore settlement receipts during replay.
This does not include a distributed account, provider-native fees, or cache storage invoices.

## D-10: Save approvals and release local workers

Streaming approvals park a goroutine but do not hold a regular tool execution slot.
The local durable engine saves each request and returns `ErrSuspended` after the current batch drains.
Completed siblings remain saved. A later worker replays those results and resumes the approved call.
For example, an independent read can finish while a write waits for approval.
Decisions require the original run revision and an idempotency key. Changed decisions and expired requests fail.
The host authenticates users and preserves separate decisions when its interface groups approvals.
A local process lock protects one run. Remote workers still need a shared lease and fencing contract.
The `types.InterruptRouter` contract is served by the in-process router and both durable engines (`local.Engine.Router`, `duraturo.Engine.Router`).
The duraturo engine records each interrupt and each reply as a ledger record. A run waiting for a reply parks off the queue and holds no worker; the reply's record plus an enqueue resumes it.
Because the reply is a write-once record, a repeated reply is a no-op and a changed one is refused.

## D-11: Fail closed on handoff compaction

A shared summary can erase ownership boundaries and expose another owner's context.
Automatic compaction is therefore rejected for handoff groups.
Per-owner checkpoints and summaries must exist before this restriction can be removed.
This is a deliberate limit, not an implicit promise that long handoff sessions fit every model.

## D-12: Reject unsupported request controls

Adapters validate configured controls before they send a request.
For example, temperature on an `o3` request returns `ErrInvalidModelConfig`.
The adapter does not silently remove the requested value.
This makes a configuration error visible and prevents retrying the same invalid request.
Each route must have settings that its own model accepts.
Validation preserves prompt cache options and reported cache usage.

Raw request options are rejected, never stripped. Dials are intents, not vendor parameters: an advisory dial (creativity, reasoning, cache, parallel on) that a model cannot honor is mapped to the nearest value the model declares, or dropped. Max output is lowered to the model's limit, never raised. A contractual dial (tool choice, parallel off, seed) is rejected, and a response schema stays under D-21. Each decision is recorded as data on the route, the tree, the span and the eval, never only as a log line.
A raw option that sets the same parameter as a dial wins and is validated strictly. Reasoning outranks creativity: when the effective reasoning rules out sampling controls, creativity is dropped and recorded.
A `DialPolicy` can tighten any dial to reject, and `StrictDials` rejects every dial a model cannot honor exactly. A contractual dial is loosened only by naming it. There is no global lenient switch.
Dials compile per attempt, against the attempt's model and request, so failover re-targets them. In a router, a contractual rejection removes a member from the request; an advisory decision never does.

## D-13: Own catalog snapshots at the boundary

The catalog copies mutable metadata on registration and on return.
A caller can edit a lookup result without changing another session or a stored revision.
For example, changing a returned effort list does not modify the next lookup.
A deliberate change requires a new registration, which records a revision.
An exact model declaration sets `Known=true`. A family inference sets it to false.
This separates a declared model from an unverified variant that has a similar name.

## D-14: Keep uncertain effects explicit

A completed step is saved before its result returns. A started step is saved before its function runs.
A crash between those records does not prove whether an external write succeeded.
Recovery therefore stops with `ErrIndeterminate`; it does not repeat the write automatically.
The host checks the external system, then supplies a result or explicitly permits retry through `Reconcile`.
For example, check a payment's idempotency key before allowing another payment attempt.
Local snapshots cannot provide an atomic transaction with an external API.

## D-15: Copy cache values at ownership boundaries

A read must not let one caller alter another caller's future result.
Tool results and embedding vectors are copied before storage and before return.
Embedding keys include the full input, including binary data and MIME type.
Tool keys include a configuration revision, scope, policy, arguments, and declared context.
Invalid key data fails explicitly. No fallback string is treated as a stable identity.
Stale data stays in storage through its stale window, but only a successful old result can mask a refresh failure.

## D-16: Use a local engine before a distributed scheduler

The local engine uses process locks, synced files, and atomic rename.
This gives one machine a testable crash boundary without a queue or database deployment.
An OS lock releases when its process exits. Pending approvals need no resident worker.
Snapshots use private files and contain versioned recovery data plus ordered event metadata.
They are not a portable trace format or an unbounded production journal.
The duraturo engine is the distributed option. duraturo supplies fenced leases, a queue, and a write-once record ledger on Postgres tables the host owns.
Retention of finished runs remains a host policy.
See [durable execution](docs/durable-execution.md) for the failure table and deployment limits.

## D-17: Allow one run per branch

A run appends input, assistant turns, and tool results to the tip of its branch.
Two runs on one branch would interleave those writes and separate a `tool_use` from its `tool_result`.
`Invoke` and `RunDurable` therefore claim the branch for the length of the run. A second run on the same branch fails with `ErrRunActive`.
The claim belongs to the tree, not the `Agent`, because several agents and handoff members can share one tree.
Runs on different branches of one tree may overlap.
A message for a busy branch goes through `Submit` instead (D-25). `LoadSession` also fails while any run uses the tree.

## D-18: Contain tool failures in the tool result

A tool, gate, or handoff tool that panics produces an error result for that call. The process and the other runs continue.
Under a durable runner the panic fails the step instead, so the engine records an uncertain effect rather than a completed result.
Arguments that are not valid JSON, or that miss a required property or have the wrong top-level type, never reach the gate or the tool.
The model receives the reason and can correct the call.
A stream that ends with a tool call still open fails the turn with `ErrResponseTruncated`. Its incomplete arguments never run.
A file that cannot be resolved or extracted becomes a text notice and a logged warning, never silent omission or raw bytes.

## D-19: Rank composed gate verdicts

`Gates` runs every gate and returns the most restrictive verdict: deny, then approval, then allow.
An approval from an early gate cannot hide a later denial.
When a person edits arguments during approval, the edited call is validated and gated again.
A denial still refuses it. A second approval is not requested, because a person already decided on those arguments.

## D-20: Bound delegations separately from tools

The parent's `ToolTimeout` bounds each tool call, including each call a child makes. It does not bound a delegation as a whole.
`SubAgentDef.Timeout` bounds one complete child run. The clock pauses while a child approval waits for a person.
A run with no consumer cannot answer approvals. A delegated child in such a run fails at once, as the parent's own tools do.
A child receives a durable approval runner only when the parent's runner resolves approvals durably.

## D-21: Apply response schemas to the final answer

A response schema is never dropped silently. A run that cannot apply it fails with `ErrInvalidModelConfig` before any request.
`OutputMode` selects the path. `native` sends the schema through the provider's structured output support.
Most providers cannot combine a native schema with tool calls in one request.
With tools present, the loop sends tools without the schema. A turn that ends without tool calls and does not already satisfy the schema is discarded.
The loop then repeats that request once with the schema and without tools, and records the structured reply.
`tool` offers a `final_answer` tool whose parameters are the schema, so tools and schema travel together and no extra turn is needed.
The run ends when that tool succeeds. Invalid arguments return to the model as a tool error inside the run.
`prompt` describes the schema in the system prompt for that request only. It suits models with neither structured output nor tool calling.
`WithResponseSchema` defaults to `native`. `Structured` prefers `tool` when the agent has tools, then `native`, then `tool`, then `prompt`.
`Structured` validates the answer recursively and with the caller's validator. A failure returns to the model with the error, up to `Repair` times.
Text answers pass through tolerant extraction, because local and reasoning models wrap JSON in fences, prose, or `<think>` blocks.
Outside `Structured`, a `tool` or `prompt` run that ends with text checks that text against the schema. A mismatch returns to the model with the error, at most twice per user turn. After that the run fails with `ErrSchemaInvalid`.
A sub-agent with a `ResponseSchema` uses `native` when its final provider supports it and `tool` otherwise. The mode is chosen after `SubAgentDef.Options`, which may replace the provider. Its default result policy validates the JSON, and a mismatch fails the delegation.
Schema validation covers types, required properties, enums, nested objects, and array items. Business rules belong in the validator.

## D-22: Compact on input pressure and never drop context silently

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

## D-23: Commit truncated turns, never their tool calls

A turn that stops at the output token limit can end with a tool call whose arguments are cut off.
Running it could act on partial input. The loop therefore never runs any tool call from a truncated turn.
The completed text is committed and the stream sends `TruncatedDelta` with the node and the finish reason.
The committed node carries a `TruncationContent` marker with the reason. The tree stores it, and the loop strips it before the next provider call.
A truncated text-only turn ends the run cleanly unless `WithAutoContinue` or a queued message resumes it (D-25). The delta shows the answer is partial.
A truncated turn that requested tools ends the run with `ResponseTruncatedError`.

## D-24: Force behavior for one turn and bound unproductive loops

A forced tool choice that stays in effect makes the model call the same tool on every turn.
A required or named choice therefore applies to one turn and then reverts to auto. Auto and none stay in effect.
A forced choice must reach the provider through `OptionsProvider`; otherwise the request is rejected. None is emulated by withholding tools.
Three guards stop a run that does not progress: the iteration cap, `MaxConsecutiveErrors` (default 2), and `MaxRepeatIterations`.
A call that a gate or a person refuses is a policy outcome, not a tool fault. It does not count toward `MaxConsecutiveErrors`.
Repeated calls are answered with an error result and not run, so each call keeps a result.
`OnMaxIter: MaxIterForceFinal` replaces the guard's error with one tool-free call that asks for a final answer.
That instruction is sent with the request only. The tree records the answer, not the instruction.

## D-25: Join active runs only at safe points

A message for an active run must never land between a `tool_use` and its `tool_result`.
`EventStream.Submit` and `Agent.Submit` hold it until a safe point: the top of an iteration, after tool results, or where the run would finish.
Queue waits for the finish and starts a new user turn on the same stream with fresh step limits. Steer joins at the next safe point and cancels nothing.
InterruptReplace cancels only the provider call in flight. Its completed text is committed as a truncated turn and its tool calls are dropped. Tools already running finish first. The replacing message starts a new user turn with fresh step limits, so it is always answered.
Side branches from the last safe point and runs separately, so it never writes to the original branch.
Every accepted message is either appended, with `InjectedDelta`, or returned by `Undelivered` after the run ends. A message sent after the final safe point fails with `ErrRunFinished`.
`Continue` resumes the last assistant turn with prefill when the provider declares it, and otherwise with a continue prompt. `WithAutoContinue` does the same for a text-only turn cut off by the output limit. A cut-off tool call still fails the run.
Runs without a consumer, and runs under a durable step runner, do not take submissions, because their transcript must replay without outside input.
A run that moves to a compacted branch extends its claim to it, so a message addressed to the active branch joins that run rather than starting a second one.

## D-26: Escalate on outcomes through the tree

Some failures are signals about the model, not the request: structured output that never validates, or a sub-agent that fails.
An `OutcomePolicy` observes these outcomes and may return a `Switch` to another model.
The agent records an accepted switch as `ConfigContent{Model, Reason}` on the branch and sends a `RouteDelta` with the reason.
Recording it in the tree makes the switch hold for later turns, survive a reload, and appear in the audit trail.
A sub-agent failure is observed after the turn's tool results are recorded, so a switch never separates a call from its result.
`Structured` observes `schema_invalid` after its repairs run out, then repairs again on the new model, at most three switches per call.
A `Switch` can also carry dials, recorded as `ConfigContent{Dials}`, so a policy can raise reasoning depth on the same model before it moves to another one.
A switch is ignored only when it changes neither the model nor the dials, so a ladder that has reached its top ends the escalation.
Cancellation is not a failure and is never reported. A policy should be deterministic, because a durable run replays its outcomes.
The policy is not inherited by sub-agents. The parent observes a child's failure and decides for itself.

## D-27: Disclose tools by search, permit them by gate

Sending every schema on every turn costs tokens and lowers tool-choice accuracy once an agent has many tools.
`selector.DeferredTools` sends pinned tools and `tool_search`. A search returns the full schemas of its matches and discovers them for the rest of the conversation, keyed by owner, conversation, and the branch the run started on. The conversation is the tree's root node ID, which is random per tree, so agents that share a name and a policy, and repeated delegations to one sub-agent, never share discovery.
Without `tool_search` in the turn's tools nothing is hidden, because a tool with no route to it is unreachable.
Disclosure is not permission. A discovered tool passes the `ToolGate` on every call.
`Selector[T]` is the shared ranking seam. The default BM25 is lexical, local, and deterministic, so the same catalog and query always disclose the same tools.
`ToolDef.Capability` lets `CapabilityGate` decide by class. Reads run, writes ask, destructive calls are denied, and undeclared tools ask, so a tool that says nothing about itself fails closed.
`Budget.ToolQuota` caps calls per tool. A call is charged at dispatch, after the gate and any approval, so a denied call never uses the allowance. An exhausted quota is the call's error result, and sub-agents sharing the budget share the quota.

## D-28: Skills supply content and never grant permission

A skill is an instruction package. Its text is guidance, not authority.
Sources are read in order and the first definition of a name wins, except that an untrusted definition never shadows a trusted one. Otherwise an untrusted project checkout could replace a trusted skill that an allow list admits by name. Each package is copied into an immutable snapshot with a hash per file and one over all paths and hashes, within bounds of 200 files, 5 MiB per file, and 20 MiB in total. A package over a bound is rejected whole.
A directory source reads through `os.Root`, so a link that leaves the root rejects the package. Resource reads use only manifest paths, are re-hashed before they are returned, and never touch the disk.
The system prompt lists names and descriptions. `load_skill` returns the instructions as a tool result, never as system content. The wrapper tag ends in a digest of the body, so the body cannot close it early.
`allowed-tools` only narrows: the effective tools are the agent's tools intersected with the list. It is never a pre-approval, and argument patterns match no tool. The inner tool policy chooses only among the allowed tools, and `tool_search` stays visible, so an allowed tool that is still deferred can be found. The active skill is kept per conversation, like tool discovery.
`AllowList` admits all trusted skills, none, or named ones. Its zero value admits none. Skills from an untrusted source are reachable only by name. A sub-agent with a parent in `AllowListPolicy` reaches only what its parent reaches.
The skill tools never run a script. Execution needs a separately registered tool behind its own gates.
A hidden skill and a missing one return the same error, so the model cannot probe for skills it was not shown.

## D-29: Spawn background children with handles

D-02 makes delegation call-and-return: the parent's tool call waits for the child. Spawn mode amends it for work the parent should not wait on.
`SubAgentDef.Mode: SubAgentSpawn` registers `spawn_<name>`. It starts the child under the run's context and returns a handle at once. The handle ID is the spawning tool call ID.
The child still returns to its caller. Its result is the `await_subagent` tool result when the parent asks for it. Otherwise it is appended as a user message at the parent's next safe point (D-25), in completion order, once per handle.
A run does not finish while a spawned child runs or its result is undelivered. It waits where it would finish and resumes with the result. A run that ends any other way, by an error, cancellation, or a stop tool, cancels its children before its stream closes.
Each spawn reserves budget under its handle ID before the child starts (D-09). The child's first provider call uses that reservation, and an unused one is released when the child ends. A busy budget refuses the spawn rather than queue it.
Spawn needs the inline runner. A durable runner cannot replay a child that outlives the step that started it, so the spawn tool returns `ErrSpawnUnsupported` there.
The run keeps each child's result. `search_subagent` and `read_subagent` read a transcript without loading it into the parent's context. Each child keeps its own tree (D-01).
Child output is data. The injected message wraps it in a tag that the output cannot close.

## D-30: Address every decision as an interrupt

Approvals, budget escalations, and clarifications use one protocol. Each `MarkerDelta` posts a `types.Interrupt` with an ID derived from the run ID, the call path, and the tool call, unique within the run, the call path from the root run, a kind, and an optional deadline.
A parent forwards a child's interrupt with the same ID and deadline, so the root stream lists every pending decision in the run and a reply by ID reaches the right child.
An unanswered interrupt never becomes a yes. With `WithInterruptExpiry` it expires to a denial by default. `fail` stops the run with `ErrInterruptExpired`. `escalate` re-posts it once to the caller one level up with a fresh deadline, then denies.
Replies are idempotent by ID and idempotency key, also after the run consumed the first reply, so a host can retry a reply safely.
A child starts with the task alone by default. `ContextFork` copies the parent branch without the delegating turn and without thinking blocks, whose signatures belong to one provider. `ContextFiltered` lets a selector choose.
A `<caller>` block tells the child its call path and that its final message is its whole result. The harness, not that text, refuses delegation to an ancestor (D-08).

## D-31: Swap personal data for placeholders at the tool boundary

Redaction is a separate policy from gates (D-08). A gate decides whether a call runs. A `ToolRedactor` decides what each side of the tool boundary sees.
`privacy.Vault` swaps each detected value for a placeholder such as `<<EMAIL_1>>`. The same value gets the same placeholder for the whole session, so the model can refer to it across turns. Permanent `[REDACTED:LABEL]` from `privacy.Redact` is a different thing: it cannot be reversed.
The loop restores arguments inside the tool step, after the gate and any approval saw the placeholders, and tokenizes the result and any error inside the same step. The provider, the tree, telemetry, and a durable runner's journal hold only placeholders.
A result that cannot be redacted is withheld as an error, never passed through. A request the provider decorator cannot tokenize is not sent.
Detectors run as one combined regular expression with a validator per pattern (Luhn, SSN ranges, IBAN mod 97, IPv4 octets), so a long digit run is not a card unless it checks out. A rejected match does not hide its text from the other patterns: the detector then scans with each pattern alone, so a phone number next to a house number is still found. A named-entity detector plugs into the same span pipeline through `privacy.Chain`, so its findings are reversible too.
Streamed text is restored with a hold-back: a suffix that could still become a placeholder waits for the next fragment, and is flushed at the end of the block, on completion, and on error. Thinking blocks are never rewritten, because their signatures cover the text the model produced.
`privacy.Provider` serves hosts that only need to keep data away from the model vendor. Its tree then holds real values.
The vault snapshot holds the original values. The host stores it with the same protection as the data.

## D-32: Address scratch artifacts by content

A workspace stores artifacts by SHA-256 digest, under a name that points at the latest content. Writing the same bytes again returns the same reference, so a replayed durable step writes nothing new (D-14).
The directory backend writes every file to a temporary name, syncs it, and renames it into place, so a crash never leaves a partial artifact.
`workspace.Spill` replaces a tool result over its threshold with a preview and a `saige-artifact://` URI that `scratch_read` pages through. Nothing is dropped from context without a way back (D-22). When the store refuses the write, the full result is returned. Approval markers stay outermost when a marked tool is wrapped.
The loop attaches its workspace to each tool call. A sub-agent receives a read-only view, so it can read what its caller saved but cannot change it.

## D-33: Scope memory by host, write it by approval

The host maps the calling agent to a memory scope. The model never names a tenant, and a call without a resolvable scope fails.
Memory writes are tool calls that carry an approval marker by default. Automatic extraction belongs in a host hook after the run, through `Policy.Remember`, never inline.
Every write passes the policy's content check. Without `Policy.Redact`, content in which the detector finds anything is rejected, so a missing redactor fails closed.
A tool write uses the tool call ID as its idempotency key, so a replayed call stores one record.
Recall is external input. Records are wrapped in a tag whose name ends in a digest of the content, so stored text cannot close it, and injected recall is a user message, never system content.
A sub-agent should get a read-only, narrowed scope. A narrowed scope sees its own namespace and those below it, never its parent's.

## D-34: Keep shadow traffic out of the caller's run

A shadow arm mirrors a sample of requests to another configuration to compare it.
It reserves from its own required budget, so shadow spend is never charged to, or hidden in, the primary budget.
It never executes tools: the tool calls it asks for are counted, not run.
Place the split inside the privacy decorator, so a mirrored request carries the same placeholders as the served one and personal data never reaches the shadow provider.

## D-35: Keep the wire format a stable contract

Wire kinds and error codes are never renamed. A breaking change to an envelope raises `WireVersion`, and readers reject versions they do not know.
New fields are optional and omitted when empty, so an older reader still decodes a newer envelope of the same version.
A marker carries its interrupt on the wire, so a remote client can reply by interrupt ID and see the deadline.
A persisted tree carries its own format version, `tree.TreeFormatVersion`, under the same rule. A tree saved before the field existed reads as version 1.

## D-36: Report malformed tool arguments to the model, truncated ones to the caller

A tool call whose arguments the model finished writing but that do not decode ends with an argument error. The loop answers it with an "invalid tool arguments" result before any gate runs, so the model can correct the call.
A call cut off by the output token limit, or by a stream that ended early, is never closed. The turn fails as truncated or incomplete (D-23).

## D-37: Declare presets as data and validate every chain entry

A preset is an ordered chain of complete configurations in the catalog, not a model string with options copied across vendors. Each entry is resolved for its own model and built into its own adapter, so failover changes the configuration only to one someone wrote down.
Options resolve field by field, lowest first: the adapter's default, the model row's defaults, the preset's options (unless the entry opts out with `inherit: none`), the entry's options, then the entry's `unset`. A per-request override is merged last, by the adapter. One reasoning control wins whole.
Catalog layers merge model rows field by field, as JSON merge patches. Presets merge whole: an overlay preset replaces the base preset of the same name, and a variation is a new preset that extends it.
An option an entry cannot honor is rejected at load time with the path and the layer it came from, never stripped (D-12). The fix is always written in the file.
Dials resolve in the same layers and are compiled for each entry's own model at load time. A contractual dial the model cannot honor is an error with its path and layer; an advisory one that is mapped or dropped is a `dial_mapped` or `dial_dropped` warning. Every value a row declares for a dial must pass that row's validation and be expressible by its adapter, so a bad mapping fails at load time. The configuration hash covers the compiled mapping.
The embedded catalog is the single source of truth for model rows; a golden test freezes what it resolves. Hosts load further layers through a `Source`, and only `Install` or `Use` changes what `Lookup` returns.
Failover across vendors is opt-in. The CLI default runs one vendor, the first that can serve, and a preset names its chain when failover is wanted. Authentication and content-filter failures end the request unless the preset says otherwise.
A layer that arrives with a repository is checked against an allowlist, not a denylist, so a new field stays closed to it until someone decides.

## D-38: Let approvals grant scope, and record every grant

Asking about every held call trains people to approve without reading. An `ApprovalPolicy` lets an approval carry a grant: once, the tool, matching arguments, or the conversation, with an optional expiry. Later calls the grant covers run without asking.
Only the host creates grants, from the decisions it delivers. The model's text and arguments, tool and skill output, and gates cannot (D-08, D-28).
Grants never cover a destructive tool, and neither does the opt-in approval ramp. A tool people denied too often is refused with the reason, or hidden from the model.
Each decision is recorded as metadata in the tree next to the call's result, and the policy's state is rebuilt from those records at the start of each run, so a restored conversation decides alike. Compaction writes the whole state onto its new branch as one snapshot record. A turn that ends before its results are written loses that turn's records, which at worst asks again.
Under a durable runner the verdict on each call is a recorded step, so a replay decides the same way after a grant expired. The decision that created a grant is saved with the approval.
State is per conversation. A sub-agent starts with none, so a grant never crosses a delegation.

## D-39: Give hooks one seam and record what they change

`agent.Hooks` is the single place host code observes a run: run start and stop, user input, model calls, tools, compaction, turns, sub-agents, and interrupts. Sets run in the order they were added and sub-agents inherit them, like the gate (D-08).
Most points only observe. A point may change its event or abort the run only where the loop can absorb it: a changed message, arguments, result, or task, a skipped compaction, or an abort at a safe point. An aborted tool call still gets a result, so pairing holds (D-18), and hooks never add or remove messages.
Each call is bounded by a timeout, and a panic or a late return is a failure. A failure aborts at an abortable point and is logged at an observing one. The agent waits for a hook rather than abandon it, because a hook still running would race with the run.
Under a durable runner the outcome of every changing or aborting point is a recorded step, so a replay applies it without calling the hook (D-14). Observing hooks run again on replay.
Post-run work, such as the memory extraction D-33 calls for, runs in `RunStop`, after the branch is released and on a context the run's cancellation does not reach.

## D-40: Guard the conversation's edges, gate the tools

Guardrails check what enters and leaves a conversation: the user's message and the final answer. Each returns pass, block, or rewrite, and an error fails closed. They run on the hook seam (D-39) and are recorded the same way, with the usage and receipts of any model call they make, so a replay neither calls nor charges a classifier twice.
A `ToolGate` decides whether a tool call runs (D-08, D-19). A guardrail never sees tool calls, and a gate never sees the user's message. An answer that arrives through a stop tool is a tool result, checked with an `AfterTool` hook.
Privacy redaction swaps values for placeholders that tools can restore (D-31). A guardrail rewrite is permanent and changes what is recorded. Redact input with a sequential guardrail when the model must never see a value, and with a vault when tools still need it.
A block is a tripwire: a typed `GuardrailTrippedError`, a `GuardrailDelta`, and a record in the tree. A blocked input is not recorded, and a blocked answer is not committed.
A parallel input guardrail races the first model call to save latency. When it blocks, the call is cancelled, its turn discarded, its usage charged, and the cancellation recorded. It cannot rewrite, because the model already has the text, so a rewrite counts as a block.
Output guardrails run after the answer streamed. A rewrite is announced with its replacement text; hosts that must never show raw output put `privacy.Provider` in front of the model.
Model calls a guardrail makes go through the run's budget, admitted and charged like a turn (D-09).
