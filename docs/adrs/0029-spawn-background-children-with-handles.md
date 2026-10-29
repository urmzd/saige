# 0029. Spawn background children with handles

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-29 in `DESIGN_DECISIONS.md`

## Decision

D-02 makes delegation call-and-return: the parent's tool call waits for the child. Spawn mode amends it for work the parent should not wait on.
`SubAgentDef.Mode: SubAgentSpawn` registers `spawn_<name>`. It starts the child under the run's context and returns a handle at once. The handle ID is the spawning tool call ID.
The child still returns to its caller. Its result is the `await_subagent` tool result when the parent asks for it. Otherwise it is appended as a user message at the parent's next safe point (D-25), in completion order, once per handle.
A run does not finish while a spawned child runs or its result is undelivered. It waits where it would finish and resumes with the result. A run that ends any other way, by an error, cancellation, or a stop tool, cancels its children before its stream closes.
Each spawn reserves budget under its handle ID before the child starts (D-09). The child's first provider call uses that reservation, and an unused one is released when the child ends. A busy budget refuses the spawn rather than queue it.
Spawn needs the inline runner. A durable runner cannot replay a child that outlives the step that started it, so the spawn tool returns `ErrSpawnUnsupported` there.
The run keeps each child's result. `search_subagent` and `read_subagent` read a transcript without loading it into the parent's context. Each child keeps its own tree (D-01).
Child output is data. The injected message wraps it in a tag that the output cannot close.
