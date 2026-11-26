package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable"
	ragtypes "github.com/urmzd/saige/rag/types"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node/inproc"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/thread"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// numbered is a retriever that hands each retrieval a passage of its own,
// numbered in the order retrievals happen, so a test can tell which turn a
// passage was retrieved for.
type numbered struct {
	mu sync.Mutex
	n  int
}

func passage(n int) string { return fmt.Sprintf("PASSAGE-%d: retrieved for one turn", n) }

func (p *numbered) Retrieve(_ context.Context, _ string) (*ragtypes.AssembledContext, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	text := passage(n)
	return &ragtypes.AssembledContext{
		Prompt: "Context:\n\n[1] " + text,
		Blocks: []ragtypes.ContextBlock{{Text: text, Citation: "[1]"}},
	}, nil
}

// grounded is a turn runtime whose agent retrieves from knowledge and answers
// with model.
type grounded func(t *testing.T, sp *specs, knowledge *numbered, model saigetypes.Provider) turnrt.Runtime

func nativeGrounded(_ *testing.T, _ *specs, knowledge *numbered, model saigetypes.Provider) turnrt.Runtime {
	return &native.Runtime{
		Options: []native.Option{
			native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }),
		},
		Knowledge: func(string, []string) native.Retriever { return knowledge },
	}
}

func sandboxGrounded(t *testing.T, sp *specs, knowledge *numbered, model saigetypes.Provider) turnrt.Runtime {
	t.Helper()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools := tool.NewRegistry()
	plane := controlplane.NewMemory(inproc.NewFactory(tools, ws), metrics.Nop())
	s, err := runtime.NewSandbox(plane, tools, sp,
		runtime.WithAgentOptions(
			native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return model, nil }),
		),
		runtime.WithKnowledge(func(string, []string) native.Retriever { return knowledge }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The journal keeps each request as it was asked. The passages retrieved for
// a turn are sent to the model with that turn's request and are not
// journaled, so a later turn in the thread is shown the earlier request and
// answer, and the passages retrieved for its own request, and nothing
// retrieved for the earlier one.
func TestTheJournalKeepsTheRequestAsAskedAndEachTurnRetrievesItsOwnPassages(t *testing.T) {
	for name, build := range map[string]grounded{
		"native":  nativeGrounded,
		"sandbox": sandboxGrounded,
	} {
		t.Run(name, func(t *testing.T) {
			sp := newSpecs()
			sp.revise("docs-bot", "You answer from the runbook.")
			sp.edit("docs-bot", func(s *native.Spec) {
				s.Sources = append(s.Sources, native.Source{Name: "runbook", URI: "kb://runbook"})
			})
			knowledge := &numbered{}
			model := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
				agenttest.TextResponse("Deploys run on merge."),
				agenttest.TextResponse("Rollbacks are manual."),
			}}

			w := newWorld(t)
			w.specs = sp
			rt, err := runtime.New(durable.New(w.ledger, w.queue), sp, w.bus,
				runtime.WithRuntime(build(t, sp, knowledge, model)), runtime.WithThreads(w.threads))
			if err != nil {
				t.Fatal(err)
			}
			w.work(t, rt, nil)

			first := tag("docs-bot", "Ev-asked-1", "how do deploys run?")
			first.Actor = event.Actor{ID: "U1", Display: "ada"}
			w.result(t, rt, w.accept(t, rt, first))
			second := tag("docs-bot", "Ev-asked-2", "and rollbacks?")
			second.Actor = event.Actor{ID: "U2", Display: "grace"}
			w.result(t, rt, w.accept(t, rt, second))

			entries := journal(t, w.threads, first.key())
			if len(entries) != 4 {
				t.Fatalf("journal = %q, want two exchanges", said(entries))
			}
			for i, e := range entries {
				if strings.Contains(e.Text, "PASSAGE-") {
					t.Errorf("journal entry %d holds a retrieved passage: %q", i, e.Text)
				}
			}
			if e := entries[0]; e.Role != thread.RoleUser || !strings.Contains(e.Text, "how do deploys run?") {
				t.Errorf("the first request was journaled as %+v", e)
			}
			if e := entries[2]; e.Role != thread.RoleUser || !strings.Contains(e.Text, "and rollbacks?") {
				t.Errorf("the second request was journaled as %+v", e)
			}

			asked := model.Requests()
			if len(asked) != 2 {
				t.Fatalf("the model was called %d times, want once per turn", len(asked))
			}
			one := encode(t, asked[0].Messages).String()
			if !strings.Contains(one, passage(1)) {
				t.Errorf("the first turn was not sent its own passage:\n%s", one)
			}
			two := encode(t, asked[1].Messages).String()
			for _, want := range []string{"how do deploys run?", "Deploys run on merge.", "and rollbacks?", passage(2)} {
				if !strings.Contains(two, want) {
					t.Errorf("the second turn was not sent %q:\n%s", want, two)
				}
			}
			if strings.Contains(two, passage(1)) {
				t.Errorf("the second turn was sent the passage retrieved for the first:\n%s", two)
			}
		})
	}
}
