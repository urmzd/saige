package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// NoIterLimit as MaxIter removes the iteration cap. An orchestrator can run
// without one while each of its sub-agents keeps a bounded budget.
const NoIterLimit = -1

// DefaultSubAgentMaxIter is the iteration cap of a sub-agent whose
// definition sets none and whose parent has no cap of its own to pass down.
const DefaultSubAgentMaxIter = 10

// DefaultWrapUpMargin is how many iterations a sub-agent has left when its
// wrap-up note arrives, unless SubAgentDef.WrapUpAt says otherwise.
const DefaultWrapUpMargin = 2

// DefaultWrapUpPrompt is the instruction in a wrap-up note.
const DefaultWrapUpPrompt = "Stop starting new work and return your result now. " +
	"Put everything your caller needs in your final message."

// WithWrapUpAt adds a wrap-up note after n model turns of a user turn (see
// Config.WrapUpAt). 0 turns it off.
func WithWrapUpAt(n int) Option {
	return func(c *Config) { c.WrapUpAt = n }
}

// childIterBudget returns a sub-agent's iteration cap and wrap-up point.
//
// The cap is the definition's MaxIter; when that is 0 it is the parent's cap,
// or DefaultSubAgentMaxIter when the parent has none, so a child is bounded
// by default even under an unbounded orchestrator. NoIterLimit in the
// definition gives an unbounded child.
//
// The wrap-up point is the definition's WrapUpAt; when that is 0 it is
// DefaultWrapUpMargin iterations before the cap, and at least 1. A negative
// WrapUpAt, an unbounded child, or a cap of 1 has none.
func childIterBudget(parentMaxIter int, sa SubAgentDef) (maxIter, wrapUpAt int) {
	maxIter = sa.MaxIter
	switch {
	case maxIter < 0:
		return NoIterLimit, 0
	case maxIter == 0 && parentMaxIter > 0:
		maxIter = parentMaxIter
	case maxIter == 0:
		maxIter = DefaultSubAgentMaxIter
	}
	switch {
	case sa.WrapUpAt < 0 || maxIter < 2:
		wrapUpAt = 0
	case sa.WrapUpAt > 0:
		wrapUpAt = sa.WrapUpAt
	default:
		wrapUpAt = max(maxIter-DefaultWrapUpMargin, 1)
	}
	return maxIter, wrapUpAt
}

// wrapUpDue reports whether a run that has made used model turns of a user
// turn capped at maxIter should get its wrap-up note now.
func wrapUpDue(wrapUpAt, used, maxIter int) bool {
	return wrapUpAt > 0 && maxIter > 0 && wrapUpAt < maxIter && used >= wrapUpAt && used < maxIter
}

// injectWrapUp appends the wrap-up note at a safe point and reports it as an
// InjectedDelta with Mode "wrap_up".
func (a *Agent) injectWrapUp(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, used, maxIter int, out runOutput) error {
	node, err := a.appendNode(ctx, tr, branch, types.SystemMsg(types.Text(wrapUpNote(a.cfg.WrapUpPrompt, used, maxIter, out))))
	if err != nil {
		return err
	}
	a.cfg.Logger.Debug("wrap-up note injected", "agent", a.cfg.Name, "used", used, "max_iter", maxIter)
	stream.send(types.InjectedDelta{Mode: "wrap_up", NodeID: string(node.ID)})
	return nil
}

// wrapUpNote is the text of a wrap-up note.
func wrapUpNote(prompt string, used, maxIter int, out runOutput) string {
	if prompt == "" {
		prompt = DefaultWrapUpPrompt
	}
	left := maxIter - used
	var b strings.Builder
	b.WriteString("<wrap_up>\n")
	fmt.Fprintf(&b, "You have used %d of %d iterations; %d remain%s. ", used, maxIter, left, plural(left, "", "s"))
	b.WriteString(prompt)
	switch {
	case out.tool():
		fmt.Fprintf(&b, " Call %s with your result now.", FinalAnswerToolName)
	case out.schema != nil:
		b.WriteString(" Reply now with your answer as JSON that matches the response schema.")
	}
	b.WriteString(" At the limit your tools are removed and you must answer with what you have.\n</wrap_up>")
	return b.String()
}

// plural returns one when n is 1 and many otherwise.
func plural(n int, many, one string) string {
	if n == 1 {
		return one
	}
	return many
}
