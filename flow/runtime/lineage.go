package runtime

import (
	"context"
	"fmt"
)

// Lineage is a run's place in the tree of runs that answers one request.
// The root run answers; every run it calls, at any depth, is a tool call of
// its caller. Every event of a run is stamped with it (event.Envelope.Root,
// ParentRun and ParentToolCall), so a subscriber that follows one response
// subscribes by Root and nests each run under the call that started it.
//
// The zero Lineage is a root run's: Root is then the run itself. A called run
// names all three, or a root and a parent with no call when the call is not
// known. It is fixed when the run is accepted and kept in its durable input,
// so every attempt, replay and resume stamps the same lineage.
//
// What starts a run sets it:
//
//   - a host passes it with WithLineage on the context of the call that
//     accepts the run;
//   - AcceptOwnChild, AcceptSessionChild and AcceptOwnSessionChild take Root
//     and ParentRun from the durable parent they verify, so a host passes
//     only the call (or a lineage that agrees with that parent);
//   - a delegation this runtime starts as a child run (WithChildRuns, the
//     session_call tool) is called through the parent's tool call, by the id
//     that call carries on the parent's run.delta.tool.call events.
type Lineage struct {
	// Root is the run the tree answers from. Empty means the run itself.
	Root string `json:"root,omitempty"`
	// ParentRun is the run that called this one.
	ParentRun string `json:"parent_run,omitempty"`
	// ParentToolCall is the id of the tool call ParentRun called this run
	// through, as whoever started the run minted it.
	ParentToolCall string `json:"parent_tool_call,omitempty"`
}

// IsZero reports whether l is a root run's lineage.
func (l Lineage) IsZero() bool { return l == Lineage{} }

// Child is the lineage of a run that run, whose own lineage is l, calls
// through the tool call call: the same root (run itself when l is a root
// run's), run as the parent, and call. A host that starts a child run from a
// turn computes it from the turn's Request:
//
//	ctx = runtime.WithLineage(ctx, req.Lineage.Child(req.RunID, callID))
func (l Lineage) Child(run, call string) Lineage {
	root := l.Root
	if root == "" {
		root = run
	}
	return Lineage{Root: root, ParentRun: run, ParentToolCall: call}
}

// of is l as the run runID stamps it: with its root, which is runID for a run
// that no run called.
func (l Lineage) of(runID string) Lineage {
	if l.Root == "" {
		l.Root = runID
	}
	return l
}

// validate checks l as the lineage of the run runID. A call needs the run
// that made it, and a called run needs the root of its tree; a run that no
// run called is its own root, and no run calls itself or roots the tree of
// its caller. A lineage that breaks one of these would nest the run's events
// under nothing, or under itself, in every subscriber that follows the tree.
func (l Lineage) validate(runID string) error {
	switch {
	case l.ParentToolCall != "" && l.ParentRun == "":
		return fmt.Errorf("%w: lineage names tool call %q but not the run that made it", ErrInvalid, l.ParentToolCall)
	case l.ParentRun != "" && l.Root == "":
		return fmt.Errorf("%w: lineage names parent run %q but no root", ErrInvalid, l.ParentRun)
	case l.ParentRun == "" && l.Root != "" && l.Root != runID:
		return fmt.Errorf("%w: lineage roots run %s at %q but names no parent: a run no run called is its own root", ErrInvalid, runID, l.Root)
	case l.ParentRun != "" && l.ParentRun == runID:
		return fmt.Errorf("%w: lineage makes run %s its own parent", ErrInvalid, runID)
	case l.ParentRun != "" && l.Root == runID:
		return fmt.Errorf("%w: lineage makes the called run %s the root of its own tree", ErrInvalid, runID)
	}
	return nil
}

// childLineage is the lineage of a run that caller, the durable run
// callerRun, starts: caller's root and callerRun, with the call the host
// passed. A host lineage that names another parent or another root is
// refused rather than corrected: it is a host that disagrees with the ledger
// about where the run belongs.
func childLineage(host Lineage, caller Input, callerRun string) (Lineage, error) {
	want := caller.Lineage.Child(callerRun, host.ParentToolCall)
	switch {
	case host.ParentRun != "" && host.ParentRun != callerRun:
		return Lineage{}, fmt.Errorf("%w: lineage names parent run %q, but the run is called by %s", ErrInvalid, host.ParentRun, callerRun)
	case host.Root != "" && host.Root != want.Root:
		return Lineage{}, fmt.Errorf("%w: lineage names root %q, but the caller %s answers from %s", ErrInvalid, host.Root, callerRun, want.Root)
	}
	return want, nil
}

type lineageKey struct{}

// WithLineage returns ctx carrying l as the lineage of the run accepted with
// it: Accept, AcceptOwn, AcceptSession, AcceptOwnSession, AcceptOwnVersion
// and the child admissions read it. It is the lineage of the run being
// started, not of the run the caller is in: a turn reads its own from
// Request.Lineage. A lineage that breaks Lineage's rules fails the accept
// with ErrInvalid.
//
// The runtime passes a ChildStarter a context carrying the lineage of the
// child it asks for, so a host that admits the child with that context
// stamps it without doing anything; one that mints its own call ids wraps
// the context again.
func WithLineage(ctx context.Context, l Lineage) context.Context {
	return context.WithValue(ctx, lineageKey{}, l)
}

// LineageFrom returns the lineage ctx carries for the run accepted with it,
// and the zero Lineage when it carries none.
func LineageFrom(ctx context.Context) Lineage {
	l, _ := ctx.Value(lineageKey{}).(Lineage)
	return l
}
