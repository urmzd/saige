// Package durable makes existing Go code durable without rewrites.
//
// Three concepts, nothing else:
//
//   - ledger — durable truth: runs and their memoized records, stored in
//     tables the caller already owns (pkg/ledger; adapters map, never migrate)
//   - queue — flow and clock: delivery of run IDs under fenced, time-bounded
//     claims (pkg/queue); disposable, rebuildable from the ledger
//   - worker — a pull loop, not a service (pkg/worker); it can be the
//     submitting process itself or something else entirely
//
// Wrap a function with Activity, wrap non-determinism with Step, run a
// worker. On crash or retry the workflow re-executes from the top: calls
// with recorded results return them instantly, the first unrecorded call
// executes for real. Runs replay any number of times, or error out.
//
// Outside a run every wrapped function is a plain Go call — code keeps
// working unchanged in tests, scripts, and codebases mid-migration.
package durable

// Void is the output type for effect-only activities: the (input, output)
// shape stays uniform, effect-only activities return Void{}.
type Void = struct{}
