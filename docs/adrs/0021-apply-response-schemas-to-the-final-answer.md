# 0021. Apply response schemas to the final answer

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-21 in `DESIGN_DECISIONS.md`

## Decision

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
