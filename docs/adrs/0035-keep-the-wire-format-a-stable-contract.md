# 0035. Keep the wire format a stable contract

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-35 in `DESIGN_DECISIONS.md`

## Decision

Wire kinds and error codes are never renamed. A breaking change to an envelope raises `WireVersion`, and readers reject versions they do not know.
New fields are optional and omitted when empty, so an older reader still decodes a newer envelope of the same version.
A marker carries its interrupt on the wire, so a remote client can reply by interrupt ID and see the deadline.
A persisted tree carries its own format version, `tree.TreeFormatVersion`, under the same rule. A tree saved before the field existed reads as version 1.
