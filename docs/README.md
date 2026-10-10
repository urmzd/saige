# saige docs

Start with [concepts](concepts.md), then open the guide for the piece you are working on. Package-level references live in each package's README; see the [root README](../README.md#documentation).

## Start here

| Guide | Covers |
| --- | --- |
| [Concepts](concepts.md) | How agent, adapters, catalog, dials, tools, RAG, evals and durable runs fit together; vendors, runtimes and adapters |
| [Deployment](deployment.md) | One PostgreSQL 18 server with pgvector and pg_search, migrations, hybrid search, licensing |

## Models and configuration

| Guide | Covers |
| --- | --- |
| [Model catalog and presets](catalog.md) | Catalog JSON schema, sources and layers, merge rules, precedence, validation, building a preset, CLI |
| [Dials](dials.md) | Model-neutral creativity, reasoning depth, max output, tools, seed and cache, compiled per model |
| [Model capabilities](model-capabilities.md) | Capability declarations, tool and cost handling per vendor, and the gaps that remain open |

## Tools and orchestration

| Guide | Covers |
| --- | --- |
| [Tool calling](tool-calling.md) | Tool call lifecycle, client and server tools, parallel and sequential execution, tool choice, where each knob lives |
| [Harness tools](harness-tools.md) | The built-in toolset: file, code, and fetch tools, sandboxes, approvals, spill, CLI `--tools` |
| [Typed function tools](func-tools.md) | `agent.Func`, `RunContext`, schema-hash versions, `agent.AIFunc` |
| [Nullable tool properties](tool-schemas.md) | Optional and nullable parameters across vendor APIs and MCP |
| [Handoffs and subagents](delegation.md) | Handoff vs delegate vs spawn: context in, data out, transcripts, size and iteration limits, sub-agent budgets, scratch, and references |
| [Orchestration policies](orchestration-policies.md) | Conversation ownership, subagent results, handoff return links, sticky routing, approval limits |
| [Context management](context-management.md) | Compaction strategies, chains, triggers, summary budget, records, per-agent policy and handoff groups |
| [Agent definitions](agent-definitions.md) | An agent as a versioned Markdown file: format, sources and trust, resolution and pinning, binding, CLI and MCP server |
| [Harnesses](harnesses.md) | Running a saige agent in Claude Code, Codex, Gemini CLI, opencode, Cursor and ACP editors: export, launch, acp, held approvals |
| [Approval policy and grants](approval-policy.md) | Grants by scope and expiry, denial limits, capability defaults, replay |
| [Run hooks](hooks.md) | Lifecycle events that observe, change or abort a run; ordering, timeouts, durable replay, inheritance |
| [Guardrails](guardrails.md) | Input and output checks that pass, block or rewrite; parallel mode, tripwires, built-ins, budget, relation to gates and redaction |
| [MCP client](mcp-client.md) | Pooled sessions, retries, drift detection, `.mcp.json` loading |

## Runs, caching and operations

| Guide | Covers |
| --- | --- |
| [Batch processing](batch.md) | Vendor batch APIs at half price, the local fallback, durable jobs, budget, evals, `agent.RunBatch` |
| [Durable execution](durable-execution.md) | Local engine and duraturo on Postgres, saved approvals, uncertain steps, reconciliation, budget receipts |
| [Cache contracts](cache-contracts.md) | Cache identity, provider cache modes, known limits |
| [Observability](observability.md) | OpenTelemetry spans and metrics, error attributes, redaction |
| [Print a conversation tree](tree-traces.md) | `tree.Print`: one JSON document with nodes, branches, checkpoints and metadata |

## Changes

| Guide | Covers |
| --- | --- |
| [Upgrade notes](upgrade-notes.md) | Behavior changes that can affect existing code |
| [System hardening](system-hardening.md) | What the hardening release changed and how each fix was verified |
| [Design decisions](../DESIGN_DECISIONS.md) | The choices behind the design and their limits |
