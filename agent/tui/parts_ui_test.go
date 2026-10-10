package tui

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

var update = flag.Bool("update", false, "rewrite golden files")

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func plain(s string) string {
	lines := strings.Split(ansi.ReplaceAllString(s, ""), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

func checkTUIGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// partsTurn is a model turn with reasoning, a tool call, text, produced
// media, citations and a refusal.
func partsTurn() [][]types.Delta {
	var final []types.Delta
	final = append(final, types.PartDeltas(0, types.ThinkingPart{Text: "The user wants a chart of the data."})...)
	final = append(final, types.PartDeltas(0, types.Text("Here is the chart you asked for."))...)
	img := types.Artifact(types.ArtifactScheme+strings.Repeat("9f1c", 16), "image/png")
	img.Size, img.Filename = 120<<10, "chart.png"
	final = append(final,
		types.PartStart{Index: 0, Kind: types.KindImageOut, MediaType: "image/png"},
		types.PartDelta{Index: 0, Data: make([]byte, 2048)},
		types.PartEnd{Index: 0, Part: types.ImageOutPart{Source: img, ImageMeta: types.ImageMeta{Width: 1024, Height: 768}}},
	)
	final = append(final, types.PartDeltas(0, types.CitationPart{Citation: types.Citation{Title: "Dataset", URI: "https://example.com/data"}})...)
	final = append(final, types.PartDeltas(0, types.CitationPart{Citation: types.Citation{Title: "Method", URI: "https://example.com/method"}})...)
	final = append(final, types.PartDeltas(0, types.RefusalPart{Text: "I won't share the raw rows.", Category: "privacy"})...)
	return [][]types.Delta{
		{
			types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "chart"},
			types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: "c1", Name: "chart", Arguments: map[string]any{"kind": "bar"}}},
		},
		final,
	}
}

// chartTool returns text and an image.
type chartTool struct{}

func (chartTool) Definition() types.ToolDef {
	return types.ToolDef{Name: "chart", Parameters: types.ParameterSchema{Type: "object"}}
}

func (c chartTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := c.ExecuteRich(ctx, args)
	return r.Text(), err
}

func (chartTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	src := types.Bytes("image/png", make([]byte, 5000))
	src.Filename = "bars.png"
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text("drawn"), types.Image(src, types.ImageMeta{Width: 64, Height: 32})}}, nil
}

func partsLoop(t *testing.T, tmpl Template) *loop {
	t.Helper()
	a := must.Get(agentsdk.New(agentsdk.Config{
		Name: "demo", Provider: &gatedProvider{responses: partsTurn(), hold: -1}, Tools: types.NewToolRegistry(chartTool{}),
	}))
	m := newRunnerModel(a, context.Background(), tmpl)
	m.header = AgentHeader{Name: "demo", Provider: "scripted", Tools: []string{"chart"}}
	model, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 40})
	l := &loop{t: t, m: model.(runnerModel), msgs: make(chan tea.Msg, 256)}
	l.typeText("chart the data")
	l.key(tea.KeyEnter)
	l.until("the turn ends", idle)
	return l
}

func TestTUIRendersPartsSnapshot(t *testing.T) {
	tmpl := TemplateDefault
	tmpl.RenderMarkdown = false
	tmpl.ShowUsage = false
	l := partsLoop(t, tmpl)
	checkTUIGolden(t, "parts_view.golden", plain(l.m.View())+"\n")

	t.Run("ctrl+t expands reasoning", func(t *testing.T) {
		l.send(tea.KeyMsg{Type: tea.KeyCtrlT})
		if v := plain(l.m.View()); !strings.Contains(v, "The user wants a chart of the data.") || strings.Contains(v, "ctrl+t expands") {
			t.Fatalf("reasoning not expanded:\n%s", v)
		}
		l.send(tea.KeyMsg{Type: tea.KeyCtrlT})
		if v := plain(l.m.View()); strings.Contains(v, "The user wants a chart") {
			t.Fatalf("reasoning not collapsed again:\n%s", v)
		}
	})
}

func TestTUIFilter(t *testing.T) {
	tmpl := TemplateDefault
	tmpl.RenderMarkdown = false
	l := partsLoop(t, tmpl)

	l.send(tea.KeyMsg{Type: tea.KeyCtrlF})
	if !l.m.filtering {
		t.Fatal("ctrl+f did not open the filter")
	}
	l.typeText("kind:media,citation")
	view := plain(l.m.View())
	if !strings.Contains(view, iconFilter+" kind:media,citation") {
		t.Fatalf("header has no filter badge:\n%s", view)
	}
	log := plain(l.m.logView())
	for _, want := range []string{"chart.png", "[1] Dataset", "[2] Method"} {
		if !strings.Contains(log, want) {
			t.Errorf("filtered log lacks %q:\n%s", want, log)
		}
	}
	for _, hidden := range []string{"you >", "Here is the chart", "declined"} {
		if strings.Contains(log, hidden) {
			t.Errorf("filtered log shows %q:\n%s", hidden, log)
		}
	}
	if !strings.Contains(view, "3/") {
		t.Errorf("badge does not count the shown entries:\n%s", view)
	}

	// Enter keeps the filter and returns to the message input.
	l.key(tea.KeyEnter)
	if l.m.filtering || !l.m.filter.active() || !l.m.textInput.Focused() {
		t.Fatalf("enter: filtering %v, active %v", l.m.filtering, l.m.filter.active())
	}

	// Words narrow further, ignoring case.
	l.send(tea.KeyMsg{Type: tea.KeyCtrlF})
	for range len("kind:media,citation") {
		l.key(tea.KeyBackspace)
	}
	l.typeText("METHOD")
	if log := plain(l.m.logView()); !strings.Contains(log, "Method") || strings.Contains(log, "Dataset") {
		t.Fatalf("word filter:\n%s", log)
	}
	l.typeText(" kind:bogus")
	if view := plain(l.m.View()); !strings.Contains(view, "unknown kind bogus") {
		t.Fatalf("unknown kind not reported:\n%s", view)
	}

	// Esc clears it.
	l.key(tea.KeyEsc)
	if l.m.filter.active() || l.m.filtering || strings.Contains(plain(l.m.View()), iconFilter) {
		t.Fatal("esc did not clear the filter")
	}
}

func TestParseFilter(t *testing.T) {
	tool := activityEntry{kind: activityTool, name: "read_file", args: map[string]any{"path": "/srv/app.go"}}
	text := activityEntry{kind: activityText, content: stringsBuilder("The answer is 42")}
	media := activityEntry{kind: activityMedia, media: &mediaInfo{kind: types.KindImageOut, mediaType: "image/png", filename: "plot.png"}}
	tests := []struct {
		q    string
		want []bool // tool, text, media
	}{
		{"", []bool{true, true, true}},
		{"kind:tool", []bool{true, false, false}},
		{"k:text,media", []bool{false, true, true}},
		{"answer", []bool{false, true, false}},
		{"APP.GO", []bool{true, false, false}},
		{"kind:media png", []bool{false, false, true}},
		{"kind:nope", []bool{false, false, false}},
		{"kind:tool answer", []bool{false, false, false}},
	}
	for _, tt := range tests {
		f := parseFilter(tt.q)
		for i, e := range []activityEntry{tool, text, media} {
			if got := f.match(e); got != tt.want[i] {
				t.Errorf("filter %q on %s = %v, want %v", tt.q, entryKind(e), got, tt.want[i])
			}
		}
	}
}

func stringsBuilder(s string) *strings.Builder {
	b := &strings.Builder{}
	b.WriteString(s)
	return b
}

// scrollModel is an idle runner with a transcript longer than its view.
func scrollModel(t *testing.T, entries int, motion bool) runnerModel {
	t.Helper()
	a := must.Get(agentsdk.New(agentsdk.Config{Name: "t", Provider: &gatedProvider{hold: -1}}))
	m := newRunnerModel(a, context.Background(), TemplateMinimal).withMotion(motion)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	m = model.(runnerModel)
	for i := range entries {
		m.act.addUser(fmt.Sprintf("line %d", i), false)
	}
	m.refresh()
	return m
}

func press(m runnerModel, k tea.KeyType) (runnerModel, tea.Cmd) {
	model, cmd := m.Update(tea.KeyMsg{Type: k})
	return model.(runnerModel), cmd
}

func TestTUIScrollKeys(t *testing.T) {
	m := scrollModel(t, 60, false)
	bottom := m.scroll.maxOffset()
	if m.scroll.vp.YOffset != bottom || !m.scroll.follow {
		t.Fatalf("not following the tail: offset %d of %d", m.scroll.vp.YOffset, bottom)
	}
	h := m.scroll.vp.Height
	tests := []struct {
		name  string
		key   tea.KeyType
		input string
		want  func(before int) int
	}{
		{name: "page up", key: tea.KeyPgUp, want: func(b int) int { return b - h }},
		{name: "half page up", key: tea.KeyShiftUp, want: func(b int) int { return b - h/2 }},
		{name: "line up", key: tea.KeyUp, want: func(b int) int { return b - 1 }},
		{name: "top", key: tea.KeyCtrlHome, want: func(int) int { return 0 }},
		{name: "home on an empty input", key: tea.KeyHome, want: func(int) int { return 0 }},
		{name: "ctrl+u on an empty input", key: tea.KeyCtrlU, want: func(b int) int { return b - h/2 }},
		{name: "home edits a non-empty input", key: tea.KeyHome, input: "abc", want: func(b int) int { return b }},
		{name: "ctrl+u edits a non-empty input", key: tea.KeyCtrlU, input: "abc", want: func(b int) int { return b }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := scrollModel(t, 60, false)
			m.textInput.SetValue(tt.input)
			before := m.scroll.vp.YOffset
			m, _ = press(m, tt.key)
			if got, want := m.scroll.vp.YOffset, max(tt.want(before), 0); got != want {
				t.Fatalf("offset = %d, want %d", got, want)
			}
		})
	}

	t.Run("mouse wheel", func(t *testing.T) {
		m := scrollModel(t, 60, false)
		before := m.scroll.vp.YOffset
		model, _ := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
		m = model.(runnerModel)
		if m.scroll.vp.YOffset != before-wheelLines || m.scroll.follow {
			t.Fatalf("wheel up: offset %d, follow %v", m.scroll.vp.YOffset, m.scroll.follow)
		}
		model, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
		m = model.(runnerModel)
		if m.scroll.vp.YOffset != before || !m.scroll.follow {
			t.Fatalf("wheel down: offset %d, follow %v", m.scroll.vp.YOffset, m.scroll.follow)
		}
	})
}

func TestTUIFollowTail(t *testing.T) {
	m := scrollModel(t, 60, false)
	m, _ = press(m, tea.KeyPgUp)
	paused := m.scroll.vp.YOffset
	if m.scroll.follow {
		t.Fatal("scrolling up did not pause following")
	}
	if ind := m.scroll.indicator("ctrl+end"); ind != "" {
		t.Fatalf("indicator before anything new = %q", ind)
	}

	m.act.addUser("new one", false)
	m.act.addUser("new two", false)
	m.refresh()
	if m.scroll.vp.YOffset != paused {
		t.Fatal("new output moved a paused view")
	}
	if v := plain(m.View()); !strings.Contains(v, "↓ 2 new messages (ctrl+end follows)") {
		t.Fatalf("no new messages indicator:\n%s", v)
	}

	// Text streaming into an entry already shown counts as more below.
	m2 := scrollModel(t, 60, false)
	m2.act.apply(types.PartDelta{Index: 0, Text: "start"})
	m2.refresh()
	m2, _ = press(m2, tea.KeyPgUp)
	m2.act.apply(types.PartDelta{Index: 0, Text: "\nmore\nlines"})
	m2.refresh()
	if ind := m2.scroll.indicator("ctrl+end"); !strings.Contains(ind, "more below") {
		t.Fatalf("indicator = %q", ind)
	}

	// Reaching the bottom resumes following and clears the indicator.
	m, _ = press(m, tea.KeyCtrlEnd)
	if !m.scroll.follow || m.scroll.indicator("ctrl+end") != "" {
		t.Fatalf("ctrl+end: follow %v, indicator %q", m.scroll.follow, m.scroll.indicator("ctrl+end"))
	}
	m.act.addUser("after", false)
	m.refresh()
	if m.scroll.vp.YOffset != m.scroll.maxOffset() {
		t.Fatal("following did not resume")
	}

	// Paging down to the bottom resumes it too.
	m, _ = press(m, tea.KeyPgUp)
	m, _ = press(m, tea.KeyPgDown)
	if !m.scroll.follow {
		t.Fatal("paging back to the bottom did not resume following")
	}
}

func TestTUISmoothScroll(t *testing.T) {
	m := scrollModel(t, 60, true)
	start := m.scroll.vp.YOffset
	m, cmd := press(m, tea.KeyPgUp)
	if cmd == nil {
		t.Fatal("a page jump with motion on scheduled no frame")
	}
	target := start - m.scroll.vp.Height
	if m.scroll.vp.YOffset == target {
		t.Fatal("a page jump with motion on did not ease")
	}
	// A second jump while moving retargets without another timer.
	if m2, cmd2 := press(m, tea.KeyPgUp); cmd2 != nil || m2.scroll.target != target-m.scroll.vp.Height {
		t.Fatalf("retarget: cmd %v, target %d", cmd2 != nil, m2.scroll.target)
	}
	prev := m.scroll.vp.YOffset
	for frames := 0; cmd != nil; frames++ {
		if frames > 50 {
			t.Fatal("smooth scroll never arrived")
		}
		var model tea.Model
		model, cmd = m.Update(scrollTickMsg{})
		m = model.(runnerModel)
		if m.scroll.vp.YOffset > prev {
			t.Fatal("smooth scroll moved backwards")
		}
		prev = m.scroll.vp.YOffset
	}
	if m.scroll.vp.YOffset != target {
		t.Fatalf("arrived at %d, want %d", m.scroll.vp.YOffset, target)
	}

	// Without motion the same jump is immediate.
	still := scrollModel(t, 60, false)
	still, cmd = press(still, tea.KeyPgUp)
	if cmd != nil || still.scroll.vp.YOffset != target {
		t.Fatalf("no-motion jump: cmd %v offset %d", cmd != nil, still.scroll.vp.YOffset)
	}
}

func TestMotionEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SAIGE_REDUCED_MOTION", "")
	t.Setenv("TERM", "xterm-256color")
	if !MotionEnabled(false) {
		t.Fatal("motion off with a clean environment")
	}
	if MotionEnabled(true) {
		t.Fatal("--no-animation did not turn motion off")
	}
	for env, val := range map[string]string{"NO_COLOR": "1", "SAIGE_REDUCED_MOTION": "1", "TERM": "dumb"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, val)
			if MotionEnabled(false) {
				t.Fatalf("%s=%s left motion on", env, val)
			}
		})
	}
}

func TestTUIMotionOff(t *testing.T) {
	m := scrollModel(t, 3, false)
	if m.spin() != staticSpinner {
		t.Fatalf("spinner = %q with motion off", m.spin())
	}
	if m.fading() {
		t.Fatal("entries fade with motion off")
	}
	now := time.Unix(100, 0)
	m = m.withMotion(true)
	m.clock = func() time.Time { return now }
	m.act.now = m.clock
	m.act.addUser("fresh", false)
	if !m.fading() {
		t.Fatal("a new entry is not fading in")
	}
	now = now.Add(fadeFor)
	if m.fading() {
		t.Fatal("an entry still fades after fadeFor")
	}
}

func TestStreamModelScrollAndFilter(t *testing.T) {
	ch := make(chan types.Delta)
	m := NewStreamModel(AgentHeader{}, ch, TemplateMinimal)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m = model.(StreamModel)
	for i := range 40 {
		m.act.addUser(fmt.Sprintf("row %d", i), false)
	}
	m.act.addCitation(types.Citation{Title: "Only source"})
	m.refresh()
	key := func(s string) {
		var msg tea.KeyMsg
		switch s {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
		}
		model, _ := m.Update(msg)
		m = model.(StreamModel)
	}
	key("g")
	if m.scroll.vp.YOffset != 0 || m.scroll.follow {
		t.Fatal("g did not go to the top")
	}
	key("j")
	if m.scroll.vp.YOffset != 1 {
		t.Fatalf("j: offset %d", m.scroll.vp.YOffset)
	}
	key("G")
	if !m.scroll.follow {
		t.Fatal("G did not resume following")
	}
	key("/")
	for _, r := range "kind:citation" {
		key(string(r))
	}
	if !strings.Contains(plain(m.View()), iconFilter+" kind:citation  1/41") {
		t.Fatalf("filter badge:\n%s", plain(m.View()))
	}
	key("q") // typed into the filter, not a quit
	key("enter")
	if m.filtering || !strings.Contains(m.filter.raw, "kind:citation") {
		t.Fatalf("enter: filtering %v filter %q", m.filtering, m.filter.raw)
	}
	key("esc")
	if m.filter.active() {
		t.Fatal("esc did not clear the filter")
	}
}

func TestMediaLabel(t *testing.T) {
	tests := []struct {
		m    mediaInfo
		want string
	}{
		{mediaInfo{mediaType: "image/png", streaming: true, size: 120 << 10}, "[image/png streaming… 120 KB]"},
		{mediaInfo{mediaType: "audio/wav", streaming: true}, "[audio/wav streaming…]"},
		{
			mediaInfo{kind: types.KindImageOut, mediaType: "image/png", width: 1024, height: 768, size: 1536 << 10, filename: "a.png",
				locator: types.ArtifactScheme + strings.Repeat("ab", 32)},
			"[image image/png 1024x768 · 1.5 MB · a.png · saige-artifact://abababababab…]",
		},
		{mediaInfo{kind: types.KindDocument, mediaType: "application/pdf", size: 900, unresolved: "storage full"}, "[document application/pdf · 900 B · not kept: storage full]"},
		{mediaInfo{kind: types.KindAudioOut, mediaType: "audio/mp3", duration: 2500 * time.Millisecond, transcript: "hi there"}, "[audio audio/mp3 · 2.5s] hi there"},
		{mediaInfo{kind: types.KindFile, digest: strings.Repeat("c", 64)}, "[file unknown type · sha256:cccccccccccc…]"},
	}
	for _, tt := range tests {
		if got := tt.m.label(); got != tt.want {
			t.Errorf("label = %q, want %q", got, tt.want)
		}
	}
}

func TestVerboseRendersParts(t *testing.T) {
	ch := make(chan types.Delta, 64)
	for _, d := range partsTurn()[1] {
		ch <- d
	}
	ch <- types.ToolExecEndDelta{ToolCallID: "c1", Name: "chart", Parts: []types.ToolOutputPart{types.Image(types.Bytes("image/png", []byte("x")))}}
	ch <- types.DoneDelta{}
	close(ch)
	var out bytes.Buffer
	tmpl := TemplateDefault
	tmpl.ShowHeader = false
	StreamVerboseWithTemplate(AgentHeader{}, ch, &out, tmpl)
	got := plain(out.String())
	for _, want := range []string{
		"Here is the chart you asked for.",
		"[image image/png 1024x768 · 120 KB · chart.png · saige-artifact://9f1c9f1c9f1c…]",
		"[1] Dataset https://example.com/data",
		"[2] Method https://example.com/method",
		iconRefusal + " declined: I won't share the raw rows.",
		"[image image/png · 1 B · sha256:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, got)
		}
	}
}
