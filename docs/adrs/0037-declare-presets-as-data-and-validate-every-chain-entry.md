# 0037. Declare presets as data and validate every chain entry

- **Status:** Accepted
- **Date:** 2026-10-09
- **Source:** PR #64 (commit cc59a32), recorded as D-37 in `DESIGN_DECISIONS.md`

## Decision

A preset is an ordered chain of complete configurations in the catalog, not a model string with options copied across vendors. Each entry is resolved for its own model and built into its own adapter, so failover changes the configuration only to one someone wrote down.
Options resolve field by field, lowest first: the adapter's default, the model row's defaults, the preset's options (unless the entry opts out with `inherit: none`), the entry's options, then the entry's `unset`. A per-request override is merged last, by the adapter. One reasoning control wins whole.
Catalog layers merge model rows field by field, as JSON merge patches. Presets merge whole: an overlay preset replaces the base preset of the same name, and a variation is a new preset that extends it.
An option an entry cannot honor is rejected at load time with the path and the layer it came from, never stripped (D-12). The fix is always written in the file.
Dials resolve in the same layers and are compiled for each entry's own model at load time. A contractual dial the model cannot honor is an error with its path and layer; an advisory one that is mapped or dropped is a `dial_mapped` or `dial_dropped` warning. Every value a row declares for a dial must pass that row's validation and be expressible by its adapter, so a bad mapping fails at load time. The configuration hash covers the compiled mapping.
The embedded catalog is the single source of truth for model rows; a golden test freezes what it resolves. Hosts load further layers through a `Source`, and only `Install` or `Use` changes what `Lookup` returns.
Failover across vendors is opt-in. The CLI default runs one vendor, the first that can serve, and a preset names its chain when failover is wanted. Authentication and content-filter failures end the request unless the preset says otherwise.
A layer that arrives with a repository is checked against an allowlist, not a denylist, so a new field stays closed to it until someone decides.
