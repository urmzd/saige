# 0013. Own catalog snapshots at the boundary

- **Status:** Accepted
- **Date:** 2026-09-20
- **Source:** PR #60 (commit b0790a3), recorded as D-13 in `DESIGN_DECISIONS.md`

## Decision

The catalog copies mutable metadata on registration and on return.
A caller can edit a lookup result without changing another session or a stored revision.
For example, changing a returned effort list does not modify the next lookup.
A deliberate change requires a new registration, which records a revision.
An exact model declaration sets `Known=true`. A family inference sets it to false.
This separates a declared model from an unverified variant that has a similar name.
