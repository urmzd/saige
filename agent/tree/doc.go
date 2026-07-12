// Package tree provides a persistent branching conversation graph with
// checkpoints, rewind, archive, compaction, feedback leaf nodes, and
// write-ahead-log durability. All mutation methods accept a context.Context
// for cancellation, deadlines, and tracing.
package tree
