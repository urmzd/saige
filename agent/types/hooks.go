package types

import "errors"

var (
	// ErrHookAborted is matched by the error of a run that a hook stopped.
	ErrHookAborted = errors.New("run aborted by a hook")
	// ErrGuardrailTripped is matched by the error of a run that a guardrail
	// blocked.
	ErrGuardrailTripped = errors.New("guardrail tripped")
)

// Guardrail phases.
const (
	GuardrailPhaseInput  = "input"
	GuardrailPhaseOutput = "output"
)

// Guardrail actions, as recorded and streamed.
const (
	GuardrailActionPass    = "pass"
	GuardrailActionBlock   = "block"
	GuardrailActionRewrite = "rewrite"
)

// GuardrailDelta reports a guardrail verdict that changed or ended the run:
// a block (tripwire) or a rewrite. A pass is not reported.
type GuardrailDelta struct {
	Guardrail string
	Phase     string // GuardrailPhaseInput or GuardrailPhaseOutput
	Action    string // GuardrailActionBlock or GuardrailActionRewrite
	Reason    string
	// Text is the replacement text of a rewrite. For an output rewrite it
	// replaces the answer the turn's text deltas streamed.
	Text string
	// Canceled is true when a parallel input guardrail stopped a model call
	// that was still in flight.
	Canceled bool
}

func (GuardrailDelta) isDelta() {}

// GuardrailPart records a guardrail verdict in the tree. A rewrite is
// attached to the message it rewrote; a block is recorded as a system
// message of its own. It is stripped before the provider call.
type GuardrailPart struct {
	Guardrail string `json:"guardrail"`
	Phase     string `json:"phase"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	// Canceled is true when a parallel input guardrail stopped a model call
	// in flight.
	Canceled bool `json:"canceled,omitempty"`
}

func (GuardrailPart) Kind() PartKind   { return KindGuardrail }
func (GuardrailPart) isPart()          {}
func (GuardrailPart) isSystemPart()    {}
func (GuardrailPart) isUserPart()      {}
func (GuardrailPart) isAssistantPart() {}

// HookRecord is the outcome of the hooks or guardrails at one point of a run.
// Under a durable StepRunner it is the result of a StepKindHook step, so a
// replay applies the same outcome without calling them again.
type HookRecord struct {
	// Abort is true when a hook stopped the run. Name names the hook or
	// guardrail and Reason says why.
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
	// Action is a guardrail's verdict: GuardrailActionPass, Block or Rewrite.
	Action string
	// Canceled is true when a parallel guardrail stopped a model call.
	Canceled bool
	// Usage and Receipts record model calls a guardrail made, such as a
	// classifier's, so a replay charges them again without calling it.
	Usage    []UsageDelta
	Receipts []BudgetReceipt
}
