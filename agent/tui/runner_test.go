package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func bufioScanner(s string) *bufio.Scanner { return bufio.NewScanner(strings.NewReader(s)) }

// gatedProvider streams one scripted response per call. Call hold (0-based)
// waits for release, or for cancellation, before streaming anything, which
// keeps a run active while a test types into the TUI.
type gatedProvider struct {
	mu        sync.Mutex
	responses [][]types.Delta
	errs      []error
	hold      int
	release   chan struct{}
	calls     [][]types.Message
	// called, when set, receives once per provider call, after the call is
	// recorded, so a test can wait for a call instead of for time to pass.
	called chan struct{}
}

func (p *gatedProvider) ChatStream(ctx context.Context, msgs []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.mu.Lock()
	n := len(p.calls)
	p.calls = append(p.calls, msgs)
	p.mu.Unlock()
	if p.called != nil {
		p.called <- struct{}{}
	}
	if n < len(p.errs) && p.errs[n] != nil {
		return nil, p.errs[n]
	}
	var script []types.Delta
	if n < len(p.responses) {
		script = p.responses[n]
	} else {
		script = agenttest.TextResponse("extra")
	}
	ch := make(chan types.Delta)
	go func() {
		defer close(ch)
		if n == p.hold && p.release != nil {
			select {
			case <-p.release:
			case <-ctx.Done():
				return
			}
		}
		for _, d := range script {
			select {
			case ch <- d:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (p *gatedProvider) requests() [][]types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]types.Message(nil), p.calls...)
}

// userTexts returns the text of every user message in msgs.
func userTexts(msgs []types.Message) []string {
	var out []string
	for _, m := range msgs {
		um, ok := m.(types.UserMessage)
		if !ok {
			continue
		}
		for _, c := range um.Content {
			if tc, ok := c.(types.TextContent); ok {
				out = append(out, tc.Text)
			}
		}
	}
	return out
}

// loop is a minimal bubbletea event loop for tests: it runs commands on
// goroutines and feeds stream and submission messages back into the model.
// Timer messages (spinner ticks, cursor blinks) are dropped.
type loop struct {
	t    *testing.T
	m    runnerModel
	msgs chan tea.Msg
	quit bool
}

func newLoop(t *testing.T, a *agentsdk.Agent) *loop {
	m := newRunnerModel(a, context.Background(), TemplateMinimal)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return &loop{t: t, m: model.(runnerModel), msgs: make(chan tea.Msg, 256)}
}

func (l *loop) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { l.msgs <- cmd() }()
}

func (l *loop) send(msg tea.Msg) {
	model, cmd := l.m.Update(msg)
	l.m = model.(runnerModel)
	l.run(cmd)
}

func (l *loop) typeText(s string) {
	for _, r := range s {
		l.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (l *loop) key(k tea.KeyType) { l.send(tea.KeyMsg{Type: k}) }

// until processes messages until cond holds.
func (l *loop) until(desc string, cond func(runnerModel) bool) {
	l.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond(l.m) {
		select {
		case msg := <-l.msgs:
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					l.run(c)
				}
			case tea.QuitMsg:
				l.quit = true
			case deltaMsg, streamDoneMsg, submitResultMsg:
				l.send(msg)
			}
		case <-deadline:
			l.t.Fatalf("timed out waiting for %s; entries: %+v", desc, l.m.act.entries)
		}
	}
}

// untilSignal processes messages until ch receives.
func (l *loop) untilSignal(desc string, ch <-chan struct{}) {
	l.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-ch:
			return
		case msg := <-l.msgs:
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					l.run(c)
				}
			case tea.QuitMsg:
				l.quit = true
			case deltaMsg, streamDoneMsg, submitResultMsg:
				l.send(msg)
			}
		case <-deadline:
			l.t.Fatalf("timed out waiting for %s; entries: %+v", desc, l.m.act.entries)
		}
	}
}

func idle(m runnerModel) bool { return !m.running() }

func entriesOf(m runnerModel, kind activityKind) []activityEntry {
	var out []activityEntry
	for _, e := range m.act.entries {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestRunnerErrorEndsTurnNotSession(t *testing.T) {
	p := &gatedProvider{
		errs:      []error{errors.New("provider exploded")},
		responses: [][]types.Delta{nil, agenttest.TextResponse("second answer")},
		hold:      -1,
	}
	l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))

	l.typeText("first")
	l.key(tea.KeyEnter)
	l.until("first run to end", idle)
	if l.quit {
		t.Fatal("an error ended the session")
	}
	if errs := entriesOf(l.m, activityError); len(errs) != 1 || !strings.Contains(errs[0].text, "provider exploded") {
		t.Fatalf("error entries = %+v", errs)
	}

	l.typeText("second")
	l.key(tea.KeyEnter)
	l.until("second run to end", idle)
	if !strings.Contains(l.m.act.text(), "second answer") {
		t.Fatalf("second turn missing; transcript %q", l.m.act.text())
	}
	if users := entriesOf(l.m, activityUser); len(users) != 2 {
		t.Fatalf("earlier turn lost: %d user entries, want 2", len(users))
	}
}

func TestRunnerStopKeys(t *testing.T) {
	tests := []struct {
		name     string
		keys     []tea.KeyType
		wantQuit bool
	}{
		{"esc stops the run and keeps the session", []tea.KeyType{tea.KeyEsc}, false},
		{"one ctrl+c stops the run and keeps the session", []tea.KeyType{tea.KeyCtrlC}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &gatedProvider{responses: [][]types.Delta{agenttest.TextResponse("never")}, release: make(chan struct{})}
			l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))

			l.typeText("hello")
			l.key(tea.KeyEnter)
			if !l.m.running() {
				t.Fatal("run did not start")
			}
			if !l.m.textInput.Focused() {
				t.Fatal("input lost focus while streaming")
			}
			for _, k := range tt.keys {
				l.key(k)
			}
			l.until("run to stop", idle)
			if l.quit != tt.wantQuit {
				t.Fatalf("quit = %v, want %v", l.quit, tt.wantQuit)
			}
			stopped := entriesOf(l.m, activityStopped)
			if len(stopped) != 1 || stopped[0].text != stoppedTag {
				t.Fatalf("stopped entries = %+v, want the stopped tag", stopped)
			}
			if errs := entriesOf(l.m, activityError); len(errs) != 0 {
				t.Fatalf("a stop was reported as an error: %+v", errs)
			}
			if l.m.phase != phaseInput {
				t.Fatalf("phase = %v, want input", l.m.phase)
			}
		})
	}
}

func TestRunnerDoubleCtrlCQuits(t *testing.T) {
	tests := []struct {
		name       string
		keys       []tea.KeyMsg
		wantQuit   bool
		wantNotice string
	}{
		{"two presses quit", []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyCtrlC}}, true, quitHint},
		{"another key in between disarms", []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyRunes, Runes: []rune{'x'}}, {Type: tea.KeyCtrlC}}, false, quitHint},
		{"one press does not quit", []tea.KeyMsg{{Type: tea.KeyCtrlC}}, false, quitHint},
		{"another key clears the quit hint", []tea.KeyMsg{{Type: tea.KeyCtrlC}, {Type: tea.KeyRunes, Runes: []rune{'x'}}}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: &gatedProvider{hold: -1}})
			var m tea.Model = newRunnerModel(a, context.Background(), TemplateMinimal)
			quit := false
			for _, k := range tt.keys {
				var cmd tea.Cmd
				m, cmd = m.Update(k)
				if cmd != nil {
					if _, ok := cmd().(tea.QuitMsg); ok {
						quit = true
					}
				}
			}
			if quit != tt.wantQuit {
				t.Fatalf("quit = %v, want %v", quit, tt.wantQuit)
			}
			if got := m.(runnerModel).notice; got != tt.wantNotice {
				t.Fatalf("notice = %q, want %q", got, tt.wantNotice)
			}
		})
	}
}

func TestRunnerQueueAndSteer(t *testing.T) {
	tests := []struct {
		name      string
		key       tea.KeyMsg
		wantSteer bool
		wantLabel string
	}{
		{"enter queues", tea.KeyMsg{Type: tea.KeyEnter}, false, "queued"},
		{"ctrl+j steers", tea.KeyMsg{Type: tea.KeyCtrlJ}, true, "steer"},
		{"alt+enter steers", tea.KeyMsg{Type: tea.KeyEnter, Alt: true}, true, "steer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &gatedProvider{
				responses: [][]types.Delta{agenttest.TextResponse("first answer"), agenttest.TextResponse("second answer")},
				release:   make(chan struct{}),
				called:    make(chan struct{}, 4),
			}
			l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))

			l.typeText("first")
			l.key(tea.KeyEnter)
			// A steer submitted before the run's first safe point joins the
			// first call; wait until that call is in flight so the steer
			// lands after it and needs a second call.
			l.untilSignal("first provider call", p.called)
			l.typeText("follow up")
			l.send(tt.key)
			if len(l.m.pending) != 1 {
				t.Fatalf("pending = %+v, want the typed message", l.m.pending)
			}
			if footer := l.m.footerView(); !strings.Contains(footer, tt.wantLabel+": follow up") {
				t.Fatalf("queue strip missing from footer:\n%s", footer)
			}
			l.until("submission accepted", func(m runnerModel) bool {
				return len(m.pending) == 1 && m.pending[0].id != ""
			})

			close(p.release)
			l.until("run to finish", idle)

			if len(l.m.pending) != 0 {
				t.Fatalf("queue strip not cleared: %+v", l.m.pending)
			}
			users := entriesOf(l.m, activityUser)
			if len(users) != 2 || users[1].text != "follow up" || users[1].steer != tt.wantSteer {
				t.Fatalf("user entries = %+v", users)
			}
			reqs := p.requests()
			if len(reqs) != 2 {
				t.Fatalf("provider calls = %d, want 2", len(reqs))
			}
			if got := userTexts(reqs[1]); len(got) != 2 || got[1] != "follow up" {
				t.Fatalf("second call user messages = %q", got)
			}
		})
	}
}

func TestRunnerQueuedMessagesReturnToInputOnStop(t *testing.T) {
	p := &gatedProvider{responses: [][]types.Delta{agenttest.TextResponse("never")}, release: make(chan struct{}), called: make(chan struct{}, 4)}
	l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))

	l.typeText("first")
	l.key(tea.KeyEnter)
	// Stop only once the first call is in flight, so the test checks that
	// the queue is not sent, not whether the call started before the stop.
	l.untilSignal("first provider call", p.called)
	l.typeText("later")
	l.key(tea.KeyEnter)
	l.until("submission accepted", func(m runnerModel) bool {
		return len(m.pending) == 1 && m.pending[0].id != ""
	})
	l.key(tea.KeyEsc)
	l.until("run to stop", idle)

	if len(l.m.pending) != 0 {
		t.Fatalf("pending = %+v, want none after a stop", l.m.pending)
	}
	if got := l.m.textInput.Value(); got != "later" {
		t.Fatalf("input = %q, want the queued message back", got)
	}
	if n := len(p.requests()); n != 1 {
		t.Fatalf("provider calls = %d, want 1: a stopped run must not send the queue", n)
	}
}

func TestRunnerIgnoresStaleStreams(t *testing.T) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: &gatedProvider{hold: -1}})
	m := newRunnerModel(a, context.Background(), TemplateMinimal)
	deltas := make(chan types.Delta)
	defer close(deltas)
	m.stream = agentsdk.NewRemoteStream(deltas, nil, nil, nil)
	m.deltaCh = m.stream.Deltas()
	m.gen = 2
	m.phase = phaseStreaming

	model, cmd := m.Update(deltaMsg{gen: 1, delta: types.TextContentDelta{Content: "old"}})
	m = model.(runnerModel)
	if cmd != nil || len(m.act.entries) != 0 {
		t.Fatal("a delta from an earlier stream was applied")
	}
	model, _ = m.Update(streamDoneMsg{gen: 1})
	if !model.(runnerModel).running() {
		t.Fatal("an earlier stream's close ended the current run")
	}
}

func TestRunnerMarkerPromptShowsArguments(t *testing.T) {
	a, _ := markedAgent()
	m := newRunnerModel(a, context.Background(), TemplateMinimal)
	deltas := make(chan types.Delta)
	defer close(deltas)
	m.stream = agentsdk.NewRemoteStream(deltas, nil, nil, nil)
	m.deltaCh = m.stream.Deltas()
	model, _ := m.handleDelta(types.MarkerDelta{
		ToolCallID: "c1",
		ToolName:   "rag_delete",
		Arguments:  map[string]any{"uuid": "doc-42"},
		Markers:    []types.Marker{{Kind: "human_approval", Message: "destructive"}},
	})
	footer := model.(runnerModel).footerView()
	for _, w := range []string{"rag_delete", "destructive", `"uuid": "doc-42"`} {
		if !strings.Contains(footer, w) {
			t.Fatalf("approval prompt missing %q:\n%s", w, footer)
		}
	}
}

func TestRunnerShortTerminal(t *testing.T) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: &gatedProvider{hold: -1}})
	m := newRunnerModel(a, context.Background(), TemplateDefault)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 3})
	if h := model.(runnerModel).viewport.Height; h < 1 {
		t.Fatalf("viewport height = %d on a short terminal", h)
	}
	_ = model.View()
}

func TestRunnerScrollKeysMoveViewport(t *testing.T) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: &gatedProvider{hold: -1}})
	m := newRunnerModel(a, context.Background(), TemplateMinimal)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m = model.(runnerModel)
	for range 50 {
		m.act.addUser("line", false)
	}
	m.refresh()
	if !m.viewport.AtBottom() {
		t.Fatal("transcript does not follow new output")
	}
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = model.(runnerModel)
	if m.viewport.AtBottom() {
		t.Fatal("pgup did not scroll the transcript")
	}
	m.act.addUser("new", false)
	m.refresh()
	if m.viewport.AtBottom() {
		t.Fatal("new output yanked the view back while scrolled up")
	}
}

func TestRunnerJSONOutputUsesLineMode(t *testing.T) {
	p := &gatedProvider{responses: [][]types.Delta{agenttest.TextResponse("json answer")}, hold: -1}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p})
	var out, errOut bytes.Buffer
	r := &Runner{In: strings.NewReader("hi\n/quit\n"), Out: &out, Output: NewJSONOutput(&out, &errOut)}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Run(ctx, a); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var text strings.Builder
	for _, line := range lines {
		d, err := types.UnmarshalDelta([]byte(line))
		if err != nil {
			t.Fatalf("stdout line %q is not a delta envelope: %v", line, err)
		}
		if tc, ok := d.(types.TextContentDelta); ok {
			text.WriteString(tc.Content)
		}
	}
	if text.String() != "json answer" {
		t.Fatalf("decoded text = %q", text.String())
	}
	if !strings.Contains(errOut.String(), ">>>") {
		t.Fatalf("prompt should go to stderr in JSON mode, got %q", errOut.String())
	}
}

// parallelMarkedAgent asks for two marked tool calls in one turn. Tools run
// concurrently, so both approval requests arrive before either is answered.
func parallelMarkedAgent() (*agentsdk.Agent, *atomic.Int32) {
	var calls atomic.Int32
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "danger", Description: "destructive"},
		Fn: func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "deleted", nil
		},
	}
	turn := append(agenttest.ToolCallResponse("c1", "danger", map[string]any{"n": 1}),
		agenttest.ToolCallResponse("c2", "danger", map[string]any{"n": 2})...)
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{turn, agenttest.TextResponse("finished")}}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:     "test",
		Provider: provider,
		Tools:    types.NewToolRegistry(types.WithMarkers(tool, types.Marker{Kind: "human_approval", Message: "needs approval"})),
	})
	return a, &calls
}

func TestRunnerParallelMarkersAreAnsweredInOrder(t *testing.T) {
	tests := []struct {
		name      string
		answers   []string
		wantCalls int32
	}{
		{"approve both", []string{"y", "y"}, 2},
		{"approve one, deny one", []string{"y", "n"}, 1},
		{"deny both", []string{"n", "n"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, calls := parallelMarkedAgent()
			l := newLoop(t, a)
			l.typeText("delete both")
			l.key(tea.KeyEnter)
			l.until("both approvals", func(m runnerModel) bool { return len(m.approvals) == 2 })
			if footer := l.m.footerView(); !strings.Contains(footer, "1 more waiting") {
				t.Fatalf("footer does not show the waiting approval:\n%s", footer)
			}

			first := l.m.approvals[0].toolCallID
			l.typeText(tt.answers[0])
			l.key(tea.KeyEnter)
			if len(l.m.approvals) != 1 || l.m.approvals[0].toolCallID == first || l.m.phase != phaseMarker {
				t.Fatalf("after one answer: approvals=%+v phase=%v", l.m.approvals, l.m.phase)
			}
			l.typeText(tt.answers[1])
			l.key(tea.KeyEnter)
			if len(l.m.approvals) != 0 || l.m.phase != phaseStreaming {
				t.Fatalf("after both answers: approvals=%+v phase=%v", l.m.approvals, l.m.phase)
			}

			l.until("run to finish", idle)
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("tool calls = %d, want %d", got, tt.wantCalls)
			}
			if !strings.Contains(l.m.act.text(), "finished") {
				t.Fatalf("run did not complete; transcript %q", l.m.act.text())
			}
		})
	}
}

func TestRunnerStopClearsAllApprovals(t *testing.T) {
	a, calls := parallelMarkedAgent()
	l := newLoop(t, a)
	l.typeText("delete both")
	l.key(tea.KeyEnter)
	l.until("both approvals", func(m runnerModel) bool { return len(m.approvals) == 2 })
	l.key(tea.KeyEsc)
	if len(l.m.approvals) != 0 || l.m.phase != phaseStreaming {
		t.Fatalf("stop left approvals=%+v phase=%v", l.m.approvals, l.m.phase)
	}
	l.until("run to stop", idle)
	if got := calls.Load(); got != 0 {
		t.Fatalf("tool calls = %d after a stop, want 0", got)
	}
}

// A message sent just before Esc can have its Submit result arrive after the
// stopped run closed. It must go back to the input, not start a new run.
func TestRunnerLateSubmitResultAfterStopReturnsToInput(t *testing.T) {
	tests := []struct {
		name   string
		result func(gen, local int) submitResultMsg
	}{
		{"run finished", func(gen, local int) submitResultMsg {
			return submitResultMsg{gen: gen, local: local, err: agentsdk.ErrRunFinished}
		}},
		{"accepted but never appended", func(gen, local int) submitResultMsg {
			return submitResultMsg{gen: gen, local: local, id: "s-late"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &gatedProvider{responses: [][]types.Delta{agenttest.TextResponse("never")}, release: make(chan struct{})}
			l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))

			l.typeText("first")
			l.key(tea.KeyEnter)
			l.typeText("later")
			// Hold back the Submit command so its result lands after the stop.
			model, _ := l.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			l.m = model.(runnerModel)
			if len(l.m.pending) != 1 || l.m.pending[0].state != subSending {
				t.Fatalf("pending = %+v, want one message in flight", l.m.pending)
			}
			gen, local := l.m.gen, l.m.pending[0].local

			l.key(tea.KeyEsc)
			l.until("run to stop", idle)
			l.send(tt.result(gen, local))

			if l.m.running() {
				t.Fatal("a stopped run's late message started a new run")
			}
			if len(l.m.pending) != 0 {
				t.Fatalf("pending = %+v, want none", l.m.pending)
			}
			if got := l.m.textInput.Value(); got != "later" {
				t.Fatalf("input = %q, want the message back", got)
			}
			// The stop can land before the first call reaches the provider.
			if n := len(p.requests()); n > 1 {
				t.Fatalf("provider calls = %d, want at most 1", n)
			}
		})
	}
}

func TestRunnerContinue(t *testing.T) {
	tests := []struct {
		name string
		// turns are completed before /continue is typed.
		turns     []string
		holdAt    int // provider call that waits for release; -1 for none
		whileRun  bool
		wantStart bool
		wantError bool
		wantNote  bool
	}{
		{name: "fresh agent reports an error and stays usable", holdAt: -1, wantError: true},
		{name: "after a turn attaches a new stream", turns: []string{"hello"}, holdAt: 1, wantStart: true},
		{name: "during a run sets a notice and starts nothing", holdAt: 0, whileRun: true, wantNote: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &gatedProvider{hold: tt.holdAt, release: make(chan struct{})}
			l := newLoop(t, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p}))
			for _, turn := range tt.turns {
				l.typeText(turn)
				l.key(tea.KeyEnter)
				l.until("turn to finish", idle)
			}
			if tt.whileRun {
				l.typeText("hello")
				l.key(tea.KeyEnter)
			}
			gen := l.m.gen

			l.typeText("/continue")
			l.key(tea.KeyEnter)

			if started := l.m.gen != gen; started != tt.wantStart {
				t.Fatalf("new stream = %v, want %v", started, tt.wantStart)
			}
			if tt.wantStart {
				if !l.m.running() {
					t.Fatal("continue did not start a run")
				}
				if notes := entriesOf(l.m, activityStopped); len(notes) == 0 || notes[len(notes)-1].text != "continuing" {
					t.Fatalf("stopped entries = %+v, want a continuing note", notes)
				}
			}
			errs := entriesOf(l.m, activityError)
			if (len(errs) > 0) != tt.wantError {
				t.Fatalf("error entries = %+v, want error %v", errs, tt.wantError)
			}
			if tt.wantError {
				if !strings.Contains(errs[0].text, "cannot continue") || l.m.phase != phaseInput || l.m.running() {
					t.Fatalf("error=%q phase=%v running=%v", errs[0].text, l.m.phase, l.m.running())
				}
			}
			if (l.m.notice != "") != tt.wantNote {
				t.Fatalf("notice = %q, want notice %v", l.m.notice, tt.wantNote)
			}

			close(p.release)
			l.until("run to finish", idle)
			if l.quit {
				t.Fatal("continue ended the session")
			}
		})
	}
}

func TestRunVerboseContinue(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCalls int
		wantOut   string
	}{
		{"fresh agent prints an error and keeps reading", "/continue\n/quit\n", 0, "assistant turn"},
		{"after a turn resumes it", "hi\n/continue\n/quit\n", 2, "second"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &gatedProvider{responses: [][]types.Delta{agenttest.TextResponse("first"), agenttest.TextResponse("second")}, hold: -1}
			var out bytes.Buffer
			r := &Runner{Verbose: true, Template: TemplateMinimal, In: strings.NewReader(tt.input), Out: &out}
			r.Output = NewStyledOutput(&out, &out, TemplateMinimal)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.Run(ctx, agentsdk.NewAgent(agentsdk.AgentConfig{Name: "t", Provider: p})); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if n := len(p.requests()); n != tt.wantCalls {
				t.Fatalf("provider calls = %d, want %d", n, tt.wantCalls)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("output missing %q:\n%s", tt.wantOut, out.String())
			}
		})
	}
}
