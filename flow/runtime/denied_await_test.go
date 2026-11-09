package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// gatedWorld is a world where "lead" sends batch, may delegate to ops only
// with approval and to docs-bot freely, with delegations as child runs. A
// child of ops asks a person which env, so one that started would wait.
func gatedWorld(t *testing.T, w *world, ttl time.Duration, batch []saigetypes.ToolCallPart) (*runtime.Runtime, *parking, *fanout) {
	t.Helper()
	p := newParking(t, w)
	w.specs.set(native.Spec{
		Name: "lead", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "Lead.",
		Access:    native.Access{Spawn: []string{"ops", "docs-bot"}},
		Approvals: []string{runtime.DelegateToolName("ops")},
	})
	lead := &fanout{batch: batch}
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{
		"lead":     lead,
		"ops":      routed{tool: native.AskPersonToolName, args: map[string]any{"question": "Which env?", "options": []any{"prod", "staging"}}, done: "Child chose"},
		"docs-bot": bystander(),
	}, runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
		return native.StaticTools(p.deploy), nil
	}))
	opts := []runtime.Option{runtime.WithChildRuns()}
	if ttl > 0 {
		opts = append(opts, runtime.WithQuestionTTL(ttl))
	}
	return p.rt(t, opts...), p, lead
}

// A delegation that needs approval starts no child run when its approval is
// denied, or expires into a denial: the run does not wait on it, and the
// call's result is the denial. In a batch, another delegation's child runs
// and answers as usual, and the run continues with both results.
func TestADeniedDelegationStartsNoChildRun(t *testing.T) {
	gated, free := runtime.DelegateToolName("ops"), runtime.DelegateToolName("docs-bot")
	for _, c := range []struct {
		name   string
		expire bool
		says   string
	}{
		{"denied", false, "not this one"},
		{"expired", true, "nobody answered"},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s batch=%v", c.name, batch), func(t *testing.T) {
				calls := []saigetypes.ToolCallPart{{ID: "g1", Name: gated, Arguments: map[string]any{"text": "deploy"}}}
				if batch {
					calls = append(calls, saigetypes.ToolCallPart{ID: "f1", Name: free, Arguments: map[string]any{"text": "document"}})
				}
				var ttl time.Duration
				if c.expire {
					ttl = 50 * time.Millisecond
				}
				w := newWorld(t)
				rt, p, lead := gatedWorld(t, w, ttl, calls)
				p.workers(t, rt, 4, 0)
				parent := p.accept(t, rt, tag("lead", "evt-1", "ship"))

				var approval event.Question
				if batch {
					qs := p.batchParked(t, parent, 2)
					if qs[0].Kind != event.QuestionApproval || qs[1].Kind != event.QuestionDelegation || qs[1].ChildRunID == "" {
						t.Fatalf("the parent waits on %+v, want the approval and the free delegation's child", qs)
					}
					approval = qs[0]
				} else {
					approval = pendingOf(t, rt, parent, event.QuestionApproval)
				}
				if approval.Tool == nil || approval.Tool.Name != gated {
					t.Fatalf("the approval is about %+v, want %s", approval.Tool, gated)
				}

				if c.expire {
					time.Sleep(80 * time.Millisecond)
					got, _ := p.answer(t, rt, parent, approval.ID, event.Answer{Decision: event.DecisionApprove})
					if !got.Expired || got.Decision != event.DecisionDeny {
						t.Fatalf("an answer after the deadline was recorded as %+v, want the denial it expired into", got)
					}
				} else {
					p.answer(t, rt, parent, approval.ID, event.Answer{Decision: event.DecisionDeny, Note: c.says})
				}

				out := p.result(t, rt, parent)
				results := strings.Split(strings.TrimPrefix(out.Text, "Lead got: "), " | ")
				if !strings.HasPrefix(out.Text, "Lead got: ") || !strings.Contains(results[0], c.says) {
					t.Fatalf("parent answered %q, want the denial as the gated call's result", out.Text)
				}
				if batch && (len(results) != 2 || results[1] != "Documented.") {
					t.Errorf("parent answered %q, want the free delegation's answer after the denial", out.Text)
				}
				if n := lead.calls.Load(); n != 2 {
					t.Errorf("the lead's model was called %d times, want 2", n)
				}

				// Nothing the denied call would have awaited was asked or
				// started: no wait on its result, and no child run but the
				// free delegation's.
				_, records, err := w.ledger.Load(context.Background(), parent)
				if err != nil {
					t.Fatal(err)
				}
				children := 0
				for _, rec := range records {
					if strings.HasPrefix(rec.Name, "fiber.child.") {
						children++
					}
					if !strings.HasPrefix(rec.Name, "fiber.question.") {
						continue
					}
					var q event.Question
					if json.Unmarshal(rec.Output, &q) == nil && q.Kind == event.QuestionDelegation && q.Tool != nil && q.Tool.Name == gated {
						t.Errorf("the run waited on the denied call's result: %+v", q)
					}
				}
				want := 0
				if batch {
					want = 1
				}
				if children != want {
					t.Errorf("the run started %d child runs, want %d", children, want)
				}
			})
		}
	}
}
