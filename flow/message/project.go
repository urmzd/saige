package message

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/event"
)

// Delta types, on a content_block_delta.
const (
	deltaText      = "text_delta"
	deltaThinking  = "thinking_delta"
	deltaInputJSON = "input_json_delta"
	deltaCitations = "citations_delta"
)

// Option configures a Projector.
type Option func(*options)

type options struct {
	toolUseID  func(run string, n int) string
	classify   func(name string, args json.RawMessage) (kind, title string)
	background func(name string) bool
}

// WithToolUseID sets how a run's n-th tool call is named in the stream.
// The default is ToolUseID. A host that mints its own ids passes the same
// rule it stamps ParentToolCall with, so a called run nests under its call.
func WithToolUseID(fn func(run string, n int) string) Option {
	return func(o *options) { o.toolUseID = fn }
}

// WithClassify sets a tool call's kind and card title from its name and
// arguments. The default: "agent" is an agent call titled by its "title"
// argument, "get_context" is a context pull, and anything else a tool.
func WithClassify(fn func(name string, args json.RawMessage) (kind, title string)) Option {
	return func(o *options) { o.classify = fn }
}

// WithBackground names the tools whose calls run in the background: the
// response ends without waiting for the run they start. The default is
// "delegate_task".
func WithBackground(fn func(name string) bool) Option {
	return func(o *options) { o.background = fn }
}

// Projector writes one response's stream from the envelopes of its run
// tree. It is not safe for concurrent use.
type Projector struct {
	head Head
	opts options

	cursor Cursor
	runs   map[string]*runState
	byID   map[string]*call
	calls  []*call
	lanes  []*lane
	root   *lane
	out    []Event
	tree   treeTally

	// ending is the root's end, held until the calls it waits on end.
	ending *ending
	done   bool
}

// NewProjector returns a projector for the response head describes, whose
// root run is head.RootRun.
func NewProjector(head Head, opts ...Option) *Projector {
	p := &Projector{head: head}
	for _, o := range opts {
		o(&p.opts)
	}
	if p.opts.toolUseID == nil {
		p.opts.toolUseID = ToolUseID
	}
	if p.opts.classify == nil {
		p.opts.classify = classify
	}
	if p.opts.background == nil {
		p.opts.background = func(name string) bool { return name == "delegate_task" }
	}
	p.cursor = Cursor{}
	p.runs = map[string]*runState{}
	p.byID = map[string]*call{}
	id := head.ID
	if id == "" {
		id = head.RootRun
	}
	r := p.newRun(head.RootRun, 0)
	p.root = &lane{run: r, root: true, msgID: id, agent: head.Agent, label: head.Agent, open: -1}
	r.own = p.root
	p.lanes = append(p.lanes, p.root)
	return p
}

// Add projects one envelope of the tree, given in total order (Less), and
// returns the events it adds to the stream. An envelope of another tree, or
// one already projected (by its run and sequence), adds nothing.
func (p *Projector) Add(e event.Envelope) []Event {
	if !p.ours(e) {
		return nil
	}
	if last, ok := p.cursor[e.Run]; ok && e.Seq <= last {
		return nil
	}
	p.cursor[e.Run] = e.Seq
	if p.done {
		return nil
	}
	p.out = nil
	if p.root.label == "" && e.Run == p.head.RootRun {
		p.root.label = e.Agent
	}
	p.start(p.root)
	p.apply(p.runOf(e), e)
	p.finish()
	out := p.out
	p.out = nil
	return out
}

// Cursor is how far the projector has read.
func (p *Projector) Cursor() Cursor {
	out := make(Cursor, len(p.cursor))
	for run, seq := range p.cursor {
		out[run] = seq
	}
	return out
}

// Done reports whether the stream has ended: message_stop was sent.
func (p *Projector) Done() bool { return p.done }

// classify is the default WithClassify.
func classify(name string, args json.RawMessage) (kind, title string) {
	switch name {
	case "agent":
		var a struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(args, &a)
		return KindAgent, a.Title
	case "get_context":
		return KindContext, ""
	}
	return KindTool, ""
}

// runState is what the projector knows of one run of the tree.
type runState struct {
	id    string
	depth int
	// n counts the run's distinct tool calls, at any scope.
	n      int
	calls  map[string]*call
	order  []*call
	own    *lane
	nested map[string]*lane
	inner  []*lane
	ended  bool
	// parked are the waits the run is parked on, one per question.
	parked []parking
	usage  ownTally
}

// lane is one message of the stream: the root's, a called run's, or a
// nested agent's inside a run.
type lane struct {
	run *runState
	// parent is the call the lane answers, nil at the root or while the
	// call is unseen; parentID names it either way.
	parent    *call
	parentID  string
	parentRun string
	depth     int
	path      []string
	root      bool
	msgID     string
	agent     string
	label     string
	started   bool
	ended     bool
	next      int
	open      int
	openType  string
	wrote     bool
	state     string
	// sources are the citations the lane made or was given by its calls'
	// results, for the result of the call it answers.
	sources []event.Citation
}

// call is one tool call of a lane.
type call struct {
	id       string
	provider string
	name     string
	kind     string
	title    string
	lane     *lane
	path     []string
	args     json.RawMessage
	shown    bool
	done     bool
	denied   bool
	executed bool
	execAt   time.Time
	status   string
	// question is what the call waits on while it awaits a person.
	question *Question
	// bubbles are the deeper calls, waiting on a person or failed, whose
	// status a depth-one call shows: the last one is shown.
	bubbles []*call
	lanes   []*lane
}

// parking is one question a run waits on.
type parking struct {
	call     *call
	lane     *lane
	kind     string
	status   string
	question *event.Question
	// at is the question's place in the park, which is the answer's.
	at int
}

// ending is the root's end as it arrived.
type ending struct {
	reason string
	usage  *event.RunUsage
}

func (p *Projector) newRun(id string, depth int) *runState {
	r := &runState{id: id, depth: depth, calls: map[string]*call{}, nested: map[string]*lane{}}
	p.runs[id] = r
	return r
}

// ours reports whether e belongs to the response's tree.
func (p *Projector) ours(e event.Envelope) bool {
	if e.Run == "" {
		return false
	}
	return e.Root == p.head.RootRun || (e.Root == "" && e.Run == p.head.RootRun)
}

// runOf returns e's run, placing a run seen for the first time under the
// call that started it.
func (p *Projector) runOf(e event.Envelope) *runState {
	if r := p.runs[e.Run]; r != nil {
		return r
	}
	// A run the runtime started names its call by the provider's id, as
	// its caller's tool events do; a run the host started, by the id the
	// host minted.
	pr := p.runs[e.ParentRun]
	var parent *call
	if pr != nil && e.ParentToolCall != "" {
		parent = p.findCall(pr, e.ParentToolCall)
	}
	if parent == nil {
		parent = p.byID[e.ParentToolCall]
	}
	depth := 1
	switch {
	case parent != nil:
		depth = parent.lane.depth + 1
	case pr != nil:
		depth = pr.depth + 1
	}
	r := p.newRun(e.Run, depth)
	l := &lane{run: r, parentRun: e.ParentRun, parentID: e.ParentToolCall, depth: depth, msgID: e.Run, agent: e.Agent, open: -1}
	switch {
	case parent != nil:
		l.parent, l.parentID, l.path = parent, parent.id, parent.path
		parent.lanes = append(parent.lanes, l)
	case e.ParentToolCall != "":
		l.path = []string{e.ParentToolCall}
	}
	l.label = firstOf(e.Agent, titleOf(parent), nameOf(parent))
	r.own = l
	p.lanes = append(p.lanes, l)
	return r
}

// laneAt returns the lane of run r's agent at path: r's own agent when path
// is empty, else the nested agent the last call of path runs.
func (p *Projector) laneAt(r *runState, path []string) *lane {
	if len(path) == 0 {
		return r.own
	}
	key := scopeOf(path)
	if l := r.nested[key]; l != nil {
		return l
	}
	c := p.callAt(r, path[:len(path)-1], path[len(path)-1])
	l := &lane{run: r, parent: c, parentID: c.id, depth: r.own.depth + len(path), path: c.path,
		msgID: c.id, agent: c.name, label: firstOf(c.title, c.name), open: -1}
	c.lanes = append(c.lanes, l)
	r.nested[key] = l
	r.inner = append(r.inner, l)
	p.lanes = append(p.lanes, l)
	return l
}

// callAt returns the call id made by the agent at scope in run r, minting
// its stream id the first time it is seen.
func (p *Projector) callAt(r *runState, scope []string, id string) *call {
	key := scopeOf(scope) + "|" + id
	if c := r.calls[key]; c != nil {
		return c
	}
	l := p.laneAt(r, scope)
	r.n++
	c := &call{id: p.opts.toolUseID(r.id, r.n), provider: id, lane: l, kind: KindTool}
	c.path = append(slices.Clone(l.path), c.id)
	r.calls[key] = c
	r.order = append(r.order, c)
	p.byID[c.id] = c
	p.calls = append(p.calls, c)
	// A called run seen before its call nests under it now.
	for _, o := range p.lanes {
		if o.parent == nil && !o.root && (o.parentID == c.id || (o.parentRun == r.id && o.parentID == id)) {
			o.parent, o.parentID = c, c.id
			c.lanes = append(c.lanes, o)
		}
	}
	return c
}

// findCall returns run r's call by its provider id, its own agent's first.
func (p *Projector) findCall(r *runState, id string) *call {
	if c := r.calls["|"+id]; c != nil {
		return c
	}
	for _, c := range r.order {
		if c.provider == id {
			return c
		}
	}
	return nil
}

func (p *Projector) name(c *call, name string, args json.RawMessage) {
	if c.name == "" && name != "" {
		c.name = name
	}
	if len(c.args) == 0 && len(args) > 0 {
		c.args = slices.Clone(args)
	}
	c.kind, c.title = p.opts.classify(c.name, c.args)
}

// background reports whether c runs in the background: its tool does, or
// the lane it was made in.
func (p *Projector) background(c *call) bool {
	if p.opts.background(c.name) {
		return true
	}
	return p.laneBackground(c.lane)
}

func (p *Projector) laneBackground(l *lane) bool {
	return l.parent != nil && p.background(l.parent)
}

func (p *Projector) apply(r *runState, e event.Envelope) {
	body := bodyOf(e.Data)
	if u, ok := body.(event.UsageCall); ok {
		p.tree.add(u)
		r.usage.add(u)
		return
	}
	if r.ended {
		return
	}
	switch b := body.(type) {
	case event.Accepted, event.Started:
		p.start(r.own)
	case event.Text:
		p.say(p.laneAt(r, b.Parent), BlockText, BlockDelta{Type: deltaText, Text: b.Text})
	case event.Thinking:
		p.say(p.laneAt(r, b.Parent), BlockThinking, BlockDelta{Type: deltaThinking, Thinking: b.Text})
	case event.Citation:
		l := p.laneAt(r, b.Parent)
		l.sources = append(l.sources, b)
		c := citation(b)
		p.say(l, BlockText, BlockDelta{Type: deltaCitations, Citation: &c})
	case event.ToolCall:
		p.toolCall(r, b, e.Time)
	case event.ToolDone:
		p.toolDone(r, b, e.Time)
	case event.Parked:
		p.park(r, b, e.Time)
	case event.Resumed:
		p.resume(r, b, e.Time)
	case event.Completed:
		p.end(r, StopEndTurn, b.Text, b.Usage, e.Time)
	case event.Failed:
		if !b.Terminal {
			p.closeAll(r)
			return
		}
		p.end(r, failedReason(b.Error), "", nil, e.Time)
	case event.Stopped:
		p.end(r, StopStopped, b.Text, b.Usage, e.Time)
	}
}

// bodyOf returns a body by value, as the catalog decodes it.
func bodyOf(b event.Body) event.Body {
	v := reflect.ValueOf(b)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		if out, ok := v.Elem().Interface().(event.Body); ok {
			return out
		}
	}
	return b
}

// emit adds ev to lane l's stream.
func (p *Projector) emit(l *lane, ev Event) {
	if l.root {
		p.out = append(p.out, ev)
		return
	}
	p.out = append(p.out, Nested{ParentToolUseID: l.parentID, AgentRunID: l.run.id, Depth: l.depth, Event: ev})
}

// start opens lane l's message.
func (p *Projector) start(l *lane) {
	if l.started {
		return
	}
	l.started = true
	m := StartedMessage{ID: l.msgID, Type: "message", Role: "assistant", Content: []Block{}}
	if l.root {
		h := p.head
		m.Humagen = &h
	} else {
		m.Humagen = &Head{RootRun: p.head.RootRun, Agent: l.agent}
	}
	p.emit(l, MessageStart{Message: m})
	if !l.root {
		p.progress(l, "running")
	}
}

// say adds delta to lane l's open block of type typ, opening one if the
// open block is of another type.
func (p *Projector) say(l *lane, typ string, d BlockDelta) {
	if l.ended {
		return
	}
	p.start(l)
	if l.open < 0 || l.openType != typ {
		p.closeBlock(l)
		l.open, l.openType = l.next, typ
		l.next++
		p.emit(l, ContentBlockStart{Index: l.open, ContentBlock: Block{Type: typ}})
	}
	p.emit(l, ContentBlockDelta{Index: l.open, Delta: d})
	if d.Type == deltaText {
		l.wrote = true
	}
}

func (p *Projector) closeBlock(l *lane) {
	if l.open < 0 {
		return
	}
	p.emit(l, ContentBlockStop{Index: l.open})
	l.open, l.openType = -1, ""
}

// closeAll closes every open block of run r: a failed attempt's blocks end
// where it failed, and the next attempt opens its own.
func (p *Projector) closeAll(r *runState) {
	p.closeBlock(r.own)
	for _, l := range r.inner {
		p.closeBlock(l)
	}
}

// block adds a whole block to lane l.
func (p *Projector) block(l *lane, b Block) {
	p.closeBlock(l)
	i := l.next
	l.next++
	p.emit(l, ContentBlockStart{Index: i, ContentBlock: b})
	p.emit(l, ContentBlockStop{Index: i})
}

func (p *Projector) toolCall(r *runState, b event.ToolCall, at time.Time) {
	l := p.laneAt(r, b.Parent)
	if l.ended {
		return
	}
	p.start(l)
	c := p.callAt(r, b.Parent, b.ID)
	p.name(c, b.Name, b.Arguments)
	if c.done {
		return
	}
	switch b.Phase {
	case event.PhaseRequested:
		if c.status == "" {
			p.setStatus(c, StatusQueued, nil, at)
		}
	case event.PhaseExecuting:
		if !c.executed {
			c.executed, c.execAt = true, at
		}
		p.show(c)
		p.setStatus(c, StatusRunning, nil, at)
	}
}

// show sends call c's server_tool_use block, once.
func (p *Projector) show(c *call) {
	if c.shown {
		return
	}
	c.shown = true
	input := c.args
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	p.block(c.lane, Block{Type: BlockServerToolUse, ID: c.id, Name: c.name, Input: input, Kind: c.kind, Title: c.title})
}

func (p *Projector) toolDone(r *runState, b event.ToolDone, at time.Time) {
	l := p.laneAt(r, b.Parent)
	if l.ended {
		return
	}
	p.start(l)
	c := p.callAt(r, b.Parent, b.ID)
	if c.done {
		return
	}
	p.name(c, b.Name, b.Arguments)
	reason := StopEndTurn
	if b.Error != "" {
		reason = StopFailed
	}
	for _, in := range c.lanes {
		if in.run == r {
			p.endLane(in, reason, &Usage{})
		}
	}
	p.show(c)
	text := b.Result
	if b.Error != "" {
		text = b.Error
	}
	content := append([]ResultContent{{Type: BlockText, Text: text}}, p.searchResults(c)...)
	p.block(l, Block{Type: BlockServerToolResult, ToolUseID: c.id, Content: content, IsError: b.Error != ""})
	c.done = true
	for _, in := range c.lanes {
		l.sources = append(l.sources, in.sources...)
	}
	status := StatusDone
	switch {
	case b.Error != "" && c.denied:
		status = StatusDenied
	case b.Error != "":
		status = StatusError
	}
	p.setStatus(c, status, nil, at)
}

// searchResults are the sources the lanes under c cited, one search_result
// per source, so they survive the call.
func (p *Projector) searchResults(c *call) []ResultContent {
	var out []ResultContent
	at := map[string]int{}
	for _, l := range c.lanes {
		for _, s := range l.sources {
			ct := citation(s)
			key := ct.Source + "\x00" + ct.Title
			i, ok := at[key]
			if !ok {
				i = len(out)
				at[key] = i
				out = append(out, ResultContent{Type: "search_result", Source: ct.Source, Title: ct.Title,
					Content: []TextItem{}, Citations: &Enabled{Enabled: true}})
			}
			if ct.CitedText != "" && !slices.Contains(out[i].Content, TextItem{Type: BlockText, Text: ct.CitedText}) {
				out[i].Content = append(out[i].Content, TextItem{Type: BlockText, Text: ct.CitedText})
			}
		}
	}
	return out
}

// park reports each question run r parked on on the call it is about: the
// call it names, else the call that started the child run it names, else
// the call that started r. A wait on a child run is not a person's and
// shows nothing: the child's own lane does. Waits on the call that started
// r are reported first, so a depth-one call goes on showing the waits of
// the calls below it.
func (p *Projector) park(r *runState, b event.Parked, at time.Time) {
	p.start(r.own)
	p.closeAll(r)
	qs := b.Questions
	if len(qs) == 0 && b.Question != nil {
		qs = []event.Question{*b.Question}
	}
	r.parked = nil
	for i := range qs {
		q := &qs[i]
		if q.Kind == event.QuestionDelegation {
			continue
		}
		w := parking{lane: r.own, kind: q.Kind, status: StatusAwaitingInput, question: q, at: i}
		if q.Kind == event.QuestionApproval || q.Kind == event.QuestionHold {
			w.status = StatusAwaitingApproval
		}
		switch w.call = p.questionCall(r, q); {
		case w.call == nil:
			w.call = r.own.parent
		case w.call.lane.run == r:
			w.lane = w.call.lane
		}
		r.parked = append(r.parked, w)
	}
	slices.SortStableFunc(r.parked, func(a, b parking) int { return b.above(r) - a.above(r) })
	var lanes []*lane
	approval := map[*lane]bool{}
	for _, w := range r.parked {
		if w.call != nil {
			p.setStatus(w.call, w.status, question(w.question), at)
		}
		if !slices.Contains(lanes, w.lane) {
			lanes = append(lanes, w.lane)
		}
		approval[w.lane] = approval[w.lane] || w.status == StatusAwaitingApproval
	}
	for _, l := range lanes {
		state := "awaiting input"
		if approval[l] {
			state = "awaiting approval"
		}
		p.progress(l, state)
	}
}

// above is 1 for a wait shown on the call that started run r, else 0.
func (w parking) above(r *runState) int {
	if w.call != nil && w.call.lane.run != r {
		return 1
	}
	return 0
}

// questionCall is the call a question of run r is about, if it names one.
func (p *Projector) questionCall(r *runState, q *event.Question) *call {
	if q.Tool != nil && q.Tool.CallID != "" {
		if c := p.findCall(r, q.Tool.CallID); c != nil {
			return c
		}
	}
	if child := p.runs[q.ChildRunID]; q.ChildRunID != "" && child != nil && child.own.parent != nil {
		return child.own.parent
	}
	return nil
}

// resume moves each call run r waited on along with its answer: denied for
// an approval denied, else running.
func (p *Projector) resume(r *runState, b event.Resumed, at time.Time) {
	waits := r.parked
	r.parked = nil
	answers := b.Answers
	if len(answers) == 0 && b.Answer != nil {
		answers = []event.Answer{*b.Answer}
	}
	var lanes []*lane
	for _, w := range waits {
		if c := w.call; c != nil && !c.done {
			status := StatusRunning
			if a := answerFor(answers, w.question.ID, w.at); a != nil && a.Decision == event.DecisionDeny && w.kind == event.QuestionApproval {
				status, c.denied = StatusDenied, true
			}
			p.setStatus(c, status, nil, at)
		}
		if !slices.Contains(lanes, w.lane) {
			lanes = append(lanes, w.lane)
		}
	}
	for _, l := range lanes {
		p.progress(l, "running")
	}
}

// answerFor is the answer to question id, else the i-th answer.
func answerFor(answers []event.Answer, id string, i int) *event.Answer {
	for j := range answers {
		if id != "" && answers[j].QuestionID == id {
			return &answers[j]
		}
	}
	if i < len(answers) && (id == "" || answers[i].QuestionID == "") {
		return &answers[i]
	}
	return nil
}

// end ends run r: its lanes' messages, and at the root the response, once
// the calls it waits on have ended.
func (p *Projector) end(r *runState, reason, text string, usage *event.RunUsage, at time.Time) {
	l := r.own
	p.start(l)
	if text != "" && !l.wrote {
		p.say(l, BlockText, BlockDelta{Type: deltaText, Text: text})
	}
	r.ended = true
	for _, c := range r.order {
		if c.done || c.denied {
			continue
		}
		switch reason {
		case StopEndTurn:
		case StopStopped:
			p.setStatus(c, StatusStopped, nil, at)
		default:
			p.setStatus(c, StatusError, nil, at)
		}
	}
	for _, in := range r.inner {
		p.endLane(in, reason, &Usage{})
	}
	if !l.root {
		p.endLane(l, reason, r.usage.of(usage))
		return
	}
	p.closeBlock(l)
	p.ending = &ending{reason: reason, usage: usage}
}

// endLane ends lane l's message.
func (p *Projector) endLane(l *lane, reason string, u *Usage) {
	if l.ended {
		return
	}
	p.start(l)
	p.closeBlock(l)
	l.ended = true
	p.emit(l, MessageDelta{Delta: StopDelta{StopReason: reason}, Usage: u})
	p.emit(l, MessageStop{})
	state := "done"
	switch reason {
	case StopStopped:
		state = "stopped"
	case StopEndTurn:
	default:
		state = "failed"
	}
	p.progress(l, state)
}

// finish sends the root's held end once nothing it waits on is left.
func (p *Projector) finish() {
	if p.ending == nil || p.done || p.pending() {
		return
	}
	root := p.runs[p.head.RootRun]
	p.emit(p.root, MessageDelta{
		Delta:   StopDelta{StopReason: p.ending.reason},
		Usage:   root.usage.of(p.ending.usage),
		Humagen: p.tree.of(p.ending.usage),
	})
	p.emit(p.root, MessageStop{})
	p.root.ended = true
	p.done = true
}

// pending reports whether a call the response waits on has not ended: a
// call outside the background with no result while its run goes on, or a
// called run that has not ended.
func (p *Projector) pending() bool {
	for _, c := range p.calls {
		if !c.done && !c.denied && !c.lane.run.ended && !p.background(c) {
			return true
		}
	}
	for _, l := range p.lanes {
		if !l.root && l.run.own == l && !l.ended && !p.laneBackground(l) {
			return true
		}
	}
	return false
}

// setStatus reports call c's status, at its lane and, when it waits on a
// person or failed below the root's calls, on its depth-one call too.
func (p *Projector) setStatus(c *call, status string, q *Question, at time.Time) {
	if c.status == status && q == nil {
		return
	}
	c.status, c.question, c.bubbles = status, q, nil
	p.emit(c.lane, ToolStatus{ToolUseID: c.id, Path: slices.Clone(c.path), Status: status, Title: c.title, Question: q, ElapsedMs: elapsed(c, at)})
	if c.lane.depth < 1 || len(c.path) < 2 {
		return
	}
	top := p.byID[c.path[0]]
	bubbles := status == StatusAwaitingApproval || status == StatusAwaitingInput || status == StatusError
	if top == nil {
		if bubbles {
			p.out = append(p.out, ToolStatus{ToolUseID: c.path[0], Path: slices.Clone(c.path), Status: status, Title: c.title, Question: q})
		}
		return
	}
	shown := len(top.bubbles) > 0 && top.bubbles[len(top.bubbles)-1] == c
	top.bubbles = slices.DeleteFunc(top.bubbles, func(b *call) bool { return b == c })
	switch {
	case bubbles:
		top.bubbles = append(top.bubbles, c)
		p.out = append(p.out, p.bubble(top, c, at))
	case !shown:
	case len(top.bubbles) > 0:
		p.out = append(p.out, p.bubble(top, top.bubbles[len(top.bubbles)-1], at))
	default:
		p.out = append(p.out, ToolStatus{ToolUseID: top.id, Path: slices.Clone(top.path), Status: firstOf(top.status, StatusRunning),
			Title: top.title, Question: top.question, ElapsedMs: elapsed(top, at)})
	}
}

// bubble is deep call c's status as depth-one call top shows it.
func (p *Projector) bubble(top, c *call, at time.Time) ToolStatus {
	return ToolStatus{ToolUseID: top.id, Path: slices.Clone(c.path), Status: c.status, Title: c.title, Question: c.question, ElapsedMs: elapsed(top, at)}
}

// progress reports lane l's state when it changes.
func (p *Projector) progress(l *lane, state string) {
	if l.state == state {
		return
	}
	l.state = state
	text := state
	if l.label != "" {
		text = l.label + ": " + state
	}
	p.out = append(p.out, Progress{ParentToolUseID: l.parentID, Text: text})
}

// elapsed is how long call c has run at time at, by envelope times only.
func elapsed(c *call, at time.Time) int64 {
	if c == nil || !c.executed {
		return 0
	}
	return max(at.Sub(c.execAt).Milliseconds(), 0)
}

func question(q *event.Question) *Question {
	who := slices.Clone(q.Approvers)
	if len(who) == 0 && q.AskedOf != "" {
		who = []string{q.AskedOf}
	}
	return &Question{ID: q.ID, Kind: q.Kind, Prompt: q.Prompt, Choices: slices.Clone(q.Options), Who: who}
}

// citation is a citation body as a text block cites it.
func citation(c event.Citation) Citation {
	return Citation{
		Type:      "search_result_location",
		Source:    firstOf(c.Source.SourceURI, c.Source.DocumentUUID, c.Label),
		Title:     firstOf(c.Source.DocumentTitle, c.Source.SectionHeading),
		CitedText: c.Text,
		Index:     c.Index,
	}
}

// failedReason is the stop reason a failure's error names, if it names one.
func failedReason(msg string) string {
	if strings.Contains(strings.ToLower(msg), "response truncated") {
		return StopMaxTokens
	}
	if strings.Contains(strings.ToLower(msg), "refusal") {
		return StopRefusal
	}
	switch k, ok := saigetypes.ClassifyErrorMessage(msg); {
	case ok && k == saigetypes.ErrorKindContextLength:
		return StopContextWindow
	case ok && k == saigetypes.ErrorKindContentFilter:
		return StopRefusal
	}
	return StopFailed
}

// scopeOf keys the agent at a tool call path within a run.
func scopeOf(path []string) string {
	var b strings.Builder
	for _, id := range path {
		b.WriteString("/")
		b.WriteString(id)
	}
	return b.String()
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func titleOf(c *call) string {
	if c == nil {
		return ""
	}
	return c.title
}

func nameOf(c *call) string {
	if c == nil {
		return ""
	}
	return c.name
}
