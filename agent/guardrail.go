package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// GuardrailVerdict is what a guardrail decides about one text.
type GuardrailVerdict struct {
	// Action is types.GuardrailActionPass, Block or Rewrite. Empty passes.
	Action string
	// Reason says why. It is recorded and reported with a block or rewrite.
	Reason string
	// Text replaces the checked text for a rewrite.
	Text string
}

// Pass lets the text through unchanged.
func Pass() GuardrailVerdict { return GuardrailVerdict{Action: types.GuardrailActionPass} }

// Block trips the guardrail: the run stops with a *GuardrailTrippedError.
func Block(reason string) GuardrailVerdict {
	return GuardrailVerdict{Action: types.GuardrailActionBlock, Reason: reason}
}

// Rewrite replaces the checked text, for example to redact it, and lets the
// run go on.
func Rewrite(text, reason string) GuardrailVerdict {
	return GuardrailVerdict{Action: types.GuardrailActionRewrite, Reason: reason, Text: text}
}

// GuardrailInput is the text a guardrail checks and where it comes from.
type GuardrailInput struct {
	HookRun
	// Phase is types.GuardrailPhaseInput or types.GuardrailPhaseOutput.
	Phase string
	// Text is the text of the user message, or of the final answer.
	Text string
	// History is the branch before the checked message, read-only.
	History []types.Message

	meter func(types.Provider) types.Provider
}

// Metered returns p wrapped so each call it makes is admitted by the run's
// budget before it is sent, charged after it, and reported as a UsageDelta.
// A guardrail that calls a model, such as a classifier, sends its calls
// through it. Without a budget the calls are still reported.
func (in GuardrailInput) Metered(p types.Provider) types.Provider {
	if in.meter == nil {
		return p
	}
	return in.meter(p)
}

// Guardrail checks one text: a user message on the way in, or the final
// answer on the way out. An error fails closed: the run stops as if the
// guardrail blocked. Check is bounded by the hook timeout and must be safe
// for concurrent use.
type Guardrail interface {
	Name() string
	Check(ctx context.Context, in GuardrailInput) (GuardrailVerdict, error)
}

// NewGuardrail adapts a function into a Guardrail.
func NewGuardrail(name string, check func(context.Context, GuardrailInput) (GuardrailVerdict, error)) Guardrail {
	return funcGuardrail{name: name, check: check}
}

type funcGuardrail struct {
	name  string
	check func(context.Context, GuardrailInput) (GuardrailVerdict, error)
}

func (g funcGuardrail) Name() string { return g.name }

func (g funcGuardrail) Check(ctx context.Context, in GuardrailInput) (GuardrailVerdict, error) {
	return g.check(ctx, in)
}

// GuardrailMode says when an input guardrail runs.
type GuardrailMode int

const (
	// GuardrailSequential checks the message before it is appended, so the
	// model never sees text the guardrail blocks or rewrites.
	GuardrailSequential GuardrailMode = iota
	// GuardrailParallel starts the run's first model call at the same time
	// and cancels it if the guardrail blocks. It saves the guardrail's
	// latency but cannot rewrite: the model already has the text, so a
	// rewrite counts as a block. Messages submitted during a run, and runs
	// under a durable runner that keeps steps on one goroutine, are checked
	// sequentially instead.
	GuardrailParallel
)

// InputGuardrail validates each user message that enters a run: its input
// and every message submitted while it runs.
type InputGuardrail struct {
	Guardrail
	Mode GuardrailMode
}

// OutputGuardrail validates the final answer before it is recorded: a turn
// without tool calls that ends the user turn, a forced final answer, or a
// text turn the output limit cut short. The answer's deltas have already
// streamed; a rewrite is reported with a GuardrailDelta that carries the
// replacement, and the recorded answer is the rewritten one. An answer
// delivered by a stop tool is a tool result; check it with an AfterTool
// hook.
type OutputGuardrail struct {
	Guardrail
}

// WithInputGuardrails adds input guardrails. They run in order, after the
// UserInput hooks; the first block stops the run.
func WithInputGuardrails(gs ...InputGuardrail) AgentOption {
	return func(c *AgentConfig) { c.InputGuardrails = append(c.InputGuardrails, gs...) }
}

// WithOutputGuardrails adds output guardrails. They run in order; each sees
// the previous one's rewrite, and the first block stops the run.
func WithOutputGuardrails(gs ...OutputGuardrail) AgentOption {
	return func(c *AgentConfig) { c.OutputGuardrails = append(c.OutputGuardrails, gs...) }
}

// ErrGuardrailTripped is matched by the error of a run a guardrail blocked.
var ErrGuardrailTripped = types.ErrGuardrailTripped

// GuardrailTrippedError reports a run a guardrail blocked. It matches
// ErrGuardrailTripped.
type GuardrailTrippedError struct {
	Guardrail string
	Phase     string
	Reason    string
	// Canceled is true when a parallel guardrail stopped a model call in
	// flight.
	Canceled bool
}

func (e *GuardrailTrippedError) Error() string {
	return fmt.Sprintf("%s guardrail %s tripped: %s", e.Phase, e.Guardrail, e.Reason)
}

// Is matches ErrGuardrailTripped.
func (e *GuardrailTrippedError) Is(target error) bool { return target == types.ErrGuardrailTripped }

// ── Runtime ──────────────────────────────────────────────────────────

// Guardrail step keys. They number the guardrail points of a run like the
// hook points the loop meets in order.
const (
	guardInputStep  HookEvent = "guardrail_input"
	guardOutputStep HookEvent = "guardrail_output"
)

// errGuardrailCanceled stops the model call a parallel guardrail tripped on.
// It matches errInterruptRequested, so the call returns its partial turn and
// estimated usage like a call a submission interrupted.
var errGuardrailCanceled = fmt.Errorf("%w: input guardrail tripped", errInterruptRequested)

// checkGuardrails runs gs in order on text and returns their combined
// verdict as a record, with the usage of any model calls they made. A
// parallel check cannot rewrite, so a rewrite becomes a block.
func (a *Agent) checkGuardrails(ctx context.Context, stream *EventStream, phase string, gs []Guardrail, text string, history []types.Message, step string, parallel bool) (rec types.HookRecord) {
	rec.Action = types.GuardrailActionPass
	var (
		mu     sync.Mutex
		meters []*meteredProvider
	)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range meters {
			calls, _ := m.result()
			for _, u := range calls {
				rec.Usage = append(rec.Usage, u)
				if a.cfg.Budget != nil && u.AccountingID != "" {
					if r := a.cfg.Budget.Receipt(u.AccountingID); r.ID != "" {
						rec.Receipts = append(rec.Receipts, r)
					}
				}
			}
		}
	}()
	in := GuardrailInput{HookRun: a.hookRun(ctx, stream), Phase: phase, History: history}
	for _, g := range gs {
		name := g.Name()
		in.Text = text
		in.meter = func(p types.Provider) types.Provider {
			mu.Lock()
			defer mu.Unlock()
			m := &meteredProvider{Provider: p, agent: a, stream: stream, step: fmt.Sprintf("%s-%s-%d", step, name, len(meters))}
			meters = append(meters, m)
			return m
		}
		var v GuardrailVerdict
		err := a.callHook(ctx, "guardrail "+name, func(ctx context.Context) error {
			var err error
			v, err = g.Check(ctx, in)
			return err
		})
		switch {
		case err != nil:
			v = Block("guardrail failed: " + err.Error())
		case v.Action == "":
			v.Action = types.GuardrailActionPass
		case v.Action == types.GuardrailActionRewrite && parallel:
			v = Block("a parallel guardrail cannot rewrite text the model already received: " + v.Reason)
		}
		switch v.Action {
		case types.GuardrailActionPass:
		case types.GuardrailActionRewrite:
			text = v.Text
			rec.Action, rec.Name, rec.Reason, rec.Changed, rec.Text = v.Action, name, v.Reason, true, text
		case types.GuardrailActionBlock:
			rec.Action, rec.Name, rec.Reason = v.Action, name, v.Reason
			rec.Changed, rec.Text = false, ""
			return rec
		default:
			rec.Action, rec.Name, rec.Reason = types.GuardrailActionBlock, name, fmt.Sprintf("unknown guardrail action %q", v.Action)
			rec.Changed, rec.Text = false, ""
			return rec
		}
	}
	return rec
}

// chargeGuardrail reports the model calls a guardrail record holds and
// charges them to the budget. A replayed record restores its receipts
// first, so the calls are charged once without being made again.
func (a *Agent) chargeGuardrail(ctx context.Context, stream *EventStream, rec types.HookRecord, ran bool) error {
	if !ran && a.cfg.Budget != nil {
		for _, r := range rec.Receipts {
			if err := a.cfg.Budget.Restore(r); err != nil {
				return err
			}
		}
	}
	for i := range rec.Usage {
		if err := a.reportUsage(ctx, stream, a.cfg.Provider, &rec.Usage[i]); err != nil {
			return err
		}
	}
	return nil
}

// tripGuardrail reports and records a block and returns the run's error.
func (a *Agent) tripGuardrail(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, phase string, rec types.HookRecord) error {
	stream.send(types.GuardrailDelta{Guardrail: rec.Name, Phase: phase, Action: types.GuardrailActionBlock, Reason: rec.Reason, Canceled: rec.Canceled})
	record := types.SystemMessage{Parts: []types.SystemPart{types.GuardrailPart{
		Guardrail: rec.Name, Phase: phase, Action: types.GuardrailActionBlock, Reason: rec.Reason, Canceled: rec.Canceled,
	}}}
	tripped := &GuardrailTrippedError{Guardrail: rec.Name, Phase: phase, Reason: rec.Reason, Canceled: rec.Canceled}
	if err := a.appendToBranch(ctx, tr, branch, record); err != nil {
		a.cfg.Logger.Warn("recording a guardrail block failed", "agent", a.cfg.Name, "guardrail", rec.Name, "error", err)
	}
	return tripped
}

// concurrentGuardrails reports whether a parallel guardrail can run beside
// the model call: inline, or under a runner that takes concurrent steps.
func (a *Agent) concurrentGuardrails() bool {
	if _, ok := a.cfg.StepRunner.(types.NoopStepRunner); ok {
		return true
	}
	c, ok := a.cfg.StepRunner.(types.ConcurrentStepRunner)
	return ok && c.ConcurrentSteps()
}

// inputGuardrails returns the guardrails that check a message before it is
// appended. withParallel includes the parallel ones.
func (a *Agent) inputGuardrails(withParallel bool) []Guardrail {
	var gs []Guardrail
	for _, g := range a.cfg.InputGuardrails {
		if g.Guardrail != nil && (g.Mode == GuardrailSequential || withParallel) {
			gs = append(gs, g.Guardrail)
		}
	}
	return gs
}

// admitUserMessage passes a user message through the UserInput hooks and
// the input guardrails that run before it is appended, and returns the
// message to append. Parallel guardrails run here too unless the message is
// the run's input and they can run beside the first model call.
func (a *Agent) admitUserMessage(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, msg types.UserMessage, source string) (types.UserMessage, error) {
	msg, err := a.userInputHooks(ctx, stream, msg, source)
	if err != nil {
		return msg, err
	}
	gs := a.inputGuardrails(source != "input" || !a.concurrentGuardrails())
	if len(gs) == 0 {
		return msg, nil
	}
	history, err := tr.FlattenBranch(branch)
	if err != nil {
		return msg, err
	}
	step := stream.hookStep(guardInputStep)
	rec, ran, err := a.recordHook(ctx, step, func(ctx context.Context) types.HookRecord {
		return a.checkGuardrails(ctx, stream, types.GuardrailPhaseInput, gs, userText(msg), history, step, false)
	})
	if err != nil {
		return msg, err
	}
	if err := a.chargeGuardrail(ctx, stream, rec, ran); err != nil {
		return msg, err
	}
	switch rec.Action {
	case types.GuardrailActionBlock:
		return msg, a.tripGuardrail(ctx, stream, tr, branch, types.GuardrailPhaseInput, rec)
	case types.GuardrailActionRewrite:
		msg = types.UserMessage{Parts: append(rewriteText(msg.Parts, rec.Text), types.UserPart(types.GuardrailPart{
			Guardrail: rec.Name, Phase: types.GuardrailPhaseInput, Action: rec.Action, Reason: rec.Reason,
		}))}
		stream.send(types.GuardrailDelta{Guardrail: rec.Name, Phase: types.GuardrailPhaseInput, Action: rec.Action, Reason: rec.Reason, Text: rec.Text})
	}
	return msg, nil
}

// guardOutput runs the output guardrails on a final answer before it is
// recorded. A rewrite changes msg in place; a block stops the run and the
// answer is not recorded.
func (a *Agent) guardOutput(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, msg *types.AssistantMessage, llmStep string) error {
	if len(a.cfg.OutputGuardrails) == 0 || msg == nil {
		return nil
	}
	text := contentText(msg.Parts)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	gs := make([]Guardrail, 0, len(a.cfg.OutputGuardrails))
	for _, g := range a.cfg.OutputGuardrails {
		if g.Guardrail != nil {
			gs = append(gs, g.Guardrail)
		}
	}
	history, err := tr.FlattenBranch(branch)
	if err != nil {
		return err
	}
	step := "hook-" + string(guardOutputStep) + "-" + llmStep
	rec, ran, err := a.recordHook(ctx, step, func(ctx context.Context) types.HookRecord {
		return a.checkGuardrails(ctx, stream, types.GuardrailPhaseOutput, gs, text, history, step, false)
	})
	if err != nil {
		return err
	}
	if err := a.chargeGuardrail(ctx, stream, rec, ran); err != nil {
		return err
	}
	switch rec.Action {
	case types.GuardrailActionBlock:
		return a.tripGuardrail(ctx, stream, tr, branch, types.GuardrailPhaseOutput, rec)
	case types.GuardrailActionRewrite:
		msg.Parts = append(rewriteText(msg.Parts, rec.Text), types.AssistantPart(types.GuardrailPart{
			Guardrail: rec.Name, Phase: types.GuardrailPhaseOutput, Action: rec.Action, Reason: rec.Reason,
		}))
		stream.send(types.GuardrailDelta{Guardrail: rec.Name, Phase: types.GuardrailPhaseOutput, Action: rec.Action, Reason: rec.Reason, Text: rec.Text})
	}
	return nil
}

// guardTruncated runs the output guardrails on a text turn the output limit
// cut short when it ends the user turn, that is when no automatic
// continuation follows.
func (a *Agent) guardTruncated(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, msg *types.AssistantMessage, usage *types.UsageDelta, continues int, llmStep string) error {
	if msg == nil || truncationReason(usage) == "" || continues < a.cfg.AutoContinue || len(assistantToolCalls(msg)) > 0 {
		return nil
	}
	return a.guardOutput(ctx, stream, tr, branch, msg, llmStep)
}

// parallelGuard runs the parallel input guardrails beside the run's first
// model call.
type parallelGuard struct {
	done   chan struct{}
	cancel context.CancelFunc
	rec    types.HookRecord
	ran    bool
	err    error

	mu         sync.Mutex
	tripped    bool
	cancelTurn context.CancelCauseFunc
}

// startParallelGuard starts the parallel input guardrails on text, the run's
// admitted input. It returns nil when there are none to run beside the call.
func (a *Agent) startParallelGuard(ctx context.Context, stream *EventStream, history []types.Message, text string) *parallelGuard {
	if !a.concurrentGuardrails() || strings.TrimSpace(text) == "" {
		return nil
	}
	var gs []Guardrail
	for _, g := range a.cfg.InputGuardrails {
		if g.Guardrail != nil && g.Mode == GuardrailParallel {
			gs = append(gs, g.Guardrail)
		}
	}
	if len(gs) == 0 {
		return nil
	}
	gctx, cancel := context.WithCancel(ctx)
	g := &parallelGuard{done: make(chan struct{}), cancel: cancel}
	step := stream.hookStep(guardInputStep)
	go func() {
		defer close(g.done)
		g.rec, g.ran, g.err = a.recordHook(gctx, step, func(ctx context.Context) types.HookRecord {
			rec := a.checkGuardrails(ctx, stream, types.GuardrailPhaseInput, gs, text, history, step, true)
			if rec.Action == types.GuardrailActionBlock {
				rec.Canceled = g.trip()
			}
			return rec
		})
		if g.err == nil && g.rec.Action == types.GuardrailActionBlock {
			g.trip() // a replayed block still stops the call
		}
	}()
	return g
}

// trip cancels the model call in flight, if any, and reports whether there
// was one.
func (g *parallelGuard) trip() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tripped = true
	if g.cancelTurn == nil {
		return false
	}
	g.cancelTurn(errGuardrailCanceled)
	return true
}

// attach registers the cancel function of the model call about to start. It
// reports whether the guardrail already tripped, in which case the call is
// not made.
func (g *parallelGuard) attach(cancel context.CancelCauseFunc) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tripped {
		return true
	}
	g.cancelTurn = cancel
	return false
}

// wait detaches the model call and waits for the guardrails' verdict.
func (g *parallelGuard) wait() {
	g.mu.Lock()
	g.cancelTurn = nil
	g.mu.Unlock()
	<-g.done
}

// stop ends guardrails the run no longer waits for.
func (g *parallelGuard) stop() {
	if g == nil {
		return
	}
	g.cancel()
	<-g.done
}

// finishParallelGuard waits for the parallel guardrails after the first
// model call. On a block the call's turn is discarded, its usage is still
// charged, and the run stops; usage is the call's usage, nil when it was
// not made.
func (a *Agent) finishParallelGuard(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, g *parallelGuard, provider types.Provider, usage *types.UsageDelta) error {
	g.wait()
	if g.err != nil {
		return g.err
	}
	if err := a.chargeGuardrail(ctx, stream, g.rec, g.ran); err != nil {
		return err
	}
	if g.rec.Action != types.GuardrailActionBlock {
		return nil
	}
	if usage != nil {
		if err := a.reportUsage(ctx, stream, provider, usage); err != nil {
			return err
		}
	}
	return a.tripGuardrail(ctx, stream, tr, branch, types.GuardrailPhaseInput, g.rec)
}

// ── Text helpers ─────────────────────────────────────────────────────

func contentText(content []types.AssistantPart) string {
	var b strings.Builder
	for _, c := range content {
		if t, ok := c.(types.TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// rewriteText replaces the text blocks of content with one block holding
// text, at the place of the first. Other blocks, such as files or signed
// thinking, are kept as they are.
func rewriteText[C any](content []C, text string) []C {
	out := make([]C, 0, len(content)+1)
	placed := false
	for _, c := range content {
		if _, ok := any(c).(types.TextPart); ok {
			if !placed {
				out = append(out, any(types.TextPart{Text: text}).(C))
				placed = true
			}
			continue
		}
		out = append(out, c)
	}
	if !placed {
		out = append(out, any(types.TextPart{Text: text}).(C))
	}
	return out
}
