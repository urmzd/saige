package types

import "errors"

// ErrHookAborted is matched by the error of a run that a hook stopped.
var ErrHookAborted = errors.New("run aborted by a hook")

// HookRecord is the outcome of the hooks at one point of a run. Under a
// durable StepRunner it is the result of a StepKindHook step, so a replay
// applies the same outcome without calling them again.
type HookRecord struct {
	// Abort is true when a hook stopped the run. Name names the hook and
	// Reason says why.
	Abort  bool
	Name   string
	Reason string
	// Changed is true when the hooks changed the event. The fields below then
	// hold the changed values for the event they belong to.
	Changed   bool
	Message   *UserMessage
	Arguments map[string]any
	Text      string
	Error     string
	Skip      bool
}
