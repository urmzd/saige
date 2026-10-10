package tui

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/urmzd/saige/agent/types"
)

// tools returns the tool entries of a, in order.
func tools(a activity) []activityEntry {
	var out []activityEntry
	for _, e := range a.entries {
		if e.kind == activityTool {
			out = append(out, e)
		}
	}
	return out
}

func TestActivityToolLifecycle(t *testing.T) {
	type want struct {
		name   string
		agent  bool
		status toolStatus
		errMsg string
	}
	tests := []struct {
		name      string
		subAgents []string
		deltas    []types.Delta
		want      []want
	}{
		{
			name: "plain tool is not a delegation and is done only after execution",
			deltas: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "search"},
				types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{"q": "go"}}},
			},
			want: []want{{name: "search", status: toolReady}},
		},
		{
			name: "plain tool executes",
			deltas: []types.Delta{
				types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "search"},
				types.PartEnd{Index: 1, Part: types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{}}},
				types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
				types.ToolExecEndDelta{ToolCallID: "c1", Name: "search", Result: "ok"},
			},
			want: []want{{name: "search", status: toolDone}},
		},
		{
			name: "tool error is kept",
			deltas: []types.Delta{
				types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "c1", Name: "fetch"},
				types.ToolExecStartDelta{ToolCallID: "c1", Name: "fetch"},
				types.ToolExecEndDelta{ToolCallID: "c1", Name: "fetch", Error: "404"},
			},
			want: []want{{name: "fetch", status: toolFailed, errMsg: "404"}},
		},
		{
			name: "parallel calls pair by ID, not position",
			deltas: []types.Delta{
				types.PartStart{Index: 3, Kind: types.KindToolCall, ID: "a", Name: "one"},
				types.PartStart{Index: 4, Kind: types.KindToolCall, ID: "b", Name: "two"},
				types.PartEnd{Index: 4, Part: types.ToolCallPart{ID: "b", Name: "two", Arguments: map[string]any{}}},
				types.PartEnd{Index: 3, Part: types.ToolCallPart{ID: "a", Name: "one", Arguments: map[string]any{}}},
				types.ToolExecStartDelta{ToolCallID: "b", Name: "two"},
				types.ToolExecStartDelta{ToolCallID: "a", Name: "one"},
				types.ToolExecEndDelta{ToolCallID: "a", Error: "boom"},
				types.ToolExecEndDelta{ToolCallID: "b"},
			},
			want: []want{
				{name: "one", status: toolFailed, errMsg: "boom"},
				{name: "two", status: toolDone},
			},
		},
		{
			name: "end closes the call at its index",
			deltas: []types.Delta{
				types.PartStart{Index: 5, Kind: types.KindToolCall, ID: "a", Name: "one"},
				types.PartStart{Index: 6, Kind: types.KindToolCall, ID: "b", Name: "two"},
				types.PartEnd{Index: 5},
			},
			want: []want{
				{name: "one", status: toolReady},
				{name: "two", status: toolPending},
			},
		},
		{
			name: "delegation prefix marks a sub-agent",
			deltas: []types.Delta{
				types.PartStart{Index: 7, Kind: types.KindToolCall, ID: "c1", Name: "delegate_to_researcher"},
				types.ToolExecStartDelta{ToolCallID: "c1", Name: "delegate_to_researcher"},
				types.ToolExecEndDelta{ToolCallID: "c1"},
			},
			want: []want{{name: "delegate_to_researcher", agent: true, status: toolDone}},
		},
		{
			name:      "registered sub-agent name marks a sub-agent",
			subAgents: []string{"helper"},
			deltas: []types.Delta{
				types.ToolExecStartDelta{ToolCallID: "c1", Name: "helper"},
			},
			want: []want{{name: "helper", agent: true, status: toolRunning}},
		},
		{
			name: "execution without a start still gets an entry",
			deltas: []types.Delta{
				types.ToolExecStartDelta{ToolCallID: "c9", Name: "forced"},
				types.ToolExecEndDelta{ToolCallID: "c9", Name: "forced"},
			},
			want: []want{{name: "forced", status: toolDone}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newActivity(tt.subAgents)
			for _, d := range tt.deltas {
				a.apply(d)
			}
			got := tools(a)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tool entries, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				g := got[i]
				if g.name != w.name || g.agent != w.agent || g.status != w.status || g.errMsg != w.errMsg {
					t.Fatalf("entry %d = {name:%q agent:%v status:%v err:%q}, want %+v", i, g.name, g.agent, g.status, g.errMsg, w)
				}
			}
		})
	}
}

func TestActivityNestedOutput(t *testing.T) {
	a := newActivity(nil)
	a.apply(types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "delegate_to_child"})
	a.apply(types.ToolExecStartDelta{ToolCallID: "c1", Name: "delegate_to_child"})
	a.apply(types.ToolExecDelta{ToolCallID: "c1", Inner: types.PartDelta{Index: 1, Text: "looking"}})
	a.apply(types.ToolExecDelta{ToolCallID: "c1", Inner: types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "n1", Name: "grep"}})
	a.apply(types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolExecEndDelta{ToolCallID: "n1", Name: "grep", Error: "denied"}})
	// A grandchild's text is attributed to the outermost call.
	a.apply(types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolExecDelta{ToolCallID: "g1", Inner: types.PartDelta{Index: 1, Text: "deep"}}})

	got := tools(a)
	if len(got) != 1 {
		t.Fatalf("got %d tool entries, want 1", len(got))
	}
	out := got[0].content.String()
	for _, want := range []string{"looking\n", iconTool + " grep\n", iconError + " grep: denied\n", "deep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("nested output %q missing %q", out, want)
		}
	}
}

func TestActivityErrorsAndFinish(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantError bool
	}{
		{"provider error is shown", errors.New("rate limited"), true},
		{"cancellation is not an error", fmt.Errorf("run: %w", types.ErrStreamCanceled), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newActivity(nil)
			a.apply(types.PartDelta{Index: 0, Text: "partial"})
			a.apply(types.ToolExecStartDelta{ToolCallID: "c1", Name: "slow"})
			a.apply(types.ErrorDelta{Error: tt.err})
			a.finish()

			var gotError bool
			for _, e := range a.entries {
				switch e.kind {
				case activityError:
					gotError = true
				case activityText:
					if !e.final {
						t.Fatal("text not final after finish")
					}
				case activityTool:
					if e.status != toolStopped {
						t.Fatalf("running tool status = %v after finish, want stopped", e.status)
					}
				}
			}
			if gotError != tt.wantError {
				t.Fatalf("error entry = %v, want %v", gotError, tt.wantError)
			}
		})
	}
}

func renderEntries(a activity, tmpl Template) string {
	return logRenderer{entries: a.entries, spinner: spinner.New(), template: tmpl}.renderLog()
}

func TestLogRendererTools(t *testing.T) {
	a := newActivity(nil)
	a.apply(types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "search"})
	a.apply(types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{"query": "golang"}}})
	a.apply(types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"})
	a.apply(types.ToolExecEndDelta{ToolCallID: "c1", Result: "result line"})
	a.apply(types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c2", Name: "fetch"})
	a.apply(types.ToolExecStartDelta{ToolCallID: "c2", Name: "fetch"})
	a.apply(types.ToolExecEndDelta{ToolCallID: "c2", Error: "timeout"})

	tests := []struct {
		name    string
		tmpl    Template
		want    []string
		notWant []string
	}{
		{
			name:    "default shows status, hides args and results",
			tmpl:    TemplateDefault,
			want:    []string{"search", iconDone, "fetch", "timeout"},
			notWant: []string{iconAgent, "golang", "result line"},
		},
		{
			name: "detailed shows args and results",
			tmpl: TemplateDetailed,
			want: []string{`"query":"golang"`, "result line", "timeout"},
		},
		{
			name:    "minimal hides tools",
			tmpl:    TemplateMinimal,
			notWant: []string{"search", "fetch"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := renderEntries(a, tt.tmpl)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Fatalf("render missing %q:\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Fatalf("render contains %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestLogRendererKeepsAllStreamingText(t *testing.T) {
	a := newActivity(nil)
	for i := range 20 {
		a.apply(types.PartDelta{Index: 0, Text: fmt.Sprintf("line %d\n", i)})
	}
	out := renderEntries(a, TemplateDefault)
	for _, w := range []string{"line 0", "line 10", "line 19"} {
		if !strings.Contains(out, w) {
			t.Fatalf("streaming render dropped %q", w)
		}
	}
}

func TestTemplateDetailedDiffersFromDefault(t *testing.T) {
	if TemplateDetailed == (Template{Name: "detailed", ShowHeader: true, ShowToolCalls: true, ShowAgents: true, ShowUsage: true, ShowMarkers: true, ShowSpinner: true, ShowStreamText: true, RenderMarkdown: true}) {
		t.Fatal("detailed template adds nothing over the default")
	}
	d := TemplateDetailed
	d.Name = TemplateDefault.Name
	if d == TemplateDefault {
		t.Fatal("detailed template equals default apart from its name")
	}
}

func TestViewportHeight(t *testing.T) {
	tests := []struct {
		name   string
		total  int
		header string
		footer string
		want   int
	}{
		{"room to spare", 30, "a\nb", "c", 27},
		{"short terminal clamps to one", 3, "a\nb\nc", "d\ne", 1},
		{"zero size", 0, "", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := viewportHeight(tt.total, tt.header, tt.footer); got != tt.want {
				t.Fatalf("viewportHeight = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatArgs(t *testing.T) {
	many := map[string]any{}
	for i := range 30 {
		many[fmt.Sprintf("k%02d", i)] = i
	}
	tests := []struct {
		name     string
		args     map[string]any
		maxLines int
		want     []string
	}{
		{"nil", nil, 10, []string{"{}"}},
		{"sorted and unescaped", map[string]any{"b": "<x>", "a": 1}, 10, []string{"\"a\": 1", "\"b\": \"<x>\""}},
		{"capped", many, 5, []string{"... 27 more lines"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatArgs(tt.args, tt.maxLines)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Fatalf("FormatArgs = %q, missing %q", got, w)
				}
			}
			if tt.maxLines > 0 && strings.Count(got, "\n")+1 > tt.maxLines+1 {
				t.Fatalf("FormatArgs returned %d lines, cap %d", strings.Count(got, "\n")+1, tt.maxLines)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"truncated text", 8, "trunc..."},
		{"héllo wörld", 6, "hél..."},
	}
	for _, tt := range tests {
		if got := truncateRunes(tt.in, tt.max); got != tt.want {
			t.Fatalf("truncateRunes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestPromptApprovalShowsArguments(t *testing.T) {
	var out bytes.Buffer
	sc := bufioScanner("n\n")
	PromptApproval(sc, &out, types.MarkerDelta{ToolCallID: "c1", ToolName: "rag_delete", Arguments: map[string]any{"uuid": "doc-42"}})
	if !strings.Contains(out.String(), `"uuid": "doc-42"`) {
		t.Fatalf("approval prompt hides the arguments:\n%s", out.String())
	}
}

func TestJSONOutputWritesEnvelopes(t *testing.T) {
	ch := make(chan types.Delta, 8)
	ch <- types.PartDelta{Index: 0, Text: "hel"}
	ch <- types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "search"}
	ch <- types.MarkerDelta{ToolCallID: "c1", ToolName: "search", Arguments: map[string]any{"q": "x"}}
	ch <- types.UsageDelta{PromptTokens: 3, CompletionTokens: 4}
	ch <- types.PartDelta{Index: 0, Text: "lo"}
	ch <- types.DoneDelta{}
	close(ch)

	var out, errOut bytes.Buffer
	res := NewJSONOutput(&out, &errOut).StreamDeltas(AgentHeader{}, ch)
	if res.Err != nil || res.Text != "hello" {
		t.Fatalf("result = %+v, want text hello", res)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want one per delta:\n%s", len(lines), out.String())
	}
	wantKinds := []string{types.WirePartDelta, types.WirePartStart, types.WireMarker, types.WireUsage, types.WirePartDelta, types.WireDone}
	for i, line := range lines {
		env, err := types.UnmarshalEnvelope([]byte(line))
		if err != nil {
			t.Fatalf("line %d %q is not an envelope: %v", i, line, err)
		}
		if env.Kind != wantKinds[i] {
			t.Fatalf("line %d kind = %q, want %q", i, env.Kind, wantKinds[i])
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", errOut.String())
	}
}

func TestVerboseDoesNotShowPlainToolsAsDelegations(t *testing.T) {
	ch := make(chan types.Delta, 16)
	for _, d := range []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "search"},
		types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "search", Arguments: map[string]any{}}},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "search"},
		types.ToolExecEndDelta{ToolCallID: "c1", Name: "search", Error: "offline"},
		types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c2", Name: "delegate_to_writer"},
		types.ToolExecStartDelta{ToolCallID: "c2", Name: "delegate_to_writer"},
		types.ToolExecDelta{ToolCallID: "c2", Inner: types.PartDelta{Index: 0, Text: "draft\n"}},
		types.ToolExecEndDelta{ToolCallID: "c2"},
		types.DoneDelta{},
	} {
		ch <- d
	}
	close(ch)

	var out bytes.Buffer
	StreamVerboseWithTemplate(AgentHeader{}, ch, &out, TemplateDefault)
	got := out.String()
	if strings.Contains(got, "Delegating to search") {
		t.Fatalf("plain tool shown as a delegation:\n%s", got)
	}
	for _, w := range []string{"search: offline", "Delegating to writer", "[writer] ", "writer complete"} {
		if !strings.Contains(got, w) {
			t.Fatalf("verbose output missing %q:\n%s", w, got)
		}
	}
}

func TestStreamModelQuitCancels(t *testing.T) {
	tests := []struct {
		name       string
		done       bool
		wantCancel bool
	}{
		{"quit mid-stream cancels the run", false, true},
		{"quit after the stream ended does not", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canceled := false
			m := NewStreamModel(AgentHeader{}, make(chan types.Delta)).WithCancel(func() { canceled = true })
			m.done = tt.done
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			if cmd == nil {
				t.Fatal("ctrl+c did not quit")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("ctrl+c did not quit")
			}
			if canceled != tt.wantCancel {
				t.Fatalf("canceled = %v, want %v", canceled, tt.wantCancel)
			}
		})
	}
}

func TestStreamModelErrorKeepsTranscript(t *testing.T) {
	m := NewStreamModel(AgentHeader{}, make(chan types.Delta), TemplateMinimal)
	model, _ := m.Update(deltaMsg{delta: types.PartDelta{Index: 0, Text: "partial answer"}})
	model, cmd := model.Update(deltaMsg{delta: types.ErrorDelta{Error: errors.New("boom")}})
	sm := model.(StreamModel)
	if sm.Err() == nil || sm.FinalReport() != "partial answer" {
		t.Fatalf("err=%v report=%q", sm.Err(), sm.FinalReport())
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("stream model did not quit after the stream failed")
	}
}

func TestVerboseShowsNestedToolCalls(t *testing.T) {
	ch := make(chan types.Delta, 16)
	for _, d := range []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "delegate_to_writer"},
		types.ToolExecStartDelta{ToolCallID: "c1", Name: "delegate_to_writer"},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.PartDelta{Index: 0, Text: "looking"}},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "n1", Name: "grep"}},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolExecEndDelta{ToolCallID: "n1", Name: "grep", Error: "denied"}},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolExecDelta{ToolCallID: "g1", Inner: types.PartDelta{Index: 0, Text: "deep"}}},
		types.ToolExecDelta{ToolCallID: "c1", Inner: types.ToolExecDelta{ToolCallID: "g1", Inner: types.ErrorDelta{Error: errors.New("inner failed")}}},
		types.ToolExecEndDelta{ToolCallID: "c1"},
		types.DoneDelta{},
	} {
		ch <- d
	}
	close(ch)

	var out bytes.Buffer
	StreamVerboseWithTemplate(AgentHeader{}, ch, &out, TemplateDefault)
	got := out.String()
	for _, w := range []string{"looking", iconTool + " grep", "grep: denied", "deep", "inner failed", "writer complete"} {
		if !strings.Contains(got, w) {
			t.Fatalf("verbose output missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "Delegating to g1") || strings.Contains(got, "Delegating to grep") {
		t.Fatalf("nested call shown as a new top-level delegation:\n%s", got)
	}
}
