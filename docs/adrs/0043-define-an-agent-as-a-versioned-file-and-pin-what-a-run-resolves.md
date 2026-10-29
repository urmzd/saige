# 0043. Define an agent as a versioned file and pin what a run resolves

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #91 (commit ee8f90f), recorded as D-43 in `DESIGN_DECISIONS.md`

## Decision

An agent definition is data: a Markdown file whose frontmatter declares the model, tools, skills, memory, sub-agents, approvals, compaction, guardrails and limits, and whose body is the system prompt. What a definition is, where it comes from, how a reference resolves and how it binds to runtime objects are separate packages, so every host (the CLI, `saige serve`, `saige-mcp`, a library caller) loads one the same way and differs only in what it binds it with.
Decoding is strict, with a path and a line for every problem, as for the catalog (D-37). A file with an unknown `apiVersion` is rejected rather than read as an older format. The JSON Schema is generated from the Go types and a test keeps the checked-in copy current.
A definition names everything outside itself (presets, MCP servers, registry tools, skills, memory stores) by reference, and only the host's environment supplies them. Loading never connects to a server or reads a credential; binding fails with the missing reference rather than reaching further.
A registry checks the whole set before it replaces what it holds: every reference resolves, there is no cycle, and a failed reload keeps the previous set. A resolution is pinned by a digest over the definition and every sub-agent it resolved to, and a reload never changes one already handed out, so a running agent keeps the definition it started with and its digest can be recorded with the run.
A definition that arrived with a repository is checked against an allowlist, as a project catalog layer is: nothing that connects to a server, reads memory, loosens an approval or runs code without asking.
Approval rules use the permission syntax of Claude Code, decided deny, then ask, then allow, then the tool's own marker, then its capability class. An allow rule matches a shell command only when it is one simple command, because a prefix rule must never approve what follows a `&&`. An approval block takes over its tools' markers, so the rules, not a marker, decide. Grants stay host-created (D-38); a definition can only cap their scope.
A sub-agent's own definition governs its own calls, behind the host's gate; a handoff member shares its entry agent's run and so cannot declare run policy of its own.
