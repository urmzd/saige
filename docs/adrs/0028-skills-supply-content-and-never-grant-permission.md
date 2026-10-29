# 0028. Skills supply content and never grant permission

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-28 in `DESIGN_DECISIONS.md`

## Decision

A skill is an instruction package. Its text is guidance, not authority.
Sources are read in order and the first definition of a name wins, except that an untrusted definition never shadows a trusted one. Otherwise an untrusted project checkout could replace a trusted skill that an allow list admits by name. Each package is copied into an immutable snapshot with a hash per file and one over all paths and hashes, within bounds of 200 files, 5 MiB per file, and 20 MiB in total. A package over a bound is rejected whole.
A directory source reads through `os.Root`, so a link that leaves the root rejects the package. Resource reads use only manifest paths, are re-hashed before they are returned, and never touch the disk.
The system prompt lists names and descriptions. `load_skill` returns the instructions as a tool result, never as system content. The wrapper tag ends in a digest of the body, so the body cannot close it early.
`allowed-tools` only narrows: the effective tools are the agent's tools intersected with the list. It is never a pre-approval, and argument patterns match no tool. The inner tool policy chooses only among the allowed tools, and `tool_search` stays visible, so an allowed tool that is still deferred can be found. The active skill is kept per conversation, like tool discovery.
`AllowList` admits all trusted skills, none, or named ones. Its zero value admits none. Skills from an untrusted source are reachable only by name. A sub-agent with a parent in `AllowListPolicy` reaches only what its parent reaches.
The skill tools never run a script. Execution needs a separately registered tool behind its own gates.
A hidden skill and a missing one return the same error, so the model cannot probe for skills it was not shown.
