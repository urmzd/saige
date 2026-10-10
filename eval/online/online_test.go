package online_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	agenteval "github.com/urmzd/saige/agent/eval"
	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/online"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
)

// turn is one recorded message of a conversation fixture.
type turn struct {
	msg types.Message
}

func user(text string) turn { return turn{types.UserMsg(types.Text(text))} }

func assistant(text string, extra ...types.AssistantPart) turn {
	content := []types.AssistantPart{types.RoutePart{Model: "gpt-6-luna", Preset: "fast"}}
	content = append(content, extra...)
	if text != "" {
		content = append(content, types.TextPart{Text: text})
	}
	return turn{types.AssistantMessage{Parts: content}}
}

func call(id, name string) types.AssistantPart {
	return types.ToolCallPart{ID: id, Name: name, Arguments: map[string]any{"q": "x"}}
}

func result(id, text string, isErr bool) turn {
	return turn{types.SystemMessage{Parts: []types.SystemPart{
		types.ToolResultPart{CallID: id, Parts: []types.ToolOutputPart{types.Text(text)}, IsError: isErr},
	}}}
}

// conversation records turns on a new tree and returns it with the IDs of
// the nodes in order.
func conversation(t *testing.T, turns ...turn) (*tree.Tree, []types.NodeID) {
	t.Helper()
	tr, err := tree.New(types.SystemMsg(types.Text("You are a support agent.")))
	if err != nil {
		t.Fatal(err)
	}
	parent := tr.Root().ID
	var ids []types.NodeID
	for _, tu := range turns {
		n, err := tr.AddChild(context.Background(), parent, tu.msg)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
		parent = n.ID
	}
	return tr, ids
}

// supportConversation is a run with one successful tool call followed by a
// run with a failing tool call.
func supportConversation(t *testing.T) (*tree.Tree, []types.NodeID) {
	return conversation(t,
		user("What is the refund policy? Reply to jane.doe@example.com"),
		assistant("", call("c1", "lookup_policy")),
		result("c1", "Refunds within 30 days.", false),
		assistant("Refunds are accepted within 30 days of purchase."),
		user("Cancel order 42 for me"),
		assistant("", call("c2", "cancel_order")),
		result("c2", "order service unavailable", true),
		assistant("Sorry, I could not cancel the order."),
	)
}

func TestFromPathExtractsTheRun(t *testing.T) {
	tr, ids := supportConversation(t)
	recs, err := online.TreeSource{"conv-1": tr}.Records(context.Background(), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("Records = %d, want the two finished runs", len(recs))
	}
	first, second := recs[0], recs[1]
	if first.Ref != (online.Ref{Conversation: "conv-1", Node: string(ids[3])}) {
		t.Errorf("first ref = %+v", first.Ref)
	}
	if !strings.HasPrefix(first.Input, "What is the refund policy?") || first.Output != "Refunds are accepted within 30 days of purchase." {
		t.Errorf("first input/output = %q / %q", first.Input, first.Output)
	}
	if first.Model != "gpt-6-luna" || first.Preset != "fast" || first.Turns != 2 || first.Error != "" {
		t.Errorf("first = %+v", first)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Result != "Refunds within 30 days." || first.ToolCalls[0].Exec != agenteval.ExecFinished {
		t.Errorf("first tool calls = %+v", first.ToolCalls)
	}
	if second.Input != "Cancel order 42 for me" || second.Error != "tool cancel_order: order service unavailable" {
		t.Errorf("second = %+v", second)
	}
	if got := second.Tools(); len(got) != 1 || got[0] != "cancel_order" {
		t.Errorf("second tools = %v", got)
	}

	// A node that still calls tools does not end a run.
	if _, err := (online.TreeSource{"conv-1": tr}).Lookup(context.Background(), online.Ref{Conversation: "conv-1", Node: string(ids[1])}); !errors.Is(err, online.ErrNotFinished) {
		t.Errorf("Lookup of a tool-calling node = %v, want ErrNotFinished", err)
	}
	got, err := (online.TreeSource{"conv-1": tr}).Lookup(context.Background(), online.Ref{Conversation: "conv-1", Node: string(ids[7]), TraceID: "t1"})
	if err != nil || got.Ref.TraceID != "t1" || got.Output != second.Output {
		t.Errorf("Lookup = %+v, %v", got, err)
	}
}

// manyRecords returns n records in distinct conversations.
func manyRecords(n int) online.RecordSource {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	recs := make(online.RecordSource, n)
	for i := range recs {
		recs[i] = online.Record{
			Ref:        online.Ref{Conversation: fmt.Sprintf("c%03d", i), Node: fmt.Sprintf("n%03d", i)},
			FinishedAt: base.Add(time.Duration(i) * time.Minute),
			Input:      "q", Output: "a", Model: "gpt-6-luna",
		}
	}
	return recs
}

func TestSamplingIsDeterministicWithASeed(t *testing.T) {
	recs := manyRecords(400)
	pick := func(s *online.Sampler, recs online.RecordSource) map[string]bool {
		out := map[string]bool{}
		for _, r := range recs {
			if s.Sampled(r) {
				out[r.Ref.Node] = true
			}
		}
		return out
	}
	a := pick(&online.Sampler{Rate: 0.25, Seed: 7}, recs)
	reversed := make(online.RecordSource, len(recs))
	for i, r := range recs {
		reversed[len(recs)-1-i] = r
	}
	b := pick(&online.Sampler{Rate: 0.25, Seed: 7}, reversed)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatal("the same seed sampled different records when the order changed")
	}
	if n := len(a); n < 70 || n > 130 {
		t.Fatalf("rate 0.25 sampled %d of 400", n)
	}
	other := pick(&online.Sampler{Rate: 0.25, Seed: 8}, recs)
	if fmt.Sprint(a) == fmt.Sprint(other) {
		t.Fatal("a different seed sampled the same records")
	}
	// A higher rate keeps every record a lower rate kept.
	wider := pick(&online.Sampler{Rate: 0.5, Seed: 7}, recs)
	for node := range a {
		if !wider[node] {
			t.Fatalf("rate 0.5 dropped %s that rate 0.25 kept", node)
		}
	}
	if all := pick(&online.Sampler{Seed: 7}, recs); len(all) != len(recs) {
		t.Fatalf("rate 0 sampled %d, want every record", len(all))
	}

	// Two sweeps with the same seed record the same units.
	ctx := context.Background()
	keys := func() []string {
		ms := memstore.New()
		s := &online.Sampler{Store: ms, Rate: 0.25, Seed: 7, Scorers: []eval.Scorer{eval.ExactMatchScorer()}}
		rep, err := s.Sweep(ctx, recs, online.Window{})
		if err != nil {
			t.Fatal(err)
		}
		units, err := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, u := range units {
			out = append(out, u.Key)
		}
		return out
	}
	if k1, k2 := keys(), keys(); fmt.Sprint(k1) != fmt.Sprint(k2) || len(k1) != len(a) {
		t.Fatalf("sweeps recorded %d and %d units, want the same %d", len(k1), len(k2), len(a))
	}
}

func TestFilter(t *testing.T) {
	tr, _ := supportConversation(t)
	recs, err := online.TreeSource{"conv-1": tr}.Records(context.Background(), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	tests := []struct {
		name   string
		filter online.Filter
		want   int
	}{
		{"none", online.Filter{}, 2},
		{"model", online.Filter{Models: []string{"gpt-6-luna"}}, 2},
		{"other model", online.Filter{Models: []string{"other"}}, 0},
		{"preset", online.Filter{Presets: []string{"fast"}}, 2},
		{"tool", online.Filter{Tools: []string{"cancel_order"}}, 1},
		{"errored", online.Filter{Errored: &yes}, 1},
		{"clean", online.Filter{Errored: &no}, 1},
		{"labels", online.Filter{Where: eval.Where{online.LabelConversation: "conv-1", online.LabelErrored: "true"}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := 0
			for _, r := range recs {
				if tt.filter.Match(r) {
					n++
				}
			}
			if n != tt.want {
				t.Fatalf("matched %d, want %d", n, tt.want)
			}
		})
	}
}

func TestSweepRecordsLabeledUnitsWithProvenance(t *testing.T) {
	ctx := context.Background()
	tr, ids := supportConversation(t)
	ms := memstore.New()
	s := &online.Sampler{
		Store:   ms,
		Scorers: []eval.Scorer{agenteval.ToolSuccessRateScorer(), eval.ContainsScorer("30 days")},
		Labels:  eval.Labels{"env": "prod"},
	}
	from := time.Now().Add(-time.Hour)
	rep, err := s.Sweep(ctx, online.TreeSource{"conv-1": tr}, online.Window{From: from})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Seen != 2 || rep.Matched != 2 || rep.Scored != 2 {
		t.Fatalf("report = %+v", rep)
	}
	run, err := ms.GetRun(ctx, rep.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Suite != online.DefaultSuite || run.Status != eval.RunSucceeded || run.Labels[online.LabelSource] != online.SourceOnline || run.Labels["env"] != "prod" {
		t.Fatalf("run = %+v", run)
	}
	if run.Provenance.Extra["window_from"] == "" {
		t.Fatalf("run provenance has no window: %+v", run.Provenance)
	}
	units, err := ms.Units(ctx, run.ID, store.UnitFilter{Where: eval.Where{online.LabelSource: online.SourceOnline}})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("units = %d", len(units))
	}
	u := units[1]
	if u.Observation.ID != string(ids[7]) || u.Observation.Labels[online.LabelConversation] != "conv-1" || u.Observation.Labels[online.LabelNode] != string(ids[7]) {
		t.Fatalf("unit provenance = %+v", u.Observation.Labels)
	}
	scores := map[string]float64{}
	for _, sc := range u.Scores {
		scores[sc.Name] = sc.Value
	}
	if scores["tool_success_rate"] != 0 {
		t.Fatalf("failed tool call scored %v", scores)
	}

	// A window that ends before the runs finds nothing but still records a run.
	empty, err := s.Sweep(ctx, online.TreeSource{"conv-1": tr}, online.Window{From: from, To: from.Add(time.Minute)})
	if err != nil || empty.Seen != 0 || empty.Run.Units != 0 {
		t.Fatalf("empty sweep = %+v, %v", empty, err)
	}
	if _, err := ms.GetRun(ctx, empty.Run.ID); err != nil {
		t.Fatalf("empty sweep left no run: %v", err)
	}
}

func TestTraceLink(t *testing.T) {
	ctx := context.Background()
	ms := memstore.New()
	run := agenteval.AgentRun{Text: "done", TurnCount: 1, Routes: []agenteval.RouteRecord{{Model: "gpt-6-luna", Preset: "fast"}}}
	rec := online.FromAgentRun(online.Ref{Conversation: "c", Node: "n", TraceID: "trace-1", SpanID: "span-1"}, "hi", time.Now(), run)
	s := &online.Sampler{Store: ms, Scorers: []eval.Scorer{eval.ContainsScorer("done")}}
	rep, err := s.Sweep(ctx, online.RecordSource{rec}, online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	units, _ := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})
	if len(units) != 1 || units[0].Trace == nil || units[0].Trace.TraceID != "trace-1" || units[0].Trace.SpanID != "span-1" {
		t.Fatalf("unit trace = %+v", units)
	}
	if units[0].Observation.Labels[online.LabelModel] != "gpt-6-luna" {
		t.Fatalf("labels = %v", units[0].Observation.Labels)
	}
}

// judgeResponse is a scripted judge verdict with usage, so the budget can
// settle it.
func judgeResponse(score float64) []types.Delta {
	return append(agenttest.TextResponse(fmt.Sprintf(`{"reasoning": "ok", "score": %g}`, score)),
		types.UsageDelta{PromptTokens: 100, CompletionTokens: 10})
}

func TestJudgesStopAtTheBudget(t *testing.T) {
	ctx := context.Background()
	prov := &agenttest.ScriptedProvider{Responses: [][]types.Delta{judgeResponse(0.9), judgeResponse(0.8), judgeResponse(0.7)}}
	budget := types.NewBudget(types.BudgetPolicy{MaxRequests: 2})
	gen := &online.BudgetedGenerator{Provider: prov, Budget: budget}
	ms := memstore.New()
	s := &online.Sampler{
		Store:   ms,
		Scorers: []eval.Scorer{eval.ContainsScorer("a")},
		Judges:  []eval.Scorer{eval.NewJudgeScorer(gen)},
		Budget:  budget,
	}
	rep, err := s.Sweep(ctx, manyRecords(4), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if prov.CallCount() != 2 || rep.JudgesSkipped != 2 {
		t.Fatalf("judge calls = %d, skipped = %d; want 2 and 2", prov.CallCount(), rep.JudgesSkipped)
	}
	if got := budget.Usage(); got.Requests != 2 || got.InputTokens != 200 {
		t.Fatalf("budget usage = %+v", got)
	}
	units, _ := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})
	judged := 0
	for _, u := range units {
		if len(u.Scores) == 0 || u.Scores[0].Name != "contains:a" {
			t.Fatalf("deterministic scorer missing on %s: %+v", u.Key, u.Scores)
		}
		for _, sc := range u.Scores {
			if sc.Error != "" {
				t.Fatalf("budget refusal recorded as an error: %+v", sc)
			}
			if sc.Name == "judge_score" {
				judged++
			}
		}
	}
	if judged != 2 {
		t.Fatalf("judged %d units, want 2", judged)
	}
}

func TestPromoteRedactsPersonalData(t *testing.T) {
	ctx := context.Background()
	tr, _ := supportConversation(t)
	ms := memstore.New()
	s := &online.Sampler{Store: ms, Scorers: []eval.Scorer{agenteval.ToolSuccessRateScorer(), eval.ContainsScorer("never")}}
	rep, err := s.Sweep(ctx, online.TreeSource{"conv-1": tr}, online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	units, _ := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})

	// Flag the first unit by label; the second fails tool_success_rate.
	units[0].Observation.Labels[online.LabelFlagged] = "true"
	cases, err := online.Promote(ctx, units, online.PromoteOptions{
		Failing: online.Failing(1, "tool_success_rate"),
		Expected: func(u eval.Unit) json.RawMessage {
			if u.Key != units[0].Key {
				return nil
			}
			return json.RawMessage(`{"answer": "Refunds within 30 days; we will write to jane.doe@example.com"}`)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("promoted %d cases, want 2", len(cases))
	}
	var buf bytes.Buffer
	if err := online.WriteCases(&buf, cases); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "jane.doe@example.com") {
		t.Fatalf("promoted cases leak an email address:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED:EMAIL]") {
		t.Fatalf("promoted cases show no redaction:\n%s", out)
	}
	first, second := cases[0], cases[1]
	if first.Expected == nil || first.Rubric != "" || first.Reasons[0] != "flagged" {
		t.Fatalf("flagged case = %+v", first)
	}
	if second.Expected != nil || !strings.Contains(second.Rubric, "tool_success_rate") {
		t.Fatalf("failing case = %+v", second)
	}
	if second.Labels[online.LabelPromotedFrom] != rep.Run.ID || second.Labels[online.LabelConversation] != "conv-1" {
		t.Fatalf("case labels = %v", second.Labels)
	}
	obs := second.Observation()
	if obs.Annotations[online.AnnotationRubric] == nil {
		t.Fatal("case observation has no rubric annotation")
	}

	// The default promotes only flagged units, failed gates, and errors.
	none, err := online.Promote(ctx, units[1:], online.PromoteOptions{})
	if err != nil || len(none) != 0 {
		t.Fatalf("default promotion = %+v, %v; want none", none, err)
	}
}

func TestWatchScoresAnnouncedRuns(t *testing.T) {
	tr, ids := supportConversation(t)
	src := online.TreeSource{"conv-1": tr}
	n := notify.NewMemory(0)
	defer n.Close()
	ms := memstore.New()
	s := &online.Sampler{Store: ms, Scorers: []eval.Scorer{eval.ContainsScorer("30 days")}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	scored := make(chan eval.Unit, 4)
	type result struct {
		rep online.Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := s.Watch(ctx, n, src, online.WatchOptions{
			OnReady: func(id string) { ready <- id },
			OnUnit:  func(u eval.Unit) { scored <- u },
		})
		done <- result{rep, err}
	}()
	runID := <-ready

	announce := func(node types.NodeID) {
		t.Helper()
		if err := online.Announce(ctx, n, "", online.Ref{Conversation: "conv-1", Node: string(node)}); err != nil {
			t.Fatal(err)
		}
	}
	announce(ids[3])
	announce(ids[3]) // a repeat is scored once
	announce(ids[1]) // not the end of a run
	announce(ids[7])
	for range 2 {
		select {
		case <-scored:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a scored unit")
		}
	}
	cancel()
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.rep.Scored != 2 || res.rep.Run.ID != runID {
		t.Fatalf("report = %+v", res.rep)
	}
	run, err := ms.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != eval.RunSucceeded || run.Units != 2 || run.Aggregate["contains:30 days"] != 0.5 {
		t.Fatalf("finished run = %+v", run)
	}
}

func TestWatchCatchesUpFromSince(t *testing.T) {
	tr, _ := supportConversation(t)
	n := notify.NewMemory(0)
	defer n.Close()
	ms := memstore.New()
	s := &online.Sampler{Store: ms, Scorers: []eval.Scorer{eval.ContainsScorer("30 days")}}
	ctx, cancel := context.WithCancel(context.Background())
	var rep online.Report
	var err error
	rep, err = s.Watch(ctx, n, online.TreeSource{"conv-1": tr}, online.WatchOptions{
		Since:   time.Now().Add(-time.Hour),
		OnReady: func(string) { cancel() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scored != 2 || rep.Run.Units != 2 {
		t.Fatalf("catch-up report = %+v", rep)
	}
}

// TestJudgeOutageIsInconclusive checks that a judge whose provider is down
// leaves an inconclusive score: counted on the run, and never a reason to
// promote the unit as a failing case.
func TestJudgeOutageIsInconclusive(t *testing.T) {
	ctx := context.Background()
	outage := &types.ProviderError{Provider: "scripted", Kind: types.ErrorKindUnavailable, Code: 503, Err: errors.New("overloaded")}
	prov := &agenttest.ScriptedProvider{
		Responses: [][]types.Delta{nil, judgeResponse(0.9)},
		Errors:    []error{outage, nil},
	}
	ms := memstore.New()
	s := &online.Sampler{
		Store:   ms,
		Scorers: []eval.Scorer{eval.ContainsScorer("a")},
		Judges:  []eval.Scorer{eval.NewJudgeScorer(&online.BudgetedGenerator{Provider: prov})},
	}
	rep, err := s.Sweep(ctx, manyRecords(2), online.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run.Inconclusive != 1 {
		t.Fatalf("run inconclusive = %d, want 1: %+v", rep.Run.Inconclusive, rep.Run)
	}
	units, _ := ms.Units(ctx, rep.Run.ID, store.UnitFilter{})
	var judged []eval.Score
	for _, u := range units {
		for _, sc := range u.Scores {
			if sc.Name == "judge_score" {
				judged = append(judged, sc)
			}
		}
	}
	inconclusive := 0
	for _, sc := range judged {
		if sc.Inconclusive {
			if sc.Error == "" {
				t.Fatalf("inconclusive score without its error: %+v", sc)
			}
			inconclusive++
		}
	}
	if len(judged) != 2 || inconclusive != 1 {
		t.Fatalf("judge scores = %+v, want one of two inconclusive", judged)
	}
	cases, err := online.Promote(ctx, units, online.PromoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Fatalf("promoted %d cases from a judge outage: %+v", len(cases), cases)
	}
}
