package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
)

// requestBody renders everything a recorded request sent: each message's
// text, tool calls and tool results, and the tool names.
func requestBody(c agenttest.ScriptedCall) string {
	var b strings.Builder
	for _, m := range c.Messages {
		b.WriteString(transcriptText(m))
		b.WriteByte('\n')
	}
	for _, d := range c.Tools {
		b.WriteString("tool:" + d.Name + "\n")
	}
	return b.String()
}

func toolNames(c agenttest.ScriptedCall) []string {
	var names []string
	for _, d := range c.Tools {
		names = append(names, d.Name)
	}
	return names
}

// toolResultIn returns the result text of call id in a recorded request.
func toolResultIn(t *testing.T, c agenttest.ScriptedCall, id string) string {
	t.Helper()
	for _, m := range c.Messages {
		for _, r := range toolResultsOf(m) {
			if r.ToolCallID == id {
				return r.Text
			}
		}
	}
	t.Fatalf("request has no result for tool call %s", id)
	return ""
}

func lookupTool() *agenttest.MockTool {
	return &agenttest.MockTool{Def: types.ToolDef{Name: "lookup", Description: "look something up"}, Result: "found"}
}

// loopingChild calls lookup n times and then answers with final.
func loopingChild(n int, final string) *agenttest.ScriptedProvider {
	p := &agenttest.ScriptedProvider{}
	for i := range n {
		p.Responses = append(p.Responses, agenttest.ToolCallResponse(fmt.Sprintf("l%d", i), "lookup", map[string]any{"n": i}))
	}
	p.Responses = append(p.Responses, agenttest.TextResponse(final))
	return p
}

// resultSink keeps every saved result.
type resultSink struct {
	mu      sync.Mutex
	results []SubAgentResult
}

func (s *resultSink) Save(_ context.Context, r SubAgentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results = append(s.results, r)
	return nil
}

func (s *resultSink) only(t *testing.T) SubAgentResult {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) != 1 {
		t.Fatalf("saved %d results, want 1", len(s.results))
	}
	return s.results[0]
}

func TestChildIterBudget(t *testing.T) {
	tests := []struct {
		name          string
		parent        int
		def           SubAgentDef
		max, wrapUpAt int
	}{
		{"inherits a bounded parent", 25, SubAgentDef{}, 25, 23},
		{"bounded under an unbounded parent", NoIterLimit, SubAgentDef{}, DefaultSubAgentMaxIter, DefaultSubAgentMaxIter - DefaultWrapUpMargin},
		{"own cap", NoIterLimit, SubAgentDef{MaxIter: 4}, 4, 2},
		{"small cap wraps up after one turn", 10, SubAgentDef{MaxIter: 2}, 2, 1},
		{"cap of one has no wrap-up", 10, SubAgentDef{MaxIter: 1}, 1, 0},
		{"explicit wrap-up", 10, SubAgentDef{MaxIter: 8, WrapUpAt: 5}, 8, 5},
		{"wrap-up off", 10, SubAgentDef{MaxIter: 8, WrapUpAt: -1}, 8, 0},
		{"unbounded child", 10, SubAgentDef{MaxIter: NoIterLimit}, NoIterLimit, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, w := childIterBudget(tt.parent, tt.def); m != tt.max || w != tt.wrapUpAt {
				t.Fatalf("childIterBudget = %d, %d, want %d, %d", m, w, tt.max, tt.wrapUpAt)
			}
		})
	}
}

func TestWrapUpNoteNamesFinalAnswer(t *testing.T) {
	plain := wrapUpNote("", 8, 10, runOutput{})
	if !strings.Contains(plain, "used 8 of 10 iterations; 2 remain.") || strings.Contains(plain, FinalAnswerToolName) {
		t.Fatalf("plain note = %q", plain)
	}
	tool := wrapUpNote("", 9, 10, runOutput{schema: cityPopulationSchema, mode: OutputTool})
	if !strings.Contains(tool, "1 remains.") || !strings.Contains(tool, "Call "+FinalAnswerToolName) {
		t.Fatalf("tool-mode note = %q", tool)
	}
	if custom := wrapUpNote("Finish the table.", 1, 3, runOutput{}); !strings.Contains(custom, "Finish the table.") || strings.Contains(custom, DefaultWrapUpPrompt) {
		t.Fatalf("custom note = %q", custom)
	}
}

// A bounded child is told to wrap up two iterations before its cap, then
// forced to answer without tools at the cap. The delegation succeeds, and
// the result says how many iterations were used and that it was forced.
func TestChildWrapUpThenForcedReturn(t *testing.T) {
	child := loopingChild(4, "forced answer")
	sink := &resultSink{}
	parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_worker", map[string]any{"task": "dig"}),
		agenttest.TextResponse("parent done"),
	}}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: parentProvider}, WithSubAgents(SubAgentDef{
		Name: "worker", Description: "w", Provider: child, MaxIter: 4, ResultSink: sink,
		Tools: types.NewToolRegistry(lookupTool()),
	}))
	stream := parent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	deltas := agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatalf("parent failed: %v", err)
	}

	reqs := child.Requests()
	if len(reqs) != 5 {
		t.Fatalf("child requests = %d, want 4 turns and the forced call", len(reqs))
	}
	for i, r := range reqs {
		has := strings.Contains(requestBody(r), "<wrap_up>")
		if want := i >= 2; has != want {
			t.Fatalf("request %d has wrap-up note = %v, want %v", i, has, want)
		}
	}
	if !strings.Contains(requestBody(reqs[2]), "used 2 of 4 iterations; 2 remain") {
		t.Fatalf("wrap-up note:\n%s", requestBody(reqs[2]))
	}
	if strings.Count(requestBody(reqs[3]), "<wrap_up>") != 1 {
		t.Fatal("wrap-up note sent more than once")
	}
	if forced := reqs[4]; len(forced.Tools) != 0 || !strings.Contains(requestBody(forced), DefaultForceFinalPrompt) {
		t.Fatalf("forced call kept tools %v", toolNames(forced))
	}

	var injected bool
	for _, d := range deltas {
		if ex, ok := d.(types.ToolExecDelta); ok {
			if in, ok := ex.Inner.(types.InjectedDelta); ok && in.Mode == "wrap_up" {
				injected = true
			}
		}
	}
	if !injected {
		t.Fatal("no wrap_up InjectedDelta forwarded from the child")
	}

	r := sink.only(t)
	if r.Error != "" || !r.Forced || r.Iterations != 5 || r.MaxIter != 4 || r.Output != "forced answer" ||
		!strings.Contains(r.ForcedReason, types.ErrMaxIterations.Error()) {
		t.Fatalf("result = iterations %d max %d forced %v (%q) output %q error %q", r.Iterations, r.MaxIter, r.Forced, r.ForcedReason, r.Output, r.Error)
	}
	got := toolResultIn(t, parentProvider.Requests()[1], "d1")
	if !strings.Contains(got, "step limit after 5 iterations") || !strings.HasSuffix(got, "forced answer") {
		t.Fatalf("parent tool result = %q", got)
	}
}

// A child that finishes before its wrap-up point gets no note and is not
// marked forced.
func TestChildWithinBudgetIsNotForced(t *testing.T) {
	child := loopingChild(1, "quick answer")
	parent := NewAgent(AgentConfig{Name: "lead", Provider: &agenttest.ScriptedProvider{}}, WithSubAgents(SubAgentDef{
		Name: "worker", Provider: child, MaxIter: 4, Tools: types.NewToolRegistry(lookupTool()),
	}))
	s, err := parent.InvokeSubAgent(context.Background(), "worker", "dig")
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	r, err := s.SubAgentResult()
	if err != nil || r.Forced || r.Iterations != 2 || r.ParentText() != "quick answer" {
		t.Fatalf("result = %+v, %v", r, err)
	}
	for i, req := range child.Requests() {
		if strings.Contains(requestBody(req), "<wrap_up>") {
			t.Fatalf("request %d has a wrap-up note", i)
		}
	}
}

// The policy can still make the limit an error.
func TestChildLimitErrorPolicy(t *testing.T) {
	parent := NewAgent(AgentConfig{Name: "lead", Provider: &agenttest.ScriptedProvider{}}, WithSubAgents(SubAgentDef{
		Name: "worker", Provider: loopingChild(6, "never"), MaxIter: 2, WrapUpAt: -1,
		Tools: types.NewToolRegistry(lookupTool()), Options: []AgentOption{WithOnMaxIter(MaxIterError)},
	}))
	s, err := parent.InvokeSubAgent(context.Background(), "worker", "dig")
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	if _, err := s.SubAgentResult(); !errors.Is(err, types.ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
}

// An orchestrator with no iteration cap runs past the default of 10 while
// its child stays bounded.
func TestUnboundedOrchestratorBoundedChild(t *testing.T) {
	parentProvider := &agenttest.ScriptedProvider{}
	for i := range 14 {
		parentProvider.Responses = append(parentProvider.Responses, agenttest.ToolCallResponse(fmt.Sprintf("p%d", i), "lookup", map[string]any{"n": i}))
	}
	parentProvider.Responses = append(parentProvider.Responses,
		agenttest.ToolCallResponse("d1", "delegate_to_worker", map[string]any{"task": "dig"}),
		agenttest.TextResponse("parent done"))
	child := loopingChild(DefaultSubAgentMaxIter, "child forced")
	sink := &resultSink{}
	parent := NewAgent(AgentConfig{
		Name: "lead", Provider: parentProvider, MaxIter: NoIterLimit, Tools: types.NewToolRegistry(lookupTool()),
		SubAgents: []SubAgentDef{{Name: "worker", Description: "w", Provider: child, ResultSink: sink, Tools: types.NewToolRegistry(lookupTool())}},
	})
	if parent.cfg.MaxIter != NoIterLimit {
		t.Fatalf("parent MaxIter = %d", parent.cfg.MaxIter)
	}
	stream := parent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	text := agenttest.CollectText(stream.Deltas())
	if err := stream.Wait(); err != nil || text != "parent done" {
		t.Fatalf("parent = %q, %v", text, err)
	}
	if n := parentProvider.CallCount(); n != 16 {
		t.Fatalf("parent calls = %d, want 16", n)
	}
	r := sink.only(t)
	if !r.Forced || r.MaxIter != DefaultSubAgentMaxIter || r.Iterations != DefaultSubAgentMaxIter+1 {
		t.Fatalf("child result = forced %v max %d iterations %d", r.Forced, r.MaxIter, r.Iterations)
	}
	if n := child.CallCount(); n != DefaultSubAgentMaxIter+1 {
		t.Fatalf("child calls = %d", n)
	}
}

// Siblings each get a private scratch: the same name holds different notes
// in each, neither reaches the parent's workspace, and the parent reads both
// through the results.
func TestSiblingScratchIsolation(t *testing.T) {
	parentWS := workspace.NewMemory()
	sinks := map[string]*resultSink{"a": {}, "b": {}}
	providers := map[string]*agenttest.ScriptedProvider{}
	var defs []SubAgentDef
	for _, name := range []string{"a", "b"} {
		providers[name] = &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.ToolCallResponse("w-"+name, workspace.WriteToolName, map[string]any{"name": "notes", "content": "notes of " + name}),
			agenttest.ToolCallResponse("r-"+name, workspace.ReadToolName, map[string]any{"ref": "notes"}),
			agenttest.TextResponse("done " + name),
		}}
		defs = append(defs, SubAgentDef{Name: name, Description: name, Provider: providers[name], ResultSink: sinks[name], Tools: types.NewToolRegistry(lookupTool())})
	}
	parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		{
			types.ToolCallStartDelta{ID: "da", Name: "delegate_to_a"},
			types.ToolCallArgumentDelta{Content: `{"task":"take notes"}`},
			types.ToolCallEndDelta{Arguments: map[string]any{"task": "take notes"}},
			types.ToolCallStartDelta{ID: "db", Name: "delegate_to_b"},
			types.ToolCallArgumentDelta{Content: `{"task":"take notes"}`},
			types.ToolCallEndDelta{Arguments: map[string]any{"task": "take notes"}},
		},
		agenttest.ToolCallResponse("s1", workspace.SearchArtifactToolName, map[string]any{"query": "notes"}),
		agenttest.TextResponse("parent done"),
	}}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: parentProvider, Workspace: parentWS, SubAgents: defs})
	stream := parent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
	agenttest.AssertNoErrors(t, agenttest.CollectDeltas(stream.Deltas()))
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"a", "b"} {
		reqs := providers[name].Requests()
		if got := toolResultIn(t, reqs[2], "r-"+name); got != "notes of "+name {
			t.Fatalf("child %s read its notes as %q", name, got)
		}
		r := sinks[name].only(t)
		if r.Scratch == nil {
			t.Fatalf("child %s result has no scratch", name)
		}
		got, err := r.Scratch.Read(context.Background(), workspace.Ref{Name: "notes"}, 0, 0)
		if err != nil || string(got) != "notes of "+name {
			t.Fatalf("result scratch of %s = %q, %v", name, got, err)
		}
		if _, err := r.Scratch.Put(context.Background(), "x", nil, nil); !errors.Is(err, workspace.ErrReadOnly) {
			t.Fatalf("result scratch is writable: %v", err)
		}
	}
	if refs, _ := parentWS.List(context.Background()); len(refs) != 0 {
		t.Fatalf("children wrote to the parent's workspace: %+v", refs)
	}
	// The parent's own run reads both children's scratch.
	found := toolResultIn(t, parentProvider.Requests()[2], "s1")
	if !strings.Contains(found, "notes of a") || !strings.Contains(found, "notes of b") {
		t.Fatalf("parent search_artifact = %q", found)
	}
}

// A large task goes to the child as a reference: the full text never
// appears in any child request, and the child reads the part it needs.
func TestLargeTaskPassedByReference(t *testing.T) {
	filler := strings.Repeat("Background paragraph about nothing in particular. ", 800)
	task := "Answer the question at the end of this document.\n" + filler + "\nThe secret code is ZEBRA-42.\n" + filler + "\nQuestion: what is the secret code?"
	uri := workspace.URIScheme + "://" + workspace.Digest([]byte(task))
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", workspace.SearchArtifactToolName, map[string]any{"query": "secret code is", "uri": uri}),
		agenttest.TextResponse("ZEBRA-42"),
	}}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: &agenttest.ScriptedProvider{}}, WithSubAgents(SubAgentDef{
		Name: "reader", Provider: child,
	}))
	s, err := parent.InvokeSubAgent(context.Background(), "reader", task)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	r, err := s.SubAgentResult()
	if err != nil || r.Output != "ZEBRA-42" || r.Task != task {
		t.Fatalf("result = %q, %v", r.Output, err)
	}
	reqs := child.Requests()
	for i, req := range reqs {
		body := requestBody(req)
		if strings.Contains(body, filler) || len(body) > 4000 {
			t.Fatalf("request %d carries the task inline (%d bytes)", i, len(body))
		}
	}
	first := requestBody(reqs[0])
	if !strings.Contains(first, uri) || !strings.Contains(first, "Answer the question") {
		t.Fatalf("first request lacks the reference and preview:\n%s", first)
	}
	if !strings.Contains(first, "tool:"+workspace.ReadArtifactToolName) || !strings.Contains(first, "tool:"+workspace.SearchArtifactToolName) {
		t.Fatalf("child tools = %v", toolNames(reqs[0]))
	}
	if got := toolResultIn(t, reqs[1], "s1"); !strings.Contains(got, "ZEBRA-42") {
		t.Fatalf("search_artifact = %q", got)
	}
	// The child's scratch holds the task for the host too.
	if got, err := r.Scratch.Read(context.Background(), workspace.Ref{Name: "inputs/task"}, 0, 0); err != nil || string(got) != task {
		t.Fatalf("scratch task = %d bytes, %v", len(got), err)
	}
}

// A large forked message is replaced by a reference too.
func TestLargeForkedMessagePassedByReference(t *testing.T) {
	big := strings.Repeat("forked history line. ", 2000)
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}
	parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_w", map[string]any{"task": "summarize"}),
		agenttest.TextResponse("done"),
	}}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: parentProvider}, WithSubAgents(SubAgentDef{
		Name: "w", Description: "w", Provider: child, Context: ContextFork,
	}))
	stream := parent.Invoke(context.Background(), []types.Message{types.NewUserMessage(big)})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	body := requestBody(child.Requests()[0])
	if strings.Contains(body, big) || !strings.Contains(body, workspace.URIScheme+"://"+workspace.Digest([]byte(big))) {
		t.Fatalf("forked message sent inline (%d bytes)", len(body))
	}
}

// Small inputs and outputs, and definitions that turn references off, stay
// inline.
func TestReferencesOffStaysInline(t *testing.T) {
	task := strings.Repeat("x ", 10000)
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(task)}}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: &agenttest.ScriptedProvider{}}, WithSubAgents(SubAgentDef{
		Name: "w", Provider: child, References: SubAgentReferences{Off: true},
	}))
	s, err := parent.InvokeSubAgent(context.Background(), "w", task)
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	r, err := s.SubAgentResult()
	if err != nil || r.OutputRef != "" || r.ParentText() != task {
		t.Fatalf("result ref %q, %v", r.OutputRef, err)
	}
	if !strings.Contains(requestBody(child.Requests()[0]), task) {
		t.Fatal("task not inline with references off")
	}
}

// A result over the threshold reaches the parent model as a reference with
// a preview, stored in the parent's workspace, while Output keeps all of it.
func TestLargeResultReturnedAsReference(t *testing.T) {
	big := "Report start. " + strings.Repeat("detail row with numbers 123 456. ", 1200) + "Report end."
	for _, tt := range []struct {
		name     string
		parentWS workspace.Workspace
	}{
		{"stored in the parent's workspace", workspace.NewMemory()},
		{"stored in the child's scratch without one", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			uri := workspace.URIScheme + "://" + workspace.Digest([]byte(big))
			child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(big)}}
			parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("d1", "delegate_to_w", map[string]any{"task": "report"}),
				agenttest.ToolCallResponse("r1", workspace.ReadArtifactToolName, map[string]any{"uri": uri, "offset": len(big) - 11}),
				agenttest.TextResponse("done"),
			}}
			sink := &resultSink{}
			parent := NewAgent(AgentConfig{Name: "lead", Provider: parentProvider, Workspace: tt.parentWS}, WithSubAgents(SubAgentDef{
				Name: "w", Description: "w", Provider: child, ResultSink: sink,
			}))
			stream := parent.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")})
			agenttest.AssertNoErrors(t, agenttest.CollectDeltas(stream.Deltas()))
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			reqs := parentProvider.Requests()
			got := toolResultIn(t, reqs[1], "d1")
			if strings.Contains(got, "Report end.") || len(got) > 2000 || !strings.Contains(got, uri) || !strings.HasPrefix(strings.SplitN(got, "Preview:]\n", 2)[1], "Report start.") {
				t.Fatalf("parent received %d bytes:\n%.600s", len(got), got)
			}
			if !strings.Contains(requestBody(reqs[0]), "tool:"+workspace.ReadArtifactToolName) {
				t.Fatalf("parent tools = %v", toolNames(reqs[0]))
			}
			if tail := toolResultIn(t, reqs[2], "r1"); tail != "Report end." {
				t.Fatalf("read_artifact = %q", tail)
			}
			r := sink.only(t)
			if r.Output != big || r.OutputRef != uri {
				t.Fatalf("result output %d bytes, ref %q", len(r.Output), r.OutputRef)
			}
			if tt.parentWS != nil {
				if _, err := tt.parentWS.Stat(context.Background(), workspace.Ref{Name: "subagents/w/d1/result"}); err != nil {
					t.Fatalf("result not in the parent's workspace: %v", err)
				}
			}
		})
	}
}

// A spilling tool built for the parent spills into the child's private
// scratch when the child runs it, instead of failing on the parent's
// read-only view, and the parent reads the spill through the result.
func TestChildSpillsIntoItsScratch(t *testing.T) {
	parentWS := workspace.NewMemory()
	big := strings.Repeat("spilled line\n", 400)
	bigTool := &agenttest.MockTool{Def: types.ToolDef{Name: "dump"}, Result: big}
	spilling := workspace.Spill(bigTool, parentWS, workspace.SpillOptions{MaxBytes: 500})
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "dump", map[string]any{}),
		agenttest.TextResponse("dumped"),
	}}
	sink := &resultSink{}
	parent := NewAgent(AgentConfig{Name: "lead", Provider: &agenttest.ScriptedProvider{}, Workspace: parentWS}, WithSubAgents(SubAgentDef{
		Name: "w", Provider: child, ResultSink: sink, Tools: types.NewToolRegistry(spilling),
	}))
	s, err := parent.InvokeSubAgent(context.Background(), "w", "dump it")
	if err != nil {
		t.Fatal(err)
	}
	for range s.Deltas() {
	}
	if _, err := s.SubAgentResult(); err != nil {
		t.Fatal(err)
	}
	got := toolResultIn(t, child.Requests()[1], "c1")
	if !strings.Contains(got, "result truncated") || len(got) >= len(big) {
		t.Fatalf("child saw %d bytes, want a spilled preview", len(got))
	}
	if refs, _ := parentWS.List(context.Background()); len(refs) != 0 {
		t.Fatalf("spill reached the parent's workspace: %+v", refs)
	}
	r := sink.only(t)
	if data, err := r.Scratch.Read(context.Background(), workspace.Ref{ID: workspace.Digest([]byte(big))}, 0, 0); err != nil || string(data) != big {
		t.Fatalf("parent read of the spill = %d bytes, %v", len(data), err)
	}
}

// agent.Ref stores a tool's large value in the call's workspace and returns
// a reference instead.
func TestRefFromTool(t *testing.T) {
	ws := workspace.NewMemory()
	data := strings.Repeat("row\n", 5000)
	var out string
	tool := &types.ToolFunc{Def: types.ToolDef{Name: "export"}, Fn: func(ctx context.Context, _ map[string]any) (string, error) {
		var err error
		out, err = Ref(ctx, "exports/rows.txt", []byte(data))
		return out, err
	}}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("e1", "export", map[string]any{}),
		agenttest.TextResponse("done"),
	}}
	a := NewAgent(AgentConfig{Provider: provider, Tools: types.NewToolRegistry(tool), Workspace: ws})
	agenttest.AssertNoErrors(t, agenttest.CollectDeltas(a.Invoke(context.Background(), []types.Message{types.NewUserMessage("go")}).Deltas()))
	if len(out) > 1500 || !strings.Contains(out, workspace.URIScheme+"://") {
		t.Fatalf("Ref returned %d bytes", len(out))
	}
	if got, _ := ws.Read(context.Background(), workspace.Ref{Name: "exports/rows.txt"}, 0, 0); string(got) != data {
		t.Fatal("Ref stored different data")
	}
	if _, err := Ref(context.Background(), "x", nil); err == nil {
		t.Fatal("Ref without a workspace succeeded")
	}
}

// A spawned child's large result arrives as a reference, and its handle
// gives the host the child's scratch.
func TestSpawnedLargeResultByReference(t *testing.T) {
	big := "Spawn report. " + strings.Repeat("spawned row. ", 2000)
	uri := workspace.URIScheme + "://" + workspace.Digest([]byte(big))
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "research"}),
		agenttest.TextResponse("waiting"),
		agenttest.TextResponse("final"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(big)}}
	run := runSpawn(t, parent, child, SubAgentDef{}, nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	msg := lastUserText(parent, 2)
	if strings.Contains(msg, big) || !strings.Contains(msg, uri) || !strings.Contains(msg, `iterations="1"`) {
		t.Fatalf("injected %d bytes:\n%.400s", len(msg), msg)
	}
	h := run.stream.SubAgents()[0]
	if h.Scratch() == nil {
		t.Fatal("handle has no scratch")
	}
	if data, err := h.Scratch().Read(context.Background(), workspace.Ref{ID: workspace.Digest([]byte(big))}, 0, 0); err != nil || string(data) != big {
		t.Fatalf("handle scratch read = %d bytes, %v", len(data), err)
	}
}
