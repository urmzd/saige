package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
)

// Runner is a multi-turn interactive TUI runner that implements agentsdk.Runner.
// It reads user input, invokes the agent, renders streaming deltas, resolves
// markers, and loops until the user types /quit or presses ctrl+c twice.
//
// While a run streams, the input stays focused: Enter queues the message for
// when the run would finish, Ctrl-J (what most terminals send for Ctrl-Enter)
// or Alt-Enter steers the run at its next safe point, and Esc stops the run
// without leaving the session. /continue resumes a stopped or cut-off turn.
//
// The transcript scrolls with PgUp/PgDn (a page), Shift-Up/Shift-Down (half
// a page), Up/Down (a line), Ctrl-Home/Ctrl-End (top and bottom) and the
// mouse wheel; with an empty input, Home/End and Ctrl-U/Ctrl-D work too. It
// follows new output until scrolled up, and again once back at the bottom.
// Ctrl-F filters the transcript by kind and text, and Ctrl-T expands
// reasoning.
type Runner struct {
	Title    string   // header title; empty uses the agent name
	Verbose  bool     // use plain text streaming instead of bubbletea
	Template Template // output template; zero value uses TemplateDefault
	// Output renders verbose mode. A *JSONOutput also selects the line mode,
	// since a full-screen TUI cannot write machine-readable output.
	Output Output

	// In and Out are the verbose-mode input and output. Nil means os.Stdin
	// and os.Stdout. Approval prompts are written to Out and answered on In.
	In  io.Reader
	Out io.Writer

	// NoAnimation turns off the spinner, fade-in and smooth scrolling. They
	// are also off when NO_COLOR or SAIGE_REDUCED_MOTION is set (see
	// MotionEnabled).
	NoAnimation bool
}

func (r *Runner) in() io.Reader {
	if r.In != nil {
		return r.In
	}
	return os.Stdin
}

func (r *Runner) out() io.Writer {
	if r.Out != nil {
		return r.Out
	}
	return os.Stdout
}

// deniedByUser is the reason sent to the model when a user rejects a marker.
const deniedByUser = "denied by user"

// Name implements agentsdk.NamedRunner.
func (r *Runner) Name() string { return "tui" }

// Run implements agentsdk.Runner. It starts the interactive conversation loop.
func (r *Runner) Run(ctx context.Context, agent *agentsdk.Agent) error {
	if r.Template.Name == "" {
		r.Template = TemplateDefault
	}
	if r.Output == nil {
		r.Output = NewStyledOutput(r.out(), os.Stderr, r.Template)
	}
	if _, isJSON := r.Output.(*JSONOutput); r.Verbose || isJSON {
		return r.runVerbose(ctx, agent)
	}
	return r.runInteractive(ctx, agent)
}

// ── Verbose mode ─────────────────────────────────────────────────────

func (r *Runner) runVerbose(ctx context.Context, agent *agentsdk.Agent) error {
	w := r.out()
	// JSON output keeps stdout machine-readable: prompts go to its error
	// writer instead.
	promptW := w
	jo, isJSON := r.Output.(*JSONOutput)
	if isJSON && jo.Err != nil {
		promptW = jo.Err
	}
	scanner := bufio.NewScanner(r.in())
	// A styled renderer that shows markers has already printed the
	// approval header and the marker details when the prompt runs, so the
	// prompt asks only its question. Other outputs (JSON on stdout) leave
	// the header to the prompt.
	prompt := PromptApproval
	if so, ok := r.Output.(*StyledOutput); ok && so.Template.ShowMarkers {
		prompt = askApproval
	}

	info := agent.Info()
	r.Output.Header(OutputHeader{
		Operation: info.Name,
		Provider:  info.Provider,
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, _ = fmt.Fprint(promptW, promptStyle.Render(">>> "))
		if !scanner.Scan() {
			return scanner.Err()
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "/quit" || input == "/exit" {
			return nil
		}

		var stream *agentsdk.EventStream
		if input == "/continue" {
			var err error
			if stream, err = agent.Continue(ctx, ""); err != nil {
				r.Output.Error(err)
				continue
			}
		} else {
			stream = agent.Invoke(ctx, []types.Message{types.UserMsg(types.Text(input))})
		}

		// Markers are resolved inline by the renderer: Deltas() has a single
		// consumer, and the prompt shares the REPL scanner, so a second
		// goroutine reading either would race this loop.
		resolve := func(d types.MarkerDelta) {
			res := agentsdk.Resolution{Approved: prompt(scanner, promptW, d)}
			if !res.Approved {
				res.Message = deniedByUser
			}
			if err := stream.ResolveMarkerErr(d.ToolCallID, res); err != nil {
				r.Output.Status("approval not delivered: " + err.Error())
			}
		}

		// Pass empty header: already printed above via Output.Header
		result := StreamDeltasResolving(r.Output, AgentHeader{}, stream.Deltas(), resolve)
		if result.Err != nil {
			r.Output.Error(result.Err)
		} else if result.Text != "" && !isJSON {
			fmt.Fprintln(w)
		}
	}
}

// ── Interactive mode ─────────────────────────────────────────────────

func (r *Runner) runInteractive(ctx context.Context, agent *agentsdk.Agent) error {
	if r.Template.RenderMarkdown {
		markdownStyle() // detect the terminal background before the program owns input
	}
	m := newRunnerModel(agent, ctx, r.Template).withMotion(MotionEnabled(r.NoAnimation))
	if r.Title != "" {
		m.header.Name = r.Title
	}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	if rm, ok := finalModel.(runnerModel); ok && rm.stream != nil {
		// The program can end without a quit key (a cancelled context or a
		// failed terminal); never leave the run going in the background.
		rm.stream.Cancel()
	}
	if err != nil {
		return err
	}
	if rm, ok := finalModel.(runnerModel); ok && rm.err != nil {
		return rm.err
	}
	return nil
}

// ── Bubbletea model for interactive runner ───────────────────────────

type runnerPhase int

const (
	phaseInput     runnerPhase = iota // no run is active
	phaseStreaming                    // a run is streaming
	phaseMarker                       // a run waits for marker approval
)

// markerPending tracks a marker awaiting user resolution.
type markerPending struct {
	toolCallID string
	toolName   string
	args       map[string]any
	markers    []types.Marker
}

// submissionState is where a message typed during a run stands.
type submissionState int

const (
	subSending submissionState = iota // Submit has not returned yet
	subQueued                         // the run accepted it
	subWaiting                        // the run ended first; it starts the next run
	subReturn                         // the run was stopped first; it goes back to the input
)

// pendingSubmission is a message typed while a run was active and not yet
// part of the conversation. It is shown in the queue strip until the run
// reports it injected.
type pendingSubmission struct {
	local int    // TUI-side ID, known before the run assigns one
	id    string // the run's SubmissionID once accepted
	text  string
	mode  agentsdk.SubmitMode
	state submissionState
}

// submitResultMsg reports the outcome of an asynchronous EventStream.Submit.
// Submit can wait while the delta buffer is full, and the buffer drains only
// through Update, so it never runs on the Update goroutine.
type submitResultMsg struct {
	gen   int
	local int
	id    agentsdk.SubmissionID
	err   error
}

type runnerModel struct {
	header    AgentHeader
	template  Template
	ctx       context.Context
	agent     *agentsdk.Agent
	phase     runnerPhase
	textInput textinput.Model
	spinner   spinner.Model
	scroll    scroller
	stream    *agentsdk.EventStream
	deltaCh   <-chan types.Delta
	gen       int // generation of the stream being read
	err       error
	approvals []markerPending // markers awaiting an answer, oldest first
	ready     bool            // viewport sized
	width     int
	height    int

	act activity

	pending   []pendingSubmission
	nextLocal int
	injected  map[string]bool // submissions injected before their Submit returned

	stopping  bool   // the user asked the active run to stop
	runFailed bool   // the active run ended with an error
	quitArmed bool   // one ctrl+c seen; a second one quits
	notice    string // one-line hint shown above the input

	// filter narrows the transcript; while filtering, keys edit it in
	// filterInput instead of the message input.
	filter      transcriptFilter
	filterInput textinput.Model
	filtering   bool
	// expandThinking shows reasoning in full.
	expandThinking bool
	// animate turns on the spinner, fade-in and smooth scrolling; clock
	// times the fade.
	animate bool
	clock   func() time.Time
}

const (
	idlePlaceholder    = "Type a message (/continue resumes, /quit exits)"
	runningPlaceholder = "Enter queues, Ctrl-J steers, Esc stops"
	stoppingNotice     = "Stopping..."
	stoppedTag         = "stopped (/continue resumes)"
	quitHint           = "Press Ctrl-C again to quit."
	stopQuitHint       = "Stopping. Press Ctrl-C again to quit."
	returnedNotice     = "Queued messages were returned to the input."

	// keyCtrlC is the key name bubbletea reports for Ctrl-C.
	keyCtrlC = "ctrl+c"
	// keyFilter opens the transcript filter.
	keyFilter = "ctrl+f"
	keyEsc    = "esc"
	keyEnter  = "enter"
)

func newRunnerModel(agent *agentsdk.Agent, ctx context.Context, tmpl Template) runnerModel {
	ti := textinput.New()
	ti.Placeholder = idlePlaceholder
	ti.Focus()
	ti.Width = 60

	info := agent.Info()
	header := AgentHeader{
		Name:      info.Name,
		Provider:  info.Provider,
		Tools:     info.Tools,
		SubAgents: info.SubAgents,
	}
	PopulateEnv(&header)
	if header.Name == "" {
		header.Name = "Agent"
	}

	return runnerModel{
		header:      header,
		template:    tmpl,
		ctx:         ctx,
		agent:       agent,
		phase:       phaseInput,
		textInput:   ti,
		spinner:     newSpinner(),
		scroll:      newScroller(false),
		act:         newActivity(info.SubAgents),
		injected:    make(map[string]bool),
		filterInput: newFilterInput(),
		clock:       time.Now,
	}
}

// withMotion returns m with animation on or off.
func (m runnerModel) withMotion(on bool) runnerModel {
	m.animate = on
	m.scroll.animate = on
	return m
}

// spin is the spinner frame for work in progress.
func (m runnerModel) spin() string {
	if !m.animate {
		return staticSpinner
	}
	return m.spinner.View()
}

func (m runnerModel) running() bool { return m.stream != nil }

// marker returns the approval being asked, or nil. Tools that run in
// parallel can each raise a marker, so they are answered one at a time in
// arrival order.
func (m runnerModel) marker() *markerPending {
	if len(m.approvals) == 0 {
		return nil
	}
	return &m.approvals[0]
}

func (m runnerModel) Init() tea.Cmd {
	if !m.animate {
		return textinput.Blink
	}
	return tea.Batch(textinput.Blink, m.spinner.Tick)
}

func (m runnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tea.MouseMsg:
		m.scroll.wheel(msg)
		return m, nil
	case scrollTickMsg:
		return m, m.scroll.tick()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.textInput.Width = max(msg.Width-4, 10)
		m.filterInput.Width = max(msg.Width-4, 10)
		m.ready = true
		m.refresh()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.running() || m.fading() {
			m.refresh()
		}
		return m, cmd
	case deltaMsg:
		if msg.gen != m.gen || !m.running() {
			return m, nil
		}
		return m.handleDelta(msg.delta)
	case streamDoneMsg:
		if msg.gen != m.gen || !m.running() {
			return m, nil
		}
		return m.finishTurn()
	case submitResultMsg:
		return m.handleSubmitResult(msg)
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m runnerModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key != keyCtrlC && m.quitArmed {
		// The hint is no longer true: the next ctrl+c only re-arms.
		m.quitArmed = false
		if m.notice == quitHint || m.notice == stopQuitHint {
			m.notice = ""
		}
	}

	if m.filtering && key != keyCtrlC {
		return m.handleFilterKey(msg)
	}
	if a := m.scrollKey(key); a != scrollNone {
		return m, m.scroll.do(a)
	}

	switch key {
	case keyFilter:
		m.filtering = true
		m.filterInput.SetValue(m.filter.raw)
		m.filterInput.CursorEnd()
		m.filterInput.Focus()
		m.textInput.Blur()
		m.refresh()
		return m, textinput.Blink
	case "ctrl+t":
		m.expandThinking = !m.expandThinking
		m.refresh()
		return m, nil
	case keyCtrlC:
		if m.quitArmed {
			m.cancelStream()
			return m, tea.Quit
		}
		m.quitArmed = true
		if m.running() {
			m.stop()
			m.notice = stopQuitHint
		} else {
			m.textInput.Reset()
			m.notice = quitHint
		}
		m.refresh()
		return m, nil

	case keyEsc:
		switch {
		case m.running() && !m.stopping:
			m.stop()
			m.refresh()
		case !m.running() && m.filter.active():
			m.filter = transcriptFilter{}
			m.refresh()
		}
		return m, nil

	case keyEnter:
		if m.phase == phaseMarker && m.marker() != nil {
			return m.answerMarker()
		}
		return m.submit(agentsdk.SubmitQueue)

	case "ctrl+j", "alt+enter":
		if m.phase == phaseMarker && m.marker() != nil {
			return m.answerMarker()
		}
		return m.submit(agentsdk.SubmitSteer)

	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

// scrollKey maps a key to a transcript scroll. Keys the input also uses
// (Home, End, Ctrl-U, Ctrl-D) scroll only while the input is empty, where
// they would do nothing.
func (m runnerModel) scrollKey(key string) scrollAction {
	switch key {
	case "pgup":
		return scrollPageUp
	case "pgdown":
		return scrollPageDown
	case "shift+up":
		return scrollHalfUp
	case "shift+down":
		return scrollHalfDown
	case "up":
		return scrollLineUp
	case "down":
		return scrollLineDown
	case "ctrl+home":
		return scrollTop
	case "ctrl+end":
		return scrollBottom
	}
	input := m.textInput.Value()
	if m.filtering {
		input = m.filterInput.Value()
	}
	if input != "" {
		return scrollNone
	}
	switch key {
	case "home":
		return scrollTop
	case "end":
		return scrollBottom
	case "ctrl+u":
		return scrollHalfUp
	case "ctrl+d":
		return scrollHalfDown
	}
	return scrollNone
}

// handleFilterKey edits the transcript filter. Enter or Ctrl-F keeps it and
// returns to the message input; Esc clears it.
func (m runnerModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key := msg.String(); key {
	case keyEnter, keyFilter:
		m.endFilter()
		return m, nil
	case keyEsc:
		m.filter = transcriptFilter{}
		m.endFilter()
		return m, nil
	default:
		if a := m.scrollKey(key); a != scrollNone {
			return m, m.scroll.do(a)
		}
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filter = parseFilter(m.filterInput.Value())
	m.refresh()
	return m, cmd
}

func (m *runnerModel) endFilter() {
	m.filtering = false
	m.filterInput.Blur()
	m.textInput.Focus()
	m.refresh()
}

// fading reports whether an entry is still fading in.
func (m runnerModel) fading() bool {
	if !m.animate {
		return false
	}
	now := m.clock()
	for i := len(m.act.entries) - 1; i >= 0; i-- {
		if age := now.Sub(m.act.entries[i].born); age >= 0 && age < fadeFor {
			return true
		}
	}
	return false
}

// answerMarker resolves the oldest pending marker from the input. Only y/yes
// and n/no count; anything else asks again.
func (m runnerModel) answerMarker() (tea.Model, tea.Cmd) {
	approved, valid := ParseApproval(m.textInput.Value())
	if !valid {
		// Never guess: an empty or unrecognized answer asks again.
		m.textInput.Reset()
		m.textInput.Placeholder = "Please answer y or n"
		return m, nil
	}
	head := m.approvals[0]
	res := agentsdk.Resolution{Approved: approved}
	if !approved {
		res.Message = deniedByUser
	}
	// A marker that expired or was already answered can no longer take the
	// decision; say so instead of dropping it silently.
	if err := m.stream.ResolveMarkerErr(head.toolCallID, res); err != nil {
		m.notice = "approval not delivered: " + err.Error()
	}
	m.approvals = append([]markerPending(nil), m.approvals[1:]...)
	m.textInput.Reset()
	if len(m.approvals) > 0 {
		m.textInput.Placeholder = strings.TrimSpace(approvalPrompt)
	} else {
		m.phase = phaseStreaming
		m.textInput.Placeholder = runningPlaceholder
	}
	m.refresh()
	return m, nil
}

// submit sends the typed message. With no active run it starts one; during
// a run it joins that run in mode.
func (m runnerModel) submit(mode agentsdk.SubmitMode) (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.textInput.Value())
	if text == "" {
		return m, nil
	}
	m.textInput.Reset()
	m.notice = ""

	switch text {
	case "/quit", "/exit":
		m.cancelStream()
		return m, tea.Quit
	case "/continue":
		if m.running() {
			m.notice = "A run is active; wait for it or press Esc to stop it."
			m.refresh()
			return m, nil
		}
		return m.continueRun()
	}

	if !m.running() {
		return m.startRun(text)
	}

	m.nextLocal++
	p := pendingSubmission{local: m.nextLocal, text: text, mode: mode, state: subSending}
	m.pending = append(m.pending, p)
	m.refresh()
	return m, submitCmd(m.stream, m.gen, p.local, text, mode)
}

func submitCmd(stream *agentsdk.EventStream, gen, local int, text string, mode agentsdk.SubmitMode) tea.Cmd {
	return func() tea.Msg {
		id, err := stream.Submit(types.UserMsg(types.Text(text)), mode)
		return submitResultMsg{gen: gen, local: local, id: id, err: err}
	}
}

func (m runnerModel) handleSubmitResult(msg submitResultMsg) (tea.Model, tea.Cmd) {
	i := m.pendingIndex(func(p pendingSubmission) bool { return p.local == msg.local })
	if i < 0 {
		return m, nil
	}
	p := &m.pending[i]
	if p.state == subReturn {
		// The user stopped the run this message was sent to. Unless the
		// run already took it, it goes back to the input; it never starts
		// a run on its own.
		if msg.err == nil && m.injected[string(msg.id)] {
			delete(m.injected, string(msg.id))
			m.injectPending(i)
		} else {
			text := p.text
			m.pending = append(m.pending[:i], m.pending[i+1:]...)
			m.returnToInput([]string{text})
		}
		m.refresh()
		return m, nil
	}
	switch {
	case msg.err == nil:
		p.id = string(msg.id)
		switch {
		case m.injected[p.id]:
			delete(m.injected, p.id)
			m.injectPending(i)
		case msg.gen != m.gen || !m.running():
			// The run ended after accepting the message but before
			// appending it, so it goes to the next run.
			p.state = subWaiting
		case p.state == subSending:
			p.state = subQueued
		}
	case errors.Is(msg.err, agentsdk.ErrRunFinished):
		p.state = subWaiting
	default:
		text := p.text
		m.pending = append(m.pending[:i], m.pending[i+1:]...)
		m.act.addNotice(activityError, "could not send message: "+msg.err.Error())
		if m.textInput.Value() == "" {
			m.textInput.SetValue(text)
		}
	}

	if !m.running() {
		return m.drainWaiting()
	}
	m.refresh()
	return m, nil
}

func (m runnerModel) pendingIndex(match func(pendingSubmission) bool) int {
	for i, p := range m.pending {
		if match(p) {
			return i
		}
	}
	return -1
}

// injectPending moves pending submission i into the transcript.
func (m *runnerModel) injectPending(i int) {
	p := m.pending[i]
	m.pending = append(m.pending[:i], m.pending[i+1:]...)
	m.act.addUser(p.text, p.mode == agentsdk.SubmitSteer)
}

// startRun records text and starts a new run with it.
func (m runnerModel) startRun(text string) (tea.Model, tea.Cmd) {
	m.act.addUser(text, false)
	stream := m.agent.Invoke(m.ctx, []types.Message{types.UserMsg(types.Text(text))})
	return m.attach(stream)
}

// continueRun resumes the last assistant turn.
func (m runnerModel) continueRun() (tea.Model, tea.Cmd) {
	stream, err := m.agent.Continue(m.ctx, "")
	if err != nil {
		m.act.addNotice(activityError, "cannot continue: "+err.Error())
		m.refresh()
		return m, nil
	}
	m.act.addNotice(activityStopped, "continuing")
	return m.attach(stream)
}

// attach starts reading stream as the active run.
func (m runnerModel) attach(stream *agentsdk.EventStream) (tea.Model, tea.Cmd) {
	m.gen++
	m.stream = stream
	m.deltaCh = stream.Deltas()
	m.phase = phaseStreaming
	m.stopping, m.runFailed = false, false
	m.textInput.Placeholder = runningPlaceholder
	m.refresh()
	return m, listenForDelta(m.gen, m.deltaCh)
}

// stop asks the active run to end. Its stream then reports the cancellation
// and closes, which ends the turn without leaving the session.
func (m *runnerModel) stop() {
	m.stopping = true
	if len(m.approvals) > 0 {
		m.approvals = nil
		m.phase = phaseStreaming
		m.textInput.Reset()
		m.textInput.Placeholder = runningPlaceholder
	}
	m.stream.Cancel()
}

func (m *runnerModel) cancelStream() {
	if m.stream != nil {
		m.stream.Cancel()
	}
}

func (m runnerModel) handleDelta(d types.Delta) (tea.Model, tea.Cmd) {
	m.act.apply(d)

	switch d := d.(type) {
	case types.MarkerDelta:
		if !m.stopping {
			m.approvals = append(m.approvals, markerPending{
				toolCallID: d.ToolCallID,
				toolName:   d.ToolName,
				args:       d.Arguments,
				markers:    d.Markers,
			})
			if m.phase != phaseMarker {
				m.phase = phaseMarker
				m.textInput.Placeholder = strings.TrimSpace(approvalPrompt)
			}
		}

	case types.QueuedDelta:
		if i := m.pendingIndex(func(p pendingSubmission) bool { return p.id == d.SubmissionID }); i >= 0 {
			m.pending[i].state = subQueued
		}

	case types.InjectedDelta:
		if i := m.pendingIndex(func(p pendingSubmission) bool { return p.id == d.SubmissionID }); i >= 0 {
			m.injectPending(i)
		} else {
			m.injected[d.SubmissionID] = true
		}

	case types.ErrorDelta:
		// One failed run ends the turn, not the session.
		if !isCancellation(d.Error) {
			m.runFailed = true
		}
	}

	m.refresh()
	return m, listenForDelta(m.gen, m.deltaCh)
}

// finishTurn runs when the active stream closes. Messages the run accepted
// but never appended start the next run, unless the user stopped this one or
// it failed: then they go back to the input for the user to decide.
func (m runnerModel) finishTurn() (tea.Model, tea.Cmd) {
	undelivered := make(map[string]bool)
	for _, s := range m.stream.Undelivered() {
		undelivered[string(s.ID)] = true
	}
	for i := range m.pending {
		if m.pending[i].state == subQueued || undelivered[m.pending[i].id] {
			m.pending[i].state = subWaiting
		}
	}

	interrupted := m.stopping || m.runFailed
	m.act.finish()
	if m.stopping {
		m.act.addNotice(activityStopped, stoppedTag)
	}
	m.phase = phaseInput
	m.stream = nil
	m.deltaCh = nil
	m.approvals = nil
	m.stopping, m.runFailed = false, false
	m.textInput.Placeholder = idlePlaceholder

	if interrupted {
		var texts []string
		kept := m.pending[:0]
		for _, p := range m.pending {
			switch p.state {
			case subWaiting:
				texts = append(texts, p.text)
				continue
			case subSending:
				// Submit has not returned yet; its result returns the text.
				p.state = subReturn
			}
			kept = append(kept, p)
		}
		m.pending = kept
		m.returnToInput(texts)
		m.refresh()
		return m, nil
	}
	return m.drainWaiting()
}

// returnToInput puts texts back in the input ahead of anything typed since,
// so the user decides whether to send them.
func (m *runnerModel) returnToInput(texts []string) {
	if len(texts) == 0 {
		return
	}
	restored := strings.Join(texts, " ")
	if cur := strings.TrimSpace(m.textInput.Value()); cur != "" {
		restored += " " + cur
	}
	m.textInput.SetValue(restored)
	m.textInput.CursorEnd()
	m.notice = returnedNotice
}

// drainWaiting starts a run for messages that missed the previous one: the
// first becomes the run's input and the rest join it in their own modes.
func (m runnerModel) drainWaiting() (tea.Model, tea.Cmd) {
	if m.running() {
		m.refresh()
		return m, nil
	}
	i := m.pendingIndex(func(p pendingSubmission) bool { return p.state == subWaiting })
	if i < 0 {
		m.refresh()
		return m, nil
	}
	first := m.pending[i]
	m.pending = append(m.pending[:i], m.pending[i+1:]...)
	model, cmd := m.startRun(first.text)
	m = model.(runnerModel)

	cmds := []tea.Cmd{cmd}
	for j := range m.pending {
		p := &m.pending[j]
		if p.state != subWaiting {
			continue
		}
		p.state = subSending
		cmds = append(cmds, submitCmd(m.stream, m.gen, p.local, p.text, p.mode))
	}
	return m, tea.Batch(cmds...)
}

// ── Layout ───────────────────────────────────────────────────────────

// refresh sizes the viewport around the header and footer and sets its
// content. The view follows new output only while it is scrolled to the
// bottom, so reading back through the transcript is not interrupted.
func (m *runnerModel) refresh() {
	width := m.width
	if width <= 0 {
		width = 80
	}
	if m.template.RenderMarkdown {
		m.act.renderMarkdown(max(width-2, 20))
	}
	if m.ready {
		m.scroll.resize(width, viewportHeight(m.height, m.headerView(), m.footerView()))
	}
	shown := m.filter.apply(m.act.entries)
	m.scroll.setContent(m.render(shown), len(shown))
}

func (m runnerModel) logView() string {
	return m.render(m.filter.apply(m.act.entries))
}

func (m runnerModel) render(entries []activityEntry) string {
	lr := logRenderer{entries: entries, spin: m.spin(), template: m.template,
		expandThinking: m.expandThinking, animate: m.animate, now: m.clock()}
	return lr.renderLog()
}

func (m runnerModel) headerView() string {
	// Count what is drawn: an entry the template hides is neither shown
	// nor part of the total.
	lr := logRenderer{template: m.template}
	badge := m.filter.badge(lr.visible(m.filter.apply(m.act.entries)), lr.visible(m.act.entries), m.filtering)
	return topView(m.template.ShowHeader, m.header, m.width, badge)
}

// markerArgLines caps the arguments shown in an approval prompt.
const markerArgLines = 12

// footerView renders everything below the transcript: the queue strip, the
// run status or approval prompt, a hint, and the input line.
func (m runnerModel) footerView() string {
	var lines []string

	width := m.width
	if width <= 0 {
		width = 80
	}
	for _, p := range m.pending {
		label := "queued"
		if p.mode == agentsdk.SubmitSteer {
			label = "steer"
		}
		if p.state == subWaiting {
			label += ", next run"
		}
		lines = append(lines, queuedStyle.Render(truncateRunes(fmt.Sprintf("  %s %s: %s", iconQueued, label, p.text), width)))
	}

	switch {
	case m.phase == phaseMarker && m.marker() != nil:
		head := m.marker()
		title := fmt.Sprintf("%s Tool %q requires approval", iconMarker, head.toolName)
		if n := len(m.approvals); n > 1 {
			title += fmt.Sprintf(" (%d more waiting)", n-1)
		}
		lines = append(lines, markerStyle.Render(title))
		for _, mk := range head.markers {
			lines = append(lines, markerDetailStyle.Render(fmt.Sprintf("  %s: %s", mk.Kind, mk.Message)))
		}
		for _, l := range strings.Split(FormatArgs(head.args, markerArgLines), "\n") {
			lines = append(lines, "  "+l)
		}
	case m.running():
		status := "Working..."
		if m.stopping {
			status = stoppingNotice
		}
		lines = append(lines, fmt.Sprintf("  %s %s", m.spin(), thinkingStyle.Render(status)))
	}

	if ind := m.scroll.indicator("ctrl+end"); ind != "" {
		lines = append(lines, indicatorStyle.Render("  "+ind))
	}
	if m.notice != "" {
		lines = append(lines, usageStyle.Render("  "+m.notice))
	}
	if m.filtering {
		lines = append(lines, m.filterInput.View())
	} else {
		lines = append(lines, m.textInput.View())
	}
	return strings.Join(lines, "\n")
}

func (m runnerModel) View() string {
	var b strings.Builder
	if h := m.headerView(); h != "" {
		b.WriteString(h)
		b.WriteString("\n")
	}
	if m.ready {
		b.WriteString(m.scroll.vp.View())
	} else {
		b.WriteString(m.logView())
	}
	b.WriteString("\n")
	b.WriteString(m.footerView())
	return b.String()
}
