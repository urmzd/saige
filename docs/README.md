# saige docs

Start with [concepts](architecture/concepts.md), then open the guide for the piece you are working on. Package-level references live in each package's README; see the [root README](../README.md#documentation).

## Start here

| Document | Purpose |
| --- | --- |
| [Concepts](architecture/concepts.md) | How agent, adapters, catalog, dials, tools, RAG, evals and durable runs fit together; vendors, runtimes and adapters |
| [Message parts](guides/parts.md) | Part kinds per role, sources and locators, sending each modality, part deltas and the aggregator, wire versions 1 and 2, persistence and externalized media |
| [Deployment](guides/deployment.md) | One PostgreSQL 18 server with pgvector and pg_search, migrations, hybrid search, licensing |

## Models and configuration

| Document | Purpose |
| --- | --- |
| [Model catalog and presets](reference/catalog.md) | Catalog JSON schema, sources and layers, merge rules, precedence, validation, building a preset, CLI |
| [Dials](guides/dials.md) | Model-neutral creativity, reasoning depth, max output, tools, seed, cache and modality, compiled per model |
| [Modality conversion](guides/modality-conversion.md) | Media a model cannot take: reject by default, or convert, transcribe, describe, extract or omit; scopes, routing, memoization, budget, reports |
| [Model capabilities](architecture/model-capabilities.md) | Capability declarations, tool and cost handling per vendor, and the gaps that remain open |

## Tools and orchestration

| Document | Purpose |
| --- | --- |
| [Tool calling](architecture/tool-calling.md) | Tool call lifecycle, client and server tools, parallel and sequential execution, tool choice, where each knob lives |
| [Harness tools](guides/harness-tools.md) | The built-in toolset: file, code, and fetch tools, sandboxes, approvals, spill, CLI `--tools` |
| [Typed function tools](guides/func-tools.md) | `agent.Func`, `RunContext`, schema-hash versions, `agent.AIFunc` |
| [Nullable tool properties](guides/tool-schemas.md) | Optional and nullable parameters across vendor APIs and MCP |
| [Handoffs and subagents](guides/delegation.md) | Handoff vs delegate vs spawn: context in, data out, transcripts, size and iteration limits, sub-agent budgets, scratch, and references |
| [Orchestration policies](architecture/orchestration-policies.md) | Conversation ownership, subagent results, handoff return links, sticky routing, approval limits |
| [Context management](guides/context-management.md) | Compaction strategies, chains, triggers, summary budget, records, per-agent policy and handoff groups |
| [Agent definitions](guides/agent-definitions.md) | An agent as a versioned Markdown file: format, sources and trust, resolution and pinning, binding, CLI and MCP server |
| [Harnesses](guides/harnesses.md) | Running a saige agent in Claude Code, Codex, Gemini CLI, opencode, Cursor and ACP editors: export, launch, acp, held approvals |
| [Approval policy and grants](guides/approval-policy.md) | Grants by scope and expiry, denial limits, capability defaults, replay |
| [Run hooks](guides/hooks.md) | Lifecycle events that observe, change or abort a run; ordering, timeouts, durable replay, inheritance |
| [Guardrails](guides/guardrails.md) | Input and output checks that pass, block or rewrite; parallel mode, tripwires, built-ins, budget, relation to gates and redaction |
| [MCP client](guides/mcp-client.md) | Pooled sessions, retries, drift detection, `.mcp.json` loading |
| [Memory](guides/memory.md) | Host scope, approval-gated writes, recall modes, Postgres hybrid recall, conversation recall, retention, post-run extraction |

## Runs, caching and operations

| Document | Purpose |
| --- | --- |
| [Batch processing](guides/batch.md) | Vendor batch APIs at half price, the local fallback, durable jobs, budget, evals, `agent.RunBatch` |
| [Durable execution](architecture/durable-execution.md) | Local engine and anchor on Postgres, saved approvals, uncertain steps, reconciliation, budget receipts |
| [Cache contracts](architecture/cache-contracts.md) | Cache identity, provider cache modes, known limits |
| [Observability](guides/observability.md) | OpenTelemetry spans and metrics, error attributes, redaction |
| [Print a conversation tree](guides/tree-traces.md) | `tree.Print`: one JSON document with nodes, branches, checkpoints and metadata |
| [Load and concurrency testing](guides/load-testing.md) | The stress build tag, Postgres and live burst tests, `-cpu` benchmarks, nightly workflow |

## Changes

| Document | Purpose |
| --- | --- |
| [Upgrade notes](guides/upgrading.md) | Upgrading to this release: breaking changes, ordered migration steps, renamed symbols, stored data, wire clients and catalogs |
| [System hardening](reference/system-hardening.md) | What the hardening release changed and how each fix was verified |

## Decisions

These decisions describe saige's orchestration and cache boundaries. See [the API guide](architecture/orchestration-policies.md) and [the cache audit](architecture/cache-contracts.md) for examples and known limits. Each ADR was first recorded as `D-NN` in `DESIGN_DECISIONS.md`; ADR `00NN` is `D-NN`, and code comments and docs still cite them by that name.

| ADR | Title | Status |
| --- | --- | --- |
| [0001](adrs/0001-keep-branches-separate-from-workers.md) | Keep branches separate from workers | Accepted |
| [0002](adrs/0002-make-delegation-a-call-and-return-contract.md) | Make delegation a call-and-return contract | Accepted |
| [0003](adrs/0003-treat-handoff-as-ownership-transfer.md) | Treat handoff as ownership transfer | Accepted |
| [0004](adrs/0004-add-direct-return-links-by-default.md) | Add direct return links by default | Accepted |
| [0005](adrs/0005-select-complete-model-configurations.md) | Select complete model configurations | Accepted |
| [0006](adrs/0006-separate-cache-contracts.md) | Separate cache contracts | Accepted |
| [0007](adrs/0007-default-to-private-response-cache-identity.md) | Default to private response-cache identity | Accepted |
| [0008](adrs/0008-keep-policies-separate.md) | Keep policies separate | Accepted |
| [0009](adrs/0009-reserve-budget-before-dispatch.md) | Reserve budget before dispatch | Accepted |
| [0010](adrs/0010-save-approvals-and-release-local-workers.md) | Save approvals and release local workers | Accepted |
| [0011](adrs/0011-fail-closed-on-handoff-compaction.md) | Fail closed on handoff compaction | Accepted |
| [0012](adrs/0012-reject-unsupported-request-controls.md) | Reject unsupported request controls | Accepted |
| [0013](adrs/0013-own-catalog-snapshots-at-the-boundary.md) | Own catalog snapshots at the boundary | Accepted |
| [0014](adrs/0014-keep-uncertain-effects-explicit.md) | Keep uncertain effects explicit | Accepted |
| [0015](adrs/0015-copy-cache-values-at-ownership-boundaries.md) | Copy cache values at ownership boundaries | Accepted |
| [0016](adrs/0016-use-a-local-engine-before-a-distributed-scheduler.md) | Use a local engine before a distributed scheduler | Accepted |
| [0017](adrs/0017-allow-one-run-per-branch.md) | Allow one run per branch | Accepted |
| [0018](adrs/0018-contain-tool-failures-in-the-tool-result.md) | Contain tool failures in the tool result | Accepted |
| [0019](adrs/0019-rank-composed-gate-verdicts.md) | Rank composed gate verdicts | Accepted |
| [0020](adrs/0020-bound-delegations-separately-from-tools.md) | Bound delegations separately from tools | Accepted |
| [0021](adrs/0021-apply-response-schemas-to-the-final-answer.md) | Apply response schemas to the final answer | Accepted |
| [0022](adrs/0022-compact-on-input-pressure-and-never-drop-context-silently.md) | Compact on input pressure and never drop context silently | Accepted |
| [0023](adrs/0023-commit-truncated-turns-never-their-tool-calls.md) | Commit truncated turns, never their tool calls | Accepted |
| [0024](adrs/0024-force-behavior-for-one-turn-and-bound-unproductive-loops.md) | Force behavior for one turn and bound unproductive loops | Accepted |
| [0025](adrs/0025-join-active-runs-only-at-safe-points.md) | Join active runs only at safe points | Accepted |
| [0026](adrs/0026-escalate-on-outcomes-through-the-tree.md) | Escalate on outcomes through the tree | Accepted |
| [0027](adrs/0027-disclose-tools-by-search-permit-them-by-gate.md) | Disclose tools by search, permit them by gate | Accepted |
| [0028](adrs/0028-skills-supply-content-and-never-grant-permission.md) | Skills supply content and never grant permission | Accepted |
| [0029](adrs/0029-spawn-background-children-with-handles.md) | Spawn background children with handles | Accepted |
| [0030](adrs/0030-address-every-decision-as-an-interrupt.md) | Address every decision as an interrupt | Accepted |
| [0031](adrs/0031-swap-personal-data-for-placeholders-at-the-tool-boundary.md) | Swap personal data for placeholders at the tool boundary | Accepted |
| [0032](adrs/0032-address-scratch-artifacts-by-content.md) | Address scratch artifacts by content | Accepted |
| [0033](adrs/0033-scope-memory-by-host-write-it-by-approval.md) | Scope memory by host, write it by approval | Accepted |
| [0034](adrs/0034-keep-shadow-traffic-out-of-the-caller-s-run.md) | Keep shadow traffic out of the caller's run | Accepted |
| [0035](adrs/0035-keep-the-wire-format-a-stable-contract.md) | Keep the wire format a stable contract | Accepted |
| [0036](adrs/0036-report-malformed-tool-arguments-to-the-model-truncated-ones-to-the-caller.md) | Report malformed tool arguments to the model, truncated ones to the caller | Accepted |
| [0037](adrs/0037-declare-presets-as-data-and-validate-every-chain-entry.md) | Declare presets as data and validate every chain entry | Accepted |
| [0038](adrs/0038-let-approvals-grant-scope-and-record-every-grant.md) | Let approvals grant scope, and record every grant | Accepted |
| [0039](adrs/0039-give-hooks-one-seam-and-record-what-they-change.md) | Give hooks one seam and record what they change | Accepted |
| [0040](adrs/0040-guard-the-conversation-s-edges-gate-the-tools.md) | Guard the conversation's edges, gate the tools | Accepted |
| [0041](adrs/0041-compose-compaction-strategies-and-record-every-compaction.md) | Compose compaction strategies and record every compaction | Accepted |
| [0042](adrs/0042-save-a-batch-job-before-submitting-it-and-batch-single-turns-only.md) | Save a batch job before submitting it, and batch single turns only | Accepted |
| [0043](adrs/0043-define-an-agent-as-a-versioned-file-and-pin-what-a-run-resolves.md) | Define an agent as a versioned file and pin what a run resolves | Accepted |
| [0044](adrs/0044-messages-are-ordered-typed-parts.md) | Messages are ordered typed parts | Accepted |
| [0045](adrs/0045-convert-the-view-never-the-record-reject-by-default.md) | Convert the view, never the record; reject by default | Accepted |
| [0046](adrs/0046-an-offering-is-a-model-on-an-endpoint-and-its-parameters-are-data.md) | An offering is a model on an endpoint, and its parameters are data | Accepted |
| [0047](adrs/0047-never-take-a-vendor-file-id-from-an-untrusted-client.md) | Never take a vendor file ID from an untrusted client | Accepted |
| [0048](adrs/0048-host-an-agent-in-other-harnesses-through-one-session-layer.md) | Host an agent in other harnesses through one session layer | Accepted |
