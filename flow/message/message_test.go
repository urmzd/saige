package message_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/message"
)

var update = flag.Bool("update", false, "rewrite the goldens in testdata")

const rootRun = "root"

var head = message.Head{ID: "msg_1", RequestID: "req_1", RootRun: rootRun, Agent: "lead"}

// run is a run of a test tree: its id, agent and lineage.
type run struct {
	id, agent, parentRun, parentCall string
}

var root = run{id: rootRun, agent: "lead"}

// tree builds a run tree's envelopes in total order, 100ms apart.
type tree struct {
	t0   time.Time
	envs []event.Envelope
	seq  map[string]uint64
}

func newTree() *tree {
	return &tree{t0: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), seq: map[string]uint64{}}
}

func (b *tree) on(r run, bodies ...event.Body) *tree {
	for _, body := range bodies {
		b.seq[r.id]++
		n := len(b.envs) + 1
		b.envs = append(b.envs, event.Envelope{
			ID: fmt.Sprintf("ev_%d", n), Source: "test://runtime", Type: body.Type(), Channel: "test://runs",
			Time: b.t0.Add(time.Duration(n) * 100 * time.Millisecond),
			Run:  r.id, Seq: b.seq[r.id], Agent: r.agent,
			Root: rootRun, ParentRun: r.parentRun, ParentToolCall: r.parentCall,
			Data: body,
		})
	}
	return b
}

func args(s string) json.RawMessage { return json.RawMessage(s) }

func requested(id, name string, parent ...string) event.ToolCall {
	return event.ToolCall{ID: id, Name: name, Phase: event.PhaseRequested, Parent: parent}
}

func executing(id, name, a string, parent ...string) event.ToolCall {
	return event.ToolCall{ID: id, Name: name, Phase: event.PhaseExecuting, Arguments: args(a), Parent: parent}
}

func usd(micros int64) *event.Cost { return &event.Cost{Micros: micros, Currency: "USD"} }

// textCitation: a root answering in text, citing a source.
func textCitation() []event.Envelope {
	return newTree().on(root,
		event.Accepted{},
		event.Started{Attempt: 1},
		event.Citation{Label: "[1]", Text: "Go 1.26 was released in February 2026.", Index: 0,
			Source: event.Provenance{DocumentTitle: "Go 1.26 release notes", SourceURI: "https://go.dev/doc/go1.26"}},
		event.Text{Text: "Go 1.26 "},
		event.Text{Text: "shipped in February."},
		event.UsageCall{AccountingID: "call_1", Model: "claude-x", PromptTokens: 120, CachedPromptTokens: 100,
			CompletionTokens: 20, Cost: usd(1500)},
		event.Completed{Text: "Go 1.26 shipped in February."},
	).envs
}

// parallelAgents: a root calls two agents whose lanes interleave, each
// making a tool call with the same provider id.
func parallelAgents() []event.Envelope {
	a := run{id: "run_a", agent: "researcher", parentRun: rootRun, parentCall: "htu_root_1"}
	b := run{id: "run_b", agent: "writer", parentRun: rootRun, parentCall: "htu_root_2"}
	return newTree().
		on(root, event.Accepted{}, event.Text{Text: "Asking two agents."},
			requested("toolu_1", "agent"), requested("toolu_2", "agent"),
			executing("toolu_1", "agent", `{"agent":"researcher","title":"Find sources"}`),
			executing("toolu_2", "agent", `{"agent":"writer","title":"Draft"}`)).
		on(a, event.Accepted{}).
		on(b, event.Accepted{}).
		on(a, event.Citation{Label: "[1]", Text: "Lanes nest under calls.", Index: 0,
			Source: event.Provenance{DocumentTitle: "Design notes", SourceURI: "https://example.com/notes"}},
			event.Text{Text: "Found one."}).
		on(b, event.Text{Text: "Drafting."}).
		on(a, requested("toolu_1", "search")).
		on(b, requested("toolu_1", "lint")).
		on(a, executing("toolu_1", "search", `{"q":"lanes"}`)).
		on(b, executing("toolu_1", "lint", `{}`)).
		on(b, event.ToolDone{ID: "toolu_1", Name: "lint", Result: "clean"}).
		on(a, event.ToolDone{ID: "toolu_1", Name: "search", Result: "3 hits"}).
		on(a, event.Text{Text: " Done."}).
		on(b, event.Text{Text: " Draft ready."}).
		on(a, event.UsageCall{AccountingID: "call_a", Model: "claude-y", PromptTokens: 50, CompletionTokens: 10, Cost: usd(200)}).
		on(root, event.UsageCall{AccountingID: "call_a", Depth: 1, Model: "claude-y", PromptTokens: 50, CompletionTokens: 10, Cost: usd(200)}).
		on(a, event.Completed{Text: "Found one. Done."}).
		on(b, event.Completed{Text: "Drafting. Draft ready."}).
		on(root, event.ToolDone{ID: "toolu_2", Name: "agent", Result: "Drafting. Draft ready."}).
		on(root, event.ToolDone{ID: "toolu_1", Name: "agent", Result: "Found one. Done."}).
		on(root, event.Text{Text: "Both agents answered."},
			event.UsageCall{AccountingID: "call_root", Model: "claude-x", PromptTokens: 300, CompletionTokens: 40, Cost: usd(3000)},
			event.Completed{}).
		envs
}

// grandchildApproval: a run two calls below the root parks for approval.
func grandchildApproval() []event.Envelope {
	c := run{id: "run_c", agent: "ops", parentRun: rootRun, parentCall: "htu_root_1"}
	g := run{id: "run_g", agent: "deployer", parentRun: "run_c", parentCall: "htu_run_c_1"}
	return newTree().
		on(root, event.Accepted{}, requested("toolu_1", "agent"),
			executing("toolu_1", "agent", `{"agent":"ops","title":"Ship it"}`)).
		on(c, event.Accepted{}, requested("toolu_9", "agent"),
			executing("toolu_9", "agent", `{"agent":"deployer","title":"Deploy"}`)).
		on(g, event.Accepted{}, event.Text{Text: "Deploying."}, requested("toolu_d", "deploy"),
			event.Parked{Question: &event.Question{ID: "q_1", Kind: event.QuestionApproval,
				Prompt: "Allow deployer to call deploy?", Approvers: []string{"alice"},
				Tool: &event.QuestionTool{Name: "deploy", CallID: "toolu_d"}}}).
		on(c, event.ChildParked{RunID: "run_g", Agent: "deployer", Question: event.Question{ID: "q_1"}}).
		on(g, event.Resumed{Answer: &event.Answer{QuestionID: "q_1", Decision: event.DecisionApprove}},
			executing("toolu_d", "deploy", `{"env":"prod"}`),
			event.ToolDone{ID: "toolu_d", Name: "deploy", Result: "deployed"},
			event.Completed{Text: "Deploying."}).
		on(c, event.ToolDone{ID: "toolu_9", Name: "agent", Result: "Deploying."},
			event.Completed{Text: "Shipped."}).
		on(root, event.ToolDone{ID: "toolu_1", Name: "agent", Result: "Shipped."},
			event.Text{Text: "Shipped to prod."}, event.Completed{}).
		envs
}

// heldEnding: the root completes while an agent it called still runs, and
// while a background task runs that it does not wait for.
func heldEnding() []event.Envelope {
	s := run{id: "run_s", agent: "scout", parentRun: rootRun, parentCall: "htu_root_1"}
	bg := run{id: "run_bg", agent: "janitor", parentRun: rootRun, parentCall: "htu_root_2"}
	return newTree().
		on(root, event.Accepted{},
			requested("toolu_1", "agent"), executing("toolu_1", "agent", `{"agent":"scout","title":"Scout"}`),
			event.ToolDone{ID: "toolu_1", Name: "agent", Result: "started"},
			requested("toolu_2", "delegate_task"), executing("toolu_2", "delegate_task", `{"task":"tidy"}`),
			event.ToolDone{ID: "toolu_2", Name: "delegate_task", Result: "queued"}).
		on(s, event.Accepted{}).
		on(bg, event.Accepted{}).
		on(root, event.Text{Text: "Scout is on it."}, event.Completed{}).
		on(s, event.Text{Text: "Scouted."}, event.Completed{}).
		on(bg, event.Text{Text: "Tidied."}, event.Completed{}).
		envs
}

// failedContext: a root that fails terminally on a full context window.
func failedContext() []event.Envelope {
	return newTree().on(root,
		event.Accepted{}, event.Text{Text: "Reading everything."},
		event.Failed{Error: "provider anthropic (model claude-x, status 400): prompt is too long", Terminal: true},
	).envs
}

// failedRetry: a root that fails, is retried, and fails for good.
func failedRetry() []event.Envelope {
	return newTree().on(root,
		event.Accepted{}, event.Started{Attempt: 1}, event.Text{Text: "First try."},
		event.Failed{Attempt: 1, Error: "connection reset"},
		event.Started{Attempt: 2}, event.Text{Text: "Second try."},
		event.Failed{Attempt: 2, Error: "boom", Terminal: true},
	).envs
}

// stopped: a root stopped while a tool call runs.
func stopped() []event.Envelope {
	return newTree().on(root,
		event.Accepted{}, requested("toolu_1", "search"), executing("toolu_1", "search", `{"q":"x"}`),
		event.Stopped{Reason: "asked to stop", Text: "Partial answer."},
	).envs
}

// inRunNested: the root's agent runs a nested agent as a tool, which runs
// one of its own.
func inRunNested() []event.Envelope {
	return newTree().on(root,
		event.Accepted{},
		requested("toolu_1", "researcher"), executing("toolu_1", "researcher", `{"q":"sources"}`),
		event.Text{Text: "Looking.", Parent: []string{"toolu_1"}},
		requested("inner_1", "search", "toolu_1"), executing("inner_1", "search", `{"q":"go"}`, "toolu_1"),
		event.ToolDone{ID: "inner_1", Name: "search", Result: "1 hit", Parent: []string{"toolu_1"}},
		event.Citation{Label: "[1]", Text: "A cited line.", Index: 0, Parent: []string{"toolu_1"},
			Source: event.Provenance{DocumentTitle: "Doc", SourceURI: "https://example.com/doc"}},
		requested("inner_2", "summarizer", "toolu_1"), executing("inner_2", "summarizer", `{}`, "toolu_1"),
		event.Thinking{Text: "Condensing.", Parent: []string{"toolu_1", "inner_2"}},
		event.Text{Text: "Short.", Parent: []string{"toolu_1", "inner_2"}},
		event.ToolDone{ID: "inner_2", Name: "summarizer", Result: "Short.", Parent: []string{"toolu_1"}},
		event.ToolDone{ID: "toolu_1", Name: "researcher", Result: "Looking. Short."},
		event.Text{Text: "Here it is."},
		event.Completed{},
	).envs
}

// lineage: one called run names its call by the provider's id, as a run
// the runtime starts does, and one by the id the host minted.
func lineage() []event.Envelope {
	byProvider := run{id: "run_p", agent: "planner", parentRun: rootRun, parentCall: "toolu_1"}
	byHost := run{id: "run_h", agent: "helper", parentRun: rootRun, parentCall: "htu_root_2"}
	return newTree().
		on(root, event.Accepted{},
			executing("toolu_1", "session_call", `{"agent":"planner","text":"plan"}`),
			executing("toolu_2", "agent", `{"agent":"helper","title":"Help"}`)).
		on(byProvider, event.Accepted{}, event.Text{Text: "Planned."}).
		on(byHost, event.Accepted{}, event.Text{Text: "Helped."}).
		on(byProvider, event.Completed{}).
		on(byHost, event.Completed{}).
		on(root, event.ToolDone{ID: "toolu_1", Name: "session_call", Result: "Planned."},
			event.ToolDone{ID: "toolu_2", Name: "agent", Result: "Helped."}, event.Completed{}).
		envs
}

// batchPark: a called agent parks once on three waits, two approvals and
// a choice, answered out of order, and one approval denied.
func batchPark() []event.Envelope {
	c := run{id: "run_c", agent: "ops", parentRun: rootRun, parentCall: "htu_root_1"}
	qa := event.Question{ID: "q_a", Kind: event.QuestionApproval, Prompt: "Allow ops to call deploy?",
		Approvers: []string{"alice"}, Tool: &event.QuestionTool{Name: "deploy", CallID: "toolu_a"}}
	qb := event.Question{ID: "q_b", Kind: event.QuestionApproval, Prompt: "Allow ops to call migrate?",
		Approvers: []string{"bob"}, Tool: &event.QuestionTool{Name: "migrate", CallID: "toolu_b"}}
	qq := event.Question{ID: "q_q", Kind: event.QuestionChoice, Prompt: "Which region?",
		Options: []string{"eu", "us"}, AskedOf: "carol"}
	answers := []event.Answer{
		{QuestionID: "q_q", Choice: "eu"},
		{QuestionID: "q_b", Decision: event.DecisionApprove},
		{QuestionID: "q_a", Decision: event.DecisionDeny},
	}
	return newTree().
		on(root, event.Accepted{}, requested("toolu_1", "agent"),
			executing("toolu_1", "agent", `{"agent":"ops","title":"Release"}`)).
		on(c, event.Accepted{}, event.Text{Text: "Need three answers."},
			requested("toolu_a", "deploy"), requested("toolu_b", "migrate"), requested("toolu_q", "ask_person"),
			event.Parked{Question: &qa, Questions: []event.Question{qa, qb, qq}},
			event.Resumed{Answer: &answers[0], Answers: answers},
			executing("toolu_b", "migrate", `{"to":"v2"}`),
			event.ToolDone{ID: "toolu_b", Name: "migrate", Result: "migrated"},
			event.ToolDone{ID: "toolu_a", Name: "deploy", Error: "denied by alice"},
			executing("toolu_q", "ask_person", `{"question":"Which region?"}`),
			event.ToolDone{ID: "toolu_q", Name: "ask_person", Result: "eu"},
			event.Text{Text: "Migrated in eu; the deploy was denied."},
			event.Completed{}).
		on(root, event.ToolDone{ID: "toolu_1", Name: "agent", Result: "Migrated in eu; the deploy was denied."},
			event.Text{Text: "Released, without the deploy."}, event.Completed{}).
		envs
}

type scenario struct {
	name string
	envs func() []event.Envelope
	// check looks at the stream as each envelope adds to it.
	check func(t *testing.T, p *message.Projector, i int, added []message.Event)
	// open are the lanes the response ends without, by their call.
	open []string
}

var scenarios = []scenario{
	{name: "text_citation", envs: textCitation},
	{name: "parallel_agents", envs: parallelAgents},
	{name: "grandchild_approval", envs: grandchildApproval},
	{name: "held_ending", envs: heldEnding, check: checkHeld, open: []string{"htu_root_2"}},
	{name: "failed_context", envs: failedContext},
	{name: "failed_retry", envs: failedRetry},
	{name: "stopped", envs: stopped},
	{name: "in_run_nested", envs: inRunNested},
	{name: "lineage", envs: lineage},
	{name: "batch_park", envs: batchPark},
}

func project(envs []event.Envelope, check func(int, *message.Projector, []message.Event)) []message.Event {
	p := message.NewProjector(head)
	var out []message.Event
	for i, e := range envs {
		added := p.Add(e)
		if check != nil {
			check(i, p, added)
		}
		out = append(out, added...)
	}
	return out
}

func wire(t *testing.T, events []message.Event) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, e := range events {
		line, err := message.MarshalEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./flow/message -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden (run go test ./flow/message -update to accept):\n%s", path, got)
	}
}

func TestProjectGoldens(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var check func(int, *message.Projector, []message.Event)
			if sc.check != nil {
				check = func(i int, p *message.Projector, added []message.Event) { sc.check(t, p, i, added) }
			}
			stream := project(sc.envs(), check)
			checkWellFormed(t, stream, sc.open...)
			golden(t, sc.name+".jsonl", wire(t, stream))

			fold := map[string]message.Message{
				"depth_0": message.Fold(stream, 0),
				"depth_1": message.Fold(stream, 1),
				"all":     message.Fold(stream, -1),
			}
			b, err := json.MarshalIndent(fold, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, sc.name+".fold.json", append(b, '\n'))
			if !fold["all"].Done {
				t.Error("the folded message is not done")
			}
		})
	}
}

// TestPurity replays the same envelopes into a fresh projector, and gets the
// same stream.
func TestPurity(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			first := wire(t, project(sc.envs(), nil))
			second := wire(t, project(sc.envs(), nil))
			if !bytes.Equal(first, second) {
				t.Fatalf("a replay differs:\n%s\nvs\n%s", first, second)
			}
		})
	}
}

func TestDuplicatesAndOtherRoots(t *testing.T) {
	envs := parallelAgents()
	want := wire(t, project(envs, nil))

	other := newTree().on(run{id: "elsewhere"}, event.Accepted{}, event.Text{Text: "not ours"}).envs
	for i := range other {
		other[i].Root = "elsewhere"
	}
	p := message.NewProjector(head)
	var got []message.Event
	for i, e := range envs {
		got = append(got, p.Add(e)...)
		if added := p.Add(e); added != nil {
			t.Fatalf("envelope %d again added %d events", i, len(added))
		}
		if added := p.Add(other[i%len(other)]); added != nil {
			t.Fatalf("an envelope of another root added %d events", len(added))
		}
	}
	if !bytes.Equal(wire(t, got), want) {
		t.Fatal("duplicates changed the stream")
	}
	cur := p.Cursor()
	wantCur := message.Cursor{rootRun: 12, "run_a": 9, "run_b": 7}
	if !reflect.DeepEqual(cur, wantCur) {
		t.Fatalf("cursor = %v, want %v", cur, wantCur)
	}
	if _, ok := cur["elsewhere"]; ok {
		t.Fatal("the cursor counts another root's run")
	}
}

// checkHeld: the root's end waits for the agent it called, not for the
// background task.
func checkHeld(t *testing.T, p *message.Projector, i int, added []message.Event) {
	t.Helper()
	const rootCompleted, scoutCompleted = 10, 12
	stops := 0
	for _, e := range added {
		if _, ok := e.(message.MessageStop); ok {
			stops++
		}
	}
	switch {
	case i < scoutCompleted && p.Done():
		t.Fatalf("envelope %d: the response ended before the scout did", i)
	case i == rootCompleted && stops != 0:
		t.Fatal("the root's end was not held")
	case i == scoutCompleted && !p.Done():
		t.Fatal("the held end was not sent when the scout ended")
	case i > scoutCompleted && added != nil:
		t.Fatalf("envelope %d added %d events after the response ended", i, len(added))
	}
}

func TestEndings(t *testing.T) {
	tests := []struct {
		name string
		envs func() []event.Envelope
		want string
	}{
		{"completed", textCitation, message.StopEndTurn},
		{"context window", failedContext, message.StopContextWindow},
		{"failed", failedRetry, message.StopFailed},
		{"stopped", stopped, message.StopStopped},
		{"max tokens", func() []event.Envelope {
			return newTree().on(root, event.Accepted{},
				event.Failed{Error: "response truncated (finish reason max_tokens)", Terminal: true}).envs
		}, message.StopMaxTokens},
		{"refusal", func() []event.Envelope {
			return newTree().on(root, event.Accepted{},
				event.Failed{Error: "provider openai (model m): content_filter", Terminal: true}).envs
		}, message.StopRefusal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := message.Fold(project(tt.envs(), nil), 0)
			if m.StopReason != tt.want || !m.Done {
				t.Fatalf("stop reason %q done %v, want %q done", m.StopReason, m.Done, tt.want)
			}
		})
	}
}

func TestStoppedCallStatus(t *testing.T) {
	m := message.Fold(project(stopped(), nil), 0)
	var got string
	for _, c := range m.Content {
		if c.Type == message.BlockServerToolUse && c.Status != nil {
			got = c.Status.Status
		}
	}
	if got != message.StatusStopped {
		t.Fatalf("the running call's status is %q, want %q", got, message.StatusStopped)
	}
}

// TestBubble: a grandchild's approval shows on the root's call, with the
// path down to it, and clears when it is answered.
func TestBubble(t *testing.T) {
	stream := project(grandchildApproval(), nil)
	var root []message.ToolStatus
	for _, e := range stream {
		if s, ok := e.(message.ToolStatus); ok {
			root = append(root, s)
		}
	}
	want := []message.ToolStatus{
		{ToolUseID: "htu_root_1", Path: []string{"htu_root_1"}, Status: message.StatusQueued},
		{ToolUseID: "htu_root_1", Path: []string{"htu_root_1"}, Status: message.StatusRunning, Title: "Ship it"},
		{ToolUseID: "htu_root_1", Path: []string{"htu_root_1", "htu_run_c_1", "htu_run_g_1"},
			Status: message.StatusAwaitingApproval, ElapsedMs: 700,
			Question: &message.Question{ID: "q_1", Kind: event.QuestionApproval, Prompt: "Allow deployer to call deploy?", Who: []string{"alice"}}},
		{ToolUseID: "htu_root_1", Path: []string{"htu_root_1"}, Status: message.StatusRunning, Title: "Ship it", ElapsedMs: 900},
		{ToolUseID: "htu_root_1", Path: []string{"htu_root_1"}, Status: message.StatusDone, Title: "Ship it", ElapsedMs: 1500},
	}
	if !reflect.DeepEqual(root, want) {
		g, _ := json.MarshalIndent(root, "", " ")
		t.Fatalf("root statuses:\n%s", g)
	}
	m := message.Fold(stream, 0)
	if s := m.Content[0].Status; s == nil || s.Status != message.StatusDone {
		t.Fatalf("folded status = %+v", s)
	}
	// Folded before the answer, a depth-0 reader sees the question.
	var upTo []message.Event
	for _, e := range stream {
		upTo = append(upTo, e)
		if s, ok := e.(message.ToolStatus); ok && s.Status == message.StatusAwaitingApproval {
			break
		}
	}
	m = message.Fold(upTo, 0)
	if s := m.Content[0].Status; s == nil || s.Question == nil || len(s.Path) != 3 {
		t.Fatalf("a depth-0 reader does not see the grandchild's question: %+v", s)
	}
}

// TestBatchPark: every wait of a parked batch shows on its own call, each
// deep one also on the depth-one call, and each answer, matched by its
// question, moves its own call on.
func TestBatchPark(t *testing.T) {
	stream := project(batchPark(), nil)
	parkedAt, resumedAt := -1, -1
	for i, e := range stream {
		if p, ok := e.(message.Progress); ok && p.Text == "ops: awaiting approval" {
			parkedAt = i
		}
		if p, ok := e.(message.Progress); ok && p.Text == "ops: running" && parkedAt >= 0 && resumedAt < 0 {
			resumedAt = i
		}
	}
	if parkedAt < 0 || resumedAt < 0 {
		t.Fatalf("no park and resume progress (%d, %d)", parkedAt, resumedAt)
	}
	type status struct {
		scope, call, status, question string
		path                          int
	}
	var park, resume []status
	for i, e := range stream {
		scope := "root"
		if n, ok := e.(message.Nested); ok {
			scope, e = n.ParentToolUseID, n.Event
		}
		s, ok := e.(message.ToolStatus)
		if !ok {
			continue
		}
		q := ""
		if s.Question != nil {
			q = s.Question.ID
		}
		st := status{scope, s.ToolUseID, s.Status, q, len(s.Path)}
		switch {
		case i < parkedAt && s.Status != message.StatusQueued && s.Status != message.StatusRunning:
			park = append(park, st)
		case i > parkedAt && i < resumedAt:
			resume = append(resume, st)
		}
	}
	wantPark := []status{
		{"root", "htu_root_1", message.StatusAwaitingInput, "q_q", 1},
		{"htu_root_1", "htu_run_c_1", message.StatusAwaitingApproval, "q_a", 2},
		{"root", "htu_root_1", message.StatusAwaitingApproval, "q_a", 2},
		{"htu_root_1", "htu_run_c_2", message.StatusAwaitingApproval, "q_b", 2},
		{"root", "htu_root_1", message.StatusAwaitingApproval, "q_b", 2},
	}
	wantResume := []status{
		{"root", "htu_root_1", message.StatusRunning, "", 1},
		{"htu_root_1", "htu_run_c_1", message.StatusDenied, "", 2},
		{"htu_root_1", "htu_run_c_2", message.StatusRunning, "", 2},
	}
	if !reflect.DeepEqual(park, wantPark) {
		t.Errorf("park statuses:\n got %+v\nwant %+v", park, wantPark)
	}
	if !reflect.DeepEqual(resume, wantResume) {
		t.Errorf("resume statuses:\n got %+v\nwant %+v", resume, wantResume)
	}
	m := message.Fold(stream, 1)
	byID := map[string]string{}
	for _, c := range m.Content[0].Lane.Content {
		if c.Status != nil {
			byID[c.ID] = c.Status.Status
		}
	}
	want := map[string]string{"htu_run_c_1": message.StatusDenied, "htu_run_c_2": message.StatusDone, "htu_run_c_3": message.StatusDone}
	if !reflect.DeepEqual(byID, want) {
		t.Errorf("folded lane statuses = %v, want %v", byID, want)
	}
}

// TestLineage: a called run nests under its call whether its lineage names
// the call by the provider's id or by the minted one.
func TestLineage(t *testing.T) {
	m := message.Fold(project(lineage(), nil), 1)
	want := map[string]string{"htu_root_1": "run_p", "htu_root_2": "run_h"}
	got := map[string]string{}
	for _, c := range m.Content {
		if c.Type == message.BlockServerToolUse && c.Lane != nil {
			got[c.ID] = c.Lane.AgentRunID
			if len(c.Lane.Content) != 1 || !c.Lane.Done {
				t.Errorf("lane under %s: %+v", c.ID, c.Lane)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lanes by call = %v, want %v", got, want)
	}
}

func TestFoldDepth(t *testing.T) {
	stream := project(grandchildApproval(), nil)
	lanes := func(m message.Message) int {
		var count func([]message.Content) int
		count = func(cs []message.Content) int {
			n := 0
			for _, c := range cs {
				if c.Lane != nil {
					n += 1 + count(c.Lane.Content)
				}
			}
			return n
		}
		return count(m.Content)
	}
	for _, tt := range []struct{ depth, want int }{{0, 0}, {1, 1}, {2, 2}, {-1, 2}} {
		if got := lanes(message.Fold(stream, tt.depth)); got != tt.want {
			t.Errorf("depth %d: %d lanes, want %d", tt.depth, got, tt.want)
		}
	}
}

// TestFoldResumedBlock: a block sent again from its start replaces it.
func TestFoldResumedBlock(t *testing.T) {
	events := []message.Event{
		message.MessageStart{Message: message.StartedMessage{ID: "m", Type: "message", Role: "assistant", Content: []message.Block{}}},
		message.ContentBlockStart{Index: 0, ContentBlock: message.Block{Type: message.BlockText}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "text_delta", Text: "Hel"}},
		message.ContentBlockStart{Index: 0, ContentBlock: message.Block{Type: message.BlockText}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "text_delta", Text: "Hello"}},
		message.ContentBlockStop{Index: 0},
		message.ContentBlockStart{Index: 1, ContentBlock: message.Block{Type: message.BlockServerToolUse, ID: "t", Input: args(`{}`)}},
		message.ContentBlockDelta{Index: 1, Delta: message.BlockDelta{Type: "input_json_delta", PartialJSON: `{"a":`}},
		message.ContentBlockDelta{Index: 1, Delta: message.BlockDelta{Type: "input_json_delta", PartialJSON: `1}`}},
		message.ContentBlockStop{Index: 1},
		message.MessageStop{},
	}
	m := message.Fold(events, 0)
	if len(m.Content) != 2 || m.Content[0].Text != "Hello" || string(m.Content[1].Input) != `{"a":1}` || !m.Done {
		t.Fatalf("folded %+v", m)
	}
}

func TestEventRoundTrip(t *testing.T) {
	q := &message.Question{ID: "q", Kind: "approval", Prompt: "ok?", Choices: []string{"a"}, Who: []string{"alice"}}
	cite := message.Citation{Type: "search_result_location", Source: "s", Title: "t", CitedText: "c", Index: 2}
	events := []message.Event{
		message.MessageStart{Message: message.StartedMessage{ID: "m", Type: "message", Role: "assistant", Content: []message.Block{},
			Humagen: &message.Head{RequestID: "r", RootRun: "root", Agent: "a", Ref: "ref", TraceURL: "https://t"}}},
		message.ContentBlockStart{Index: 0, ContentBlock: message.Block{Type: message.BlockText, Text: "", Citations: []message.Citation{cite}}},
		message.ContentBlockStart{Index: 1, ContentBlock: message.Block{Type: message.BlockServerToolUse, ID: "htu_root_1", Name: "agent",
			Input: args(`{"title":"x"}`), Kind: message.KindAgent, Title: "x"}},
		message.ContentBlockStart{Index: 2, ContentBlock: message.Block{Type: message.BlockServerToolResult, ToolUseID: "htu_root_1", IsError: true,
			Content: []message.ResultContent{{Type: "text", Text: "r"}, {Type: "search_result", Source: "s", Title: "t",
				Content: []message.TextItem{{Type: "text", Text: "c"}}, Citations: &message.Enabled{Enabled: true}}}}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "text_delta", Text: "hi"}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "thinking_delta", Thinking: "hm"}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "input_json_delta", PartialJSON: "{"}},
		message.ContentBlockDelta{Index: 0, Delta: message.BlockDelta{Type: "citations_delta", Citation: &cite}},
		message.ContentBlockStop{Index: 3},
		message.MessageDelta{Delta: message.StopDelta{StopReason: message.StopEndTurn},
			Usage:   &message.Usage{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3, CacheCreationInputTokens: 4},
			Humagen: &message.TreeUsage{Usage: message.ModelUsage{ByModel: map[string]message.Usage{"m": {InputTokens: 1}}, CostUSD: 0.25}}},
		message.MessageStop{},
		message.Ping{},
		message.Error{Error: message.ErrorBody{Type: "overloaded_error", Message: "busy"}},
		message.ToolStatus{ToolUseID: "t", Path: []string{"a", "t"}, Status: message.StatusAwaitingApproval, Title: "x", Question: q, ElapsedMs: 5},
		message.Progress{ParentToolUseID: "t", Text: "x: running"},
		message.Nested{ParentToolUseID: "t", AgentRunID: "r", Depth: 1, Event: message.ContentBlockStop{Index: 0}},
		message.Nested{ParentToolUseID: "t", AgentRunID: "r", Depth: 1, Event: message.MessageStop{}},
		message.Nested{ParentToolUseID: "t", AgentRunID: "r", Depth: 2,
			Event: message.Nested{ParentToolUseID: "u", AgentRunID: "g", Depth: 2, Event: message.ToolStatus{ToolUseID: "v", Path: []string{"t", "u", "v"}, Status: "running"}}},
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.EventType()] = true
		b, err := message.MarshalEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &head); err != nil || head.Type != e.EventType() {
			t.Fatalf("%s: wire type %q, err %v", b, head.Type, err)
		}
		if !bytes.HasPrefix(b, []byte(`{"type":`)) {
			t.Fatalf("%s: type is not first", b)
		}
		back, err := message.Decode(b)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		if !reflect.DeepEqual(back, e) {
			t.Fatalf("round trip of %s:\n got %#v\nwant %#v", b, back, e)
		}
	}
	for _, typ := range []string{message.TypeMessageStart, message.TypeContentBlockStart, message.TypeContentBlockDelta,
		message.TypeContentBlockStop, message.TypeMessageDelta, message.TypeMessageStop, message.TypePing, message.TypeError,
		message.TypeNested, message.TypeToolStatus, message.TypeProgress} {
		if !seen[typ] {
			t.Errorf("no round trip of %s", typ)
		}
	}
	if _, err := message.Decode(json.RawMessage(`{"type":"nope"}`)); err == nil {
		t.Error("an unknown type decoded")
	}
}

// TestStreamRoundTrip: every projected event survives its wire form.
func TestStreamRoundTrip(t *testing.T) {
	for _, sc := range scenarios {
		stream := project(sc.envs(), nil)
		for _, e := range stream {
			b, err := message.MarshalEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			back, err := message.Decode(b)
			if err != nil {
				t.Fatalf("%s: %s: %v", sc.name, b, err)
			}
			again, _ := message.MarshalEvent(back)
			if !bytes.Equal(b, again) {
				t.Fatalf("%s: %s came back as %s", sc.name, b, again)
			}
		}
	}
}

func TestWithin(t *testing.T) {
	nested := func(d int) message.Event { return message.Nested{Depth: d, Event: message.Ping{}} }
	tests := []struct {
		e     message.Event
		depth int
		want  bool
	}{
		{message.MessageStop{}, 0, true},
		{message.ToolStatus{}, 0, true},
		{message.Progress{}, 0, true},
		{nested(1), 0, false},
		{nested(1), 1, true},
		{nested(2), 1, false},
		{nested(2), 2, true},
		{nested(3), -1, true},
	}
	for _, tt := range tests {
		if got := message.Within(tt.e, tt.depth); got != tt.want {
			t.Errorf("Within(%+v, %d) = %v, want %v", tt.e, tt.depth, got, tt.want)
		}
	}
}

func TestOptions(t *testing.T) {
	c := run{id: "run_c", agent: "helper", parentRun: rootRun, parentCall: "call-root-1"}
	envs := newTree().
		on(root, event.Accepted{}, executing("toolu_1", "ask", `{}`)).
		on(c, event.Accepted{}, event.Completed{Text: "ok"}).
		on(root, event.ToolDone{ID: "toolu_1", Name: "ask", Result: "ok"}, event.Completed{}).envs
	p := message.NewProjector(head,
		message.WithToolUseID(func(run string, n int) string { return fmt.Sprintf("call-%s-%d", run, n) }),
		message.WithClassify(func(name string, _ json.RawMessage) (string, string) { return message.KindAgent, "Ask " + name }),
		message.WithBackground(func(string) bool { return false }))
	var stream []message.Event
	for _, e := range envs {
		stream = append(stream, p.Add(e)...)
	}
	m := message.Fold(stream, -1)
	use := m.Content[0]
	if use.ID != "call-root-1" || use.Kind != message.KindAgent || use.Title != "Ask ask" || use.Lane == nil || use.Lane.AgentRunID != "run_c" {
		t.Fatalf("options not applied: %+v", use)
	}
}

// checkWellFormed checks each message of a stream: it starts with
// message_start, its blocks are numbered from zero and open and close in
// turn, and nothing follows its message_stop.
func checkWellFormed(t *testing.T, stream []message.Event, open ...string) {
	t.Helper()
	type state struct {
		started, stopped bool
		next, open       int
	}
	lanes := map[string]*state{}
	for i, e := range stream {
		key := ""
		if n, ok := e.(message.Nested); ok {
			key, e = n.ParentToolUseID, n.Event
		}
		s := lanes[key]
		if s == nil {
			s = &state{open: -1}
			lanes[key] = s
		}
		fail := func(why string) { t.Fatalf("event %d (%s) of lane %q: %s", i, e.EventType(), key, why) }
		if s.stopped {
			fail("after message_stop")
		}
		switch v := e.(type) {
		case message.MessageStart:
			if s.started {
				fail("second message_start")
			}
			s.started = true
		case message.ContentBlockStart:
			if v.Index != s.next || s.open >= 0 {
				fail(fmt.Sprintf("block %d starts with %d next and %d open", v.Index, s.next, s.open))
			}
			s.open, s.next = v.Index, s.next+1
		case message.ContentBlockDelta:
			if v.Index != s.open {
				fail("delta to a block that is not open")
			}
		case message.ContentBlockStop:
			if v.Index != s.open {
				fail("stop of a block that is not open")
			}
			s.open = -1
		case message.MessageDelta:
			if s.open >= 0 {
				fail("message_delta with a block open")
			}
		case message.MessageStop:
			s.stopped = true
		case message.ToolStatus, message.Progress:
			continue
		}
		if !s.started {
			fail("before message_start")
		}
	}
	if !lanes[""].stopped {
		t.Fatal("the root message did not stop")
	}
	keys := make([]string, 0, len(lanes))
	for k := range lanes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if s := lanes[k]; !s.stopped && !slices.Contains(open, k) {
			t.Errorf("lane %q did not stop", k)
		}
	}
}
