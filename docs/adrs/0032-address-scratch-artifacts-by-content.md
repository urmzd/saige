# 0032. Address scratch artifacts by content

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-32 in `DESIGN_DECISIONS.md`

## Decision

A workspace stores artifacts by SHA-256 digest, under a name that points at the latest content. Writing the same bytes again returns the same reference, so a replayed durable step writes nothing new (D-14).
The directory backend writes every file to a temporary name, syncs it, and renames it into place, so a crash never leaves a partial artifact.
`workspace.Spill` replaces a tool result over its threshold with a preview and a `saige-artifact://` URI that `scratch_read` pages through. Nothing is dropped from context without a way back (D-22). When the store refuses the write, the full result is returned. Approval markers stay outermost when a marked tool is wrapped.
The loop attaches its workspace to each tool call. A sub-agent receives a read-only view, so it can read what its caller saved but cannot change it.
