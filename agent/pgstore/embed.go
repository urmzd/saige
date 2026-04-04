package pgstore

import _ "embed"

// ── Node queries ───────────────────────────────────────────────────

//go:embed sql/node_upsert.sql
var nodeUpsertSQL string

//go:embed sql/node_get.sql
var nodeGetSQL string

//go:embed sql/node_children.sql
var nodeChildrenSQL string

//go:embed sql/node_path.sql
var nodePathSQL string

//go:embed sql/node_tree.sql
var nodeTreeSQL string

// ── Branch queries ─────────────────────────────────────────────────

//go:embed sql/branch_upsert.sql
var branchUpsertSQL string

//go:embed sql/branch_get.sql
var branchGetSQL string

//go:embed sql/branch_list.sql
var branchListSQL string

// ── Checkpoint queries ─────────────────────────────────────────────

//go:embed sql/checkpoint_upsert.sql
var checkpointUpsertSQL string

//go:embed sql/checkpoint_get.sql
var checkpointGetSQL string
