package native_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"
	ragtypes "github.com/urmzd/saige/rag/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// loopProvider is a model that calls a tool for a fixed number of turns and
// then answers. It decides from the request alone, never from a call counter
// the test set up in advance, so the same history always gets the same reply
// and a test does not have to know beforehand at which call a compaction will
// spend a request on a summary.
type loopProvider struct {
	turns int
	// usage, when set, is reported on every call.
	usage *saigetypes.UsageDelta

	mu       sync.Mutex
	requests []saigetypes.Request
}

func (p *loopProvider) Stream(_ context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)

	var out []saigetypes.Delta
	if p.usage != nil {
		out = append(out, *p.usage)
	}
	switch done := toolTurnsIn(req.Messages); {
	case isSummaryRequest(req):
		out = append(out, agenttest.TextResponse("the earlier lookups, summarized")...)
	case p.model() <= p.turns:
		out = append(out, agenttest.ToolCallResponse(fmt.Sprintf("call-%d-%d", p.model(), done), "lookup", map[string]any{"n": p.model()})...)
	default:
		out = append(out, agenttest.TextResponse("done")...)
	}
	return scripted(out), nil
}

// model counts the requests so far that were turns of the loop rather than
// summaries. The caller holds mu.
func (p *loopProvider) model() int {
	n := 0
	for _, r := range p.requests {
		if !isSummaryRequest(r) {
			n++
		}
	}
	return n
}

// calls returns every request received, summaries included.
func (p *loopProvider) calls() []saigetypes.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]saigetypes.Request(nil), p.requests...)
}

// isSummaryRequest reports whether req is a compaction asking for a summary
// rather than a turn of the loop.
func isSummaryRequest(req saigetypes.Request) bool {
	if len(req.Messages) == 0 {
		return false
	}
	return strings.Contains(renderMessages(req.Messages[:1]), saigetypes.SummaryInstruction)
}

func toolTurnsIn(msgs []saigetypes.Message) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(saigetypes.AssistantMessage); ok {
			n++
		}
	}
	return n
}

// wire renders a request as the bytes a prefix cache compares, in the order
// Anthropic (and every provider that caches by prefix) reads them: tool
// definitions, then the system prompt, then the conversation.
func wire(t *testing.T, req saigetypes.Request) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, d := range req.Tools {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal tool %q: %v", d.Name, err)
		}
		fmt.Fprintf(&b, "tool %s\n", raw)
	}
	b.Write(wireMessages(t, req.Messages))
	return b.Bytes()
}

func wireMessages(t *testing.T, msgs []saigetypes.Message) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %T: %v", m, err)
		}
		fmt.Fprintf(&b, "%T %s\n", m, raw)
	}
	return b.Bytes()
}

// stableHead is the part of a request that must survive everything, a
// compaction included: the tools, the system prompt, and the original request.
func stableHead(t *testing.T, req saigetypes.Request) []byte {
	t.Helper()
	if len(req.Messages) < 2 {
		t.Fatalf("request has %d messages, want at least the system prompt and the request", len(req.Messages))
	}
	return wire(t, saigetypes.Request{Tools: req.Tools, Messages: req.Messages[:2]})
}

// lookupTool returns a tool whose result is large enough that a handful of
// calls outgrow a small budget.
func lookupTool(name string) saigetypes.Tool {
	return &agenttest.MockTool{
		Def: saigetypes.ToolDef{
			Name:        name,
			Description: "Look something up (" + name + ")",
			Parameters:  saigetypes.ParameterSchema{Type: "object"},
		},
		Result: strings.Repeat("row of results. ", 40),
	}
}

// runLoop runs one turn of a tool-calling agent against p and returns the
// requests it sent that were turns of the loop, plus how many requests before
// each one were summaries.
func runLoop(t *testing.T, p *loopProvider, opts ...native.Option) (model []saigetypes.Request, summariesBefore []int, res native.Result) {
	t.Helper()
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Tools = []string{"lookup"}
	opts = append([]native.Option{
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return p, nil }),
		native.WithTools(native.StaticTools(lookupTool("lookup"))),
		native.WithMaxIter(p.turns + 5),
	}, opts...)
	r, err := native.New(rev, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, res = collect(t, r, native.Turn{Text: "find the answer", Meta: map[string]string{"issue": "42"}})
	if res.Text != "done" {
		t.Fatalf("turn answered %q, want it to finish the loop with %q", res.Text, "done")
	}

	summaries := 0
	for _, req := range p.calls() {
		if isSummaryRequest(req) {
			summaries++
			continue
		}
		model = append(model, req)
		summariesBefore = append(summariesBefore, summaries)
	}
	if len(model) != p.turns+1 {
		t.Fatalf("loop made %d model calls, want %d", len(model), p.turns+1)
	}
	return model, summariesBefore, res
}

// The property the prompt cache depends on: with no compaction, every model
// call of a turn sends exactly what the previous one sent, plus more. A single
// byte changed anywhere earlier would make the provider re-read the whole
// request at full price.
func TestEachRequestExtendsThePreviousOneByteForByte(t *testing.T) {
	t.Parallel()

	model, _, res := runLoop(t, &loopProvider{turns: 8})
	for k := 0; k+1 < len(model); k++ {
		prev, next := wire(t, model[k]), wire(t, model[k+1])
		if !bytes.HasPrefix(next, prev) {
			t.Fatalf("request %d is not a byte prefix of request %d:\n%s\n---\n%s", k, k+1, prev, next)
		}
		if len(next) <= len(prev) {
			t.Errorf("request %d did not grow past request %d: history must append", k+1, k)
		}
	}
	if len(res.Compactions) != 0 {
		t.Errorf("turn compacted %d times with no budget set; compaction must be opt-in", len(res.Compactions))
	}
}

// With a budget, the prefix property holds everywhere except at the one call
// where the history was compacted. Across that call the tools, the system
// prompt, and the original request are still shared, and after it requests go
// back to extending one another, which is what lets the cache warm up again.
func TestCompactionIsTheOnlyPlaceTheRequestPrefixChanges(t *testing.T) {
	t.Parallel()

	const turns = 10
	model, summariesBefore, res := runLoop(t, &loopProvider{turns: turns},
		native.WithCompaction(native.Compaction{MaxInputTokens: 1200, KeepTurns: 2}))

	if len(res.Compactions) == 0 {
		t.Fatalf("turn never compacted; the budget is too large for this test to prove anything")
	}
	if got := summariesBefore[len(summariesBefore)-1]; got != len(res.Compactions) {
		t.Errorf("provider wrote %d summaries but the result records %d compactions", got, len(res.Compactions))
	}
	// This provider reports no tokens, so the summaries are not counted.
	if res.Usage.ModelCalls != len(model) {
		t.Errorf("Usage.ModelCalls = %d, want %d (every call of the loop)", res.Usage.ModelCalls, len(model))
	}

	breaks := 0
	for k := 0; k+1 < len(model); k++ {
		prev, next := wire(t, model[k]), wire(t, model[k+1])
		compacted := summariesBefore[k+1] > summariesBefore[k]
		switch {
		case compacted:
			breaks++
			if bytes.HasPrefix(next, prev) {
				t.Errorf("request %d followed a compaction but still extends request %d: nothing was compacted", k+1, k)
			}
			if !bytes.Equal(stableHead(t, model[k]), stableHead(t, model[k+1])) {
				t.Errorf("compaction before request %d changed the tools, system prompt, or original request", k+1)
			}
			if len(next) >= len(prev) {
				t.Errorf("compaction before request %d did not shrink it (%d bytes, was %d)", k+1, len(next), len(prev))
			}
			if !strings.Contains(string(next), saigetypes.CompactionSummaryPrefix[:20]) {
				t.Errorf("request %d carries no summary of what was compacted", k+1)
			}
		case !bytes.HasPrefix(next, prev):
			t.Fatalf("request %d is not a byte prefix of request %d, and no compaction happened between them:\n%s\n---\n%s", k, k+1, prev, next)
		}
	}
	if breaks != len(res.Compactions) {
		t.Errorf("request prefix changed at %d calls, want exactly the %d compactions", breaks, len(res.Compactions))
	}
	if breaks > turns/2 {
		t.Errorf("%d compactions in %d calls: compaction must be a rare event", breaks, turns)
	}
	for i, c := range res.Compactions {
		if c.Trigger != string(saigetypes.CompactionTriggerInputPressure) {
			t.Errorf("compaction %d trigger = %q, want the input budget", i, c.Trigger)
		}
		if c.Summarized == 0 || c.TokensAfter >= c.TokensBefore {
			t.Errorf("compaction %d record %+v does not describe a history that shrank", i, c)
		}
	}
}

// A durable run that is executed again must compact at the same call and cut
// at the same place, or the two executions would send the model different
// conversations. Nothing but the history may enter the decision.
func TestCompactionDecisionsRepeatExactly(t *testing.T) {
	t.Parallel()

	run := func() ([][]byte, []int, []event.Compaction) {
		model, summariesBefore, res := runLoop(t, &loopProvider{turns: 10},
			native.WithCompaction(native.Compaction{MaxInputTokens: 1200, KeepTurns: 2}))
		wires := make([][]byte, len(model))
		for i, m := range model {
			wires[i] = wire(t, m)
		}
		return wires, summariesBefore, res.Compactions
	}

	firstWire, firstAt, firstRecs := run()
	secondWire, secondAt, secondRecs := run()

	if fmt.Sprint(firstAt) != fmt.Sprint(secondAt) {
		t.Fatalf("compactions happened at different calls: %v then %v", firstAt, secondAt)
	}
	if fmt.Sprintf("%+v", firstRecs) != fmt.Sprintf("%+v", secondRecs) {
		t.Errorf("compaction records differ: %+v then %+v", firstRecs, secondRecs)
	}
	for i := range firstWire {
		if !bytes.Equal(firstWire[i], secondWire[i]) {
			t.Fatalf("request %d differs between two executions of the same turn:\n%s\n---\n%s", i, firstWire[i], secondWire[i])
		}
	}
}

// The failure a budget must not cause: when the turns a compaction keeps are
// already over the budget, compacting again cannot help, and doing it before
// every call would pay for a summary and miss the cache each time. Compactions
// must stay at least KeepTurns calls apart however far over the budget the
// turn is.
func TestCompactionDoesNotRepeatOnEveryCallWhenTheBudgetCannotBeMet(t *testing.T) {
	t.Parallel()

	const keep = 3
	_, summariesBefore, res := runLoop(t, &loopProvider{turns: 14},
		native.WithCompaction(native.Compaction{MaxInputTokens: 1, KeepTurns: keep}))

	if len(res.Compactions) < 2 {
		t.Fatalf("turn compacted %d times; the test needs at least two to measure the distance between them", len(res.Compactions))
	}
	last := -1
	for k := 1; k < len(summariesBefore); k++ {
		if summariesBefore[k] == summariesBefore[k-1] {
			continue
		}
		if n := summariesBefore[k] - summariesBefore[k-1]; n != 1 {
			t.Errorf("%d summaries were written before call %d, want one compaction at a time", n, k)
		}
		if last >= 0 && k-last < keep {
			t.Errorf("compactions before calls %d and %d are %d calls apart, want at least %d", last, k, k-last, keep)
		}
		last = k
	}
}

// Tool definitions are the first bytes of the cached prefix. Their order must
// come from their names, not from the order a spec happens to list them in or
// from map iteration, or two runs of one agent would not share a prefix.
func TestToolOrderInTheRequestIsIndependentOfSpecOrder(t *testing.T) {
	t.Parallel()

	catalog := native.StaticTools(lookupTool("charlie"), lookupTool("alpha"), lookupTool("bravo"))
	orders := [][]string{
		{"alpha", "bravo", "charlie"},
		{"charlie", "alpha", "bravo"},
		{"bravo", "charlie", "alpha"},
	}

	var first []byte
	for _, order := range orders {
		// Repeated, because a map-ordered tool list is right some of the
		// time.
		for range 20 {
			p := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("ok")}}
			rev := offlineRevision(1, "You look things up.")
			rev.Spec.Tools = order
			r, err := native.New(rev,
				native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return p, nil }),
				native.WithTools(catalog),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			collect(t, r, native.Turn{Text: "hi"})

			calls := p.Requests()
			if len(calls) != 1 {
				t.Fatalf("provider saw %d requests, want 1", len(calls))
			}
			var names []string
			for _, d := range calls[0].Tools {
				names = append(names, d.Name)
			}
			if got := strings.Join(names, ","); got != "alpha,bravo,charlie" {
				t.Fatalf("spec order %v reached the request as %s, want tools sorted by name", order, got)
			}
			got := wire(t, saigetypes.Request{Tools: calls[0].Tools, Messages: calls[0].Messages})
			if first == nil {
				first = got
			}
			if !bytes.Equal(first, got) {
				t.Fatalf("spec order %v produced a different request:\n%s\n---\n%s", order, first, got)
			}
		}
	}
}

// Two different tags to one agent share a cacheable prefix only if everything
// that differs between them comes after everything that does not. Metadata,
// retrieved passages, and the request text all vary per run, so none of them
// may reach the tool definitions or the system prompt.
func TestWhatVariesBetweenRunsComesAfterWhatDoesNot(t *testing.T) {
	t.Parallel()

	const prompt = "You are docs-bot. Answer from the retrieved passages."
	request := func(turn native.Turn, passage string) saigetypes.Request {
		p := &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{agenttest.TextResponse("ok")}}
		rev := offlineRevision(3, prompt)
		rev.Spec.Tools = []string{"lookup"}
		rev.Spec.Sources = []native.Source{{Name: "docs", URI: "mem://docs"}}
		r, err := native.New(rev,
			native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return p, nil }),
			native.WithTools(native.StaticTools(lookupTool("lookup"))),
			native.WithRetriever(retrieverFunc(func(context.Context, string) (*ragtypes.AssembledContext, error) {
				return &ragtypes.AssembledContext{Prompt: passage}, nil
			})),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		collect(t, r, turn)
		calls := p.Requests()
		if len(calls) != 1 {
			t.Fatalf("provider saw %d requests, want 1", len(calls))
		}
		return saigetypes.Request{Tools: calls[0].Tools, Messages: calls[0].Messages}
	}

	a := request(native.Turn{Text: "how do I deploy?", Meta: map[string]string{"run": "run-aaa", "at": "2026-10-13T01:00:00Z"}}, "PASSAGE-ONE")
	b := request(native.Turn{Text: "what broke?", Meta: map[string]string{"run": "run-bbb", "at": "2026-10-13T02:00:00Z"}}, "PASSAGE-TWO")

	for name, req := range map[string]saigetypes.Request{"first": a, "second": b} {
		if len(req.Messages) != 2 {
			t.Fatalf("%s request has %d messages, want the system prompt and one request message", name, len(req.Messages))
		}
		sys, ok := req.Messages[0].(saigetypes.SystemMessage)
		if !ok {
			t.Fatalf("%s request starts with %T, want the system prompt", name, req.Messages[0])
		}
		if len(sys.Parts) != 1 || sys.Parts[0] != saigetypes.SystemPart(saigetypes.Text(prompt)) {
			t.Errorf("%s request's system prompt is %s, want exactly the pinned spec's prompt", name, renderMessages(req.Messages[:1]))
		}
	}

	stable := func(r saigetypes.Request) []byte {
		return wire(t, saigetypes.Request{Tools: r.Tools, Messages: r.Messages[:1]})
	}
	if !bytes.Equal(stable(a), stable(b)) {
		t.Fatalf("two runs of one revision do not share their tools and system prompt:\n%s\n---\n%s", stable(a), stable(b))
	}
	for _, volatile := range []string{"run-aaa", "2026-10-13T01:00:00Z", "PASSAGE-ONE", "how do I deploy?"} {
		if bytes.Contains(stable(a), []byte(volatile)) {
			t.Errorf("%q is in the stable prefix; it changes every run and must come after it", volatile)
		}
		if !bytes.Contains(wireMessages(t, a.Messages[1:]), []byte(volatile)) {
			t.Errorf("%q never reached the request message", volatile)
		}
	}
}

// Cache health has to be measurable from outside the provider: a turn reports
// how much of its input was read from the cache and how much was written, on
// its result and on one usage.call per model call, and nowhere else.
func TestUsageAccountsForCacheReadsAndWritesAcrossATurn(t *testing.T) {
	t.Parallel()

	p := &loopProvider{turns: 3, usage: &saigetypes.UsageDelta{
		PromptTokens: 1000, CachedPromptTokens: 800, CacheWriteTokens: 150, CompletionTokens: 20,
	}}
	var kinds []string
	rev := offlineRevision(1, "You look things up.")
	rev.Spec.Tools = []string{"lookup"}
	r, err := native.New(rev,
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return p, nil }),
		native.WithTools(native.StaticTools(lookupTool("lookup"))),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	chunks, res := collect(t, r, native.Turn{Text: "find the answer"})
	for _, c := range chunks {
		kinds = append(kinds, string(c.Type()))
	}

	// The test provider declares no rate card, so the calls are counted and
	// not priced.
	want := event.Usage{ModelCalls: 4, Requests: 4, PromptTokens: 4000, CachedPromptTokens: 3200, CacheWriteTokens: 600, CompletionTokens: 80, UnpricedCalls: 4}
	if !reflect.DeepEqual(res.Usage, want) {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	if got := res.Usage.CacheReadRatio(); got != 0.8 {
		t.Errorf("CacheReadRatio = %v, want 0.8", got)
	}
	calls := 0
	for _, c := range chunks {
		if c.Type() == event.TypeUsageCall {
			calls++
			continue
		}
		if strings.Contains(string(raw(c)), "prompt_tokens") {
			t.Errorf("token counts were published as a %s event (%v); they belong on usage.call only", c.Type(), kinds)
		}
	}
	if calls != 4 {
		t.Errorf("published %d usage.call events, want one per model call (4): %v", calls, kinds)
	}
}

// The offline path is what runs the example and the tests with no API key. A
// budget must not disturb a turn that stays under it: no extra provider call,
// and the script is replayed exactly as written.
func TestOfflineScriptRunsUnchangedUnderABudgetItDoesNotCross(t *testing.T) {
	t.Parallel()

	rev := offlineRevision(1, "You are helpful.")
	rev.Spec.Tools = []string{"lookup"}
	r, err := native.New(rev,
		native.WithScript(
			agenttest.ToolCallResponse("call-1", "lookup", map[string]any{}),
			agenttest.TextResponse("scripted answer"),
		),
		native.WithTools(native.StaticTools(lookupTool("lookup"))),
		native.WithCompaction(native.Compaction{MaxInputTokens: 100_000}),
		native.WithPromptCache(native.PromptCacheLong),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, res := collect(t, r, native.Turn{Text: "hi"})
	if res.Text != "scripted answer" {
		t.Errorf("Text = %q, want the script's answer", res.Text)
	}
	if res.Usage.ModelCalls != 2 || len(res.Compactions) != 0 {
		t.Errorf("turn made %d model calls and %d compactions, want 2 and 0", res.Usage.ModelCalls, len(res.Compactions))
	}
}

// A cache lifetime the API does not accept would fail every request of every
// run, so it is refused when the runner is built.
func TestAnUnknownPromptCacheLifetimeIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ttl     string
		wantErr bool
	}{
		{native.PromptCacheOff, false},
		{native.PromptCacheShort, false},
		{native.PromptCacheLong, false},
		{"24h", true},
		{"forever", true},
	}
	for _, tc := range tests {
		_, err := native.New(offlineRevision(1, ""), native.WithPromptCache(tc.ttl))
		if tc.wantErr != (err != nil) {
			t.Errorf("WithPromptCache(%q): err = %v, want an error: %v", tc.ttl, err, tc.wantErr)
		}
		if tc.wantErr && !errors.Is(err, native.ErrInvalid) {
			t.Errorf("WithPromptCache(%q): err = %v, want it to match ErrInvalid", tc.ttl, err)
		}
	}
}
