package types

import (
	"context"
	"slices"
)

// OutcomeKind names a result the agent loop can escalate on.
type OutcomeKind string

const (
	// OutcomeSchemaInvalid: structured output still failed its schema or
	// validator after every repair attempt.
	OutcomeSchemaInvalid OutcomeKind = "schema_invalid"
	// OutcomeSubagentFailed: a delegated sub-agent returned an error.
	OutcomeSubagentFailed OutcomeKind = "subagent_failed"
	// OutcomeEvalScore: an evaluator scored the answer below the host's bar.
	OutcomeEvalScore OutcomeKind = "eval_score"
	// OutcomeRefusal: the model declined to answer.
	OutcomeRefusal OutcomeKind = "refusal"
	// OutcomeContextLength: the request did not fit the model's window.
	OutcomeContextLength OutcomeKind = "context_length"
)

// Outcome describes one result an OutcomePolicy may answer with a model
// switch.
type Outcome struct {
	Kind OutcomeKind
	// Agent is the agent that observed the outcome.
	Agent string
	// Provider and Model identify the configuration that produced it.
	Provider string
	Model    string
	// SubAgent names the failed child for OutcomeSubagentFailed.
	SubAgent string
	// Attempts counts the tries made before the outcome, repairs included.
	Attempts int
	// Score is the evaluator score for OutcomeEvalScore.
	Score float64
	// Err is the failure that produced the outcome, when there is one.
	Err error
}

// Switch asks the agent to continue on another model, or with other dials.
// Model is passed to ConfigPart.Model, so for a routing session it names
// a profile ID; empty keeps the model. Dials are passed to
// ConfigPart.Dials, so a policy can raise reasoning depth on the same
// model before it moves to another one.
type Switch struct {
	Model  string
	Reason string
	Dials  *Dials
}

// OutcomePolicy decides whether an outcome moves the conversation to another
// model. A nil Switch keeps the current model. The agent records an accepted
// switch as ConfigPart in the tree, so it holds for later turns and
// survives a reload. Implementations should be deterministic: a durable run
// that replays the same outcome must reach the same decision.
type OutcomePolicy interface {
	Observe(ctx context.Context, o Outcome) (*Switch, error)
}

// OutcomePolicyFunc adapts a function to an OutcomePolicy.
type OutcomePolicyFunc func(ctx context.Context, o Outcome) (*Switch, error)

// Observe calls f.
func (f OutcomePolicyFunc) Observe(ctx context.Context, o Outcome) (*Switch, error) {
	return f(ctx, o)
}

// EscalationLadder moves to the next model in Models when one of Kinds is
// observed. It never moves down, and it stops at the last model. An empty
// Kinds escalates on every outcome. A current model that is not in the
// ladder escalates to the first entry.
type EscalationLadder struct {
	Models []string
	Kinds  []OutcomeKind
}

// Observe implements OutcomePolicy.
func (l EscalationLadder) Observe(_ context.Context, o Outcome) (*Switch, error) {
	if len(l.Models) == 0 || (len(l.Kinds) > 0 && !slices.Contains(l.Kinds, o.Kind)) {
		return nil, nil
	}
	next := slices.Index(l.Models, o.Model) + 1
	if next >= len(l.Models) {
		return nil, nil
	}
	return &Switch{Model: l.Models[next], Reason: string(o.Kind)}, nil
}
