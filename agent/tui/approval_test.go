package tui

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

func TestParseApproval(t *testing.T) {
	tests := []struct {
		in       string
		approved bool
		valid    bool
	}{
		{"y", true, true},
		{"Y", true, true},
		{"yes", true, true},
		{" YES ", true, true},
		{"n", false, true},
		{"N", false, true},
		{"No", false, true},
		{"", false, false},
		{"   ", false, false},
		{"nope", false, false},
		{"yess", false, false},
		{"ok", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			approved, valid := ParseApproval(tt.in)
			if approved != tt.approved || valid != tt.valid {
				t.Fatalf("ParseApproval(%q) = (%v, %v), want (%v, %v)", tt.in, approved, valid, tt.approved, tt.valid)
			}
		})
	}
}

func TestPromptApproval(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    bool
		prompts int
	}{
		{"yes", "y\n", true, 1},
		{"no", "n\n", false, 1},
		{"empty then yes", "\ny\n", true, 2},
		{"typo then no", "yess\nno\n", false, 2},
		{"end of input denies", "", false, 1},
		{"garbage then end of input denies", "maybe\n", false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			sc := bufio.NewScanner(strings.NewReader(tt.input))
			got := PromptApproval(sc, &out, types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"})
			if got != tt.want {
				t.Fatalf("PromptApproval = %v, want %v", got, tt.want)
			}
			if n := strings.Count(out.String(), approvalPrompt); n != tt.prompts {
				t.Fatalf("prompted %d times, want %d; output:\n%s", n, tt.prompts, out.String())
			}
		})
	}
}

// plainOutput is an Output without inline marker support, used to exercise
// the forwarding fallback of StreamDeltasResolving.
type plainOutput struct{ *JSONOutput }

func (p plainOutput) StreamDeltas(h AgentHeader, ch <-chan types.Delta) VerboseResult {
	return p.JSONOutput.StreamDeltas(h, ch)
}

func TestStreamDeltasResolvingCallsResolver(t *testing.T) {
	outputs := map[string]func(*bytes.Buffer) Output{
		"styled":   func(b *bytes.Buffer) Output { return NewStyledOutput(b, b, TemplateMinimal) },
		"json":     func(b *bytes.Buffer) Output { return NewJSONOutput(b, b) },
		"fallback": func(b *bytes.Buffer) Output { return plainOutput{NewJSONOutput(b, b)} },
	}
	for name, mk := range outputs {
		t.Run(name, func(t *testing.T) {
			ch := make(chan types.Delta, 4)
			ch <- types.PartDelta{Index: 0, Text: "hi"}
			ch <- types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"}
			ch <- types.MarkerDelta{ToolCallID: "c2", ToolName: "danger"}
			ch <- types.DoneDelta{}
			close(ch)

			var buf bytes.Buffer
			var got []string
			res := StreamDeltasResolving(mk(&buf), AgentHeader{}, ch, func(d types.MarkerDelta) {
				got = append(got, d.ToolCallID)
			})
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			if strings.Join(got, ",") != "c1,c2" {
				t.Fatalf("resolved %v, want [c1 c2]", got)
			}
		})
	}
}

// markedAgent returns an agent whose first turn calls a tool that carries an
// approval marker, and whose second turn answers with text. The returned
// counter reports how many times the tool actually ran.
func markedAgent() (*agentsdk.Agent, *atomic.Int32) {
	var calls atomic.Int32
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "danger", Description: "destructive"},
		Fn: func(context.Context, map[string]any) (string, error) {
			calls.Add(1)
			return "deleted", nil
		},
	}
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "danger", map[string]any{}),
		agenttest.TextResponse("finished"),
	}}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name:     "test",
		Provider: provider,
		Tools:    types.NewToolRegistry(types.WithMarkers(tool, types.Marker{Kind: "human_approval", Message: "needs approval"})),
	})
	return a, &calls
}

func TestRunVerboseResolvesMarkers(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCalls int32
	}{
		{"deny", "delete it\nn\n", 0},
		{"approve", "delete it\ny\n", 1},
		{"empty answer asks again then denies", "delete it\n\nno\n", 0},
		{"end of input denies", "delete it\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			a, calls := markedAgent()
			var out bytes.Buffer
			r := &Runner{Verbose: true, Template: TemplateMinimal, In: strings.NewReader(tt.input), Out: &out}
			r.Output = NewStyledOutput(&out, &out, TemplateMinimal)

			done := make(chan error, 1)
			go func() { done <- r.Run(ctx, a) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("verbose runner hung waiting for marker resolution")
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("tool ran %d times, want %d; output:\n%s", got, tt.wantCalls, out.String())
			}
		})
	}
}

func TestRunnerModelMarkerKeys(t *testing.T) {
	tests := []struct {
		name         string
		keys         []string
		wantResolved bool
		wantApproved bool
	}{
		{"enter alone does not approve", []string{""}, false, false},
		{"uppercase N denies", []string{"N"}, true, false},
		{"typo asks again", []string{"nope"}, false, false},
		{"typo then yes approves", []string{"nope", "yes"}, true, true},
		{"Y approves", []string{"Y"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []agentsdk.Resolution
			deltas := make(chan types.Delta)
			stream := agentsdk.NewRemoteStream(deltas, nil, nil, func(id string, r agentsdk.Resolution) error {
				got = append(got, r)
				return nil
			})
			defer close(deltas)

			a, _ := markedAgent()
			m := newRunnerModel(a, context.Background(), TemplateMinimal)
			m.stream = stream
			m.deltaCh = stream.Deltas()
			model, _ := m.handleDelta(types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"})
			m = model.(runnerModel)

			for _, k := range tt.keys {
				for _, r := range k {
					model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
					m = model.(runnerModel)
				}
				model, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				m = model.(runnerModel)
			}

			if resolved := len(got) == 1; resolved != tt.wantResolved {
				t.Fatalf("resolutions = %d, want resolved=%v", len(got), tt.wantResolved)
			}
			if tt.wantResolved {
				if got[0].Approved != tt.wantApproved {
					t.Fatalf("approved = %v, want %v", got[0].Approved, tt.wantApproved)
				}
				if m.phase != phaseStreaming {
					t.Fatalf("phase = %v, want streaming after a decision", m.phase)
				}
			} else if m.phase != phaseMarker {
				t.Fatalf("phase = %v, want still waiting for approval", m.phase)
			}
		})
	}
}

// TestStreamDeltasResolvingFallbackSharedWriter checks that a renderer
// without inline marker support and an approval prompt can share one
// non-thread-safe writer. Run with -race: any overlap between the renderer's
// writes and the prompt's writes is reported as a data race.
func TestStreamDeltasResolvingFallbackSharedWriter(t *testing.T) {
	tests := []struct {
		name    string
		deltas  []types.Delta
		answers string
		want    []bool
	}{
		{
			name: "one marker approved",
			deltas: []types.Delta{
				types.PartDelta{Index: 0, Text: "before"},
				types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"},
				types.PartDelta{Index: 0, Text: "after"},
				types.DoneDelta{},
			},
			answers: "y\n",
			want:    []bool{true},
		},
		{
			name: "two markers",
			deltas: []types.Delta{
				types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"},
				types.PartDelta{Index: 0, Text: "between"},
				types.MarkerDelta{ToolCallID: "c2", ToolName: "danger"},
				types.DoneDelta{},
			},
			answers: "n\nyes\n",
			want:    []bool{false, true},
		},
		{
			name:    "no markers",
			deltas:  []types.Delta{types.PartDelta{Index: 0, Text: "plain"}, types.DoneDelta{}},
			answers: "",
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan types.Delta)
			go func() {
				defer close(ch)
				for _, d := range tt.deltas {
					ch <- d
				}
			}()

			var shared bytes.Buffer
			sc := bufio.NewScanner(strings.NewReader(tt.answers))
			var got []bool
			res := StreamDeltasResolving(plainOutput{NewJSONOutput(&shared, &shared)}, AgentHeader{}, ch, func(d types.MarkerDelta) {
				got = append(got, PromptApproval(sc, &shared, d))
			})
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("resolved %d markers, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("decision %d = %v, want %v", i, got[i], tt.want[i])
				}
			}
			if n := strings.Count(shared.String(), approvalPrompt); n != len(tt.want) {
				t.Fatalf("prompted %d times, want %d; output:\n%s", n, len(tt.want), shared.String())
			}
		})
	}
}

// stopEarlyOutput renders only the first delta and returns, leaving the rest
// of the stream unread.
type stopEarlyOutput struct{ *JSONOutput }

func (s stopEarlyOutput) StreamDeltas(_ AgentHeader, ch <-chan types.Delta) VerboseResult {
	<-ch
	return VerboseResult{}
}

func TestStreamDeltasResolvingFallbackRendererStopsEarly(t *testing.T) {
	ch := make(chan types.Delta, 4)
	ch <- types.PartDelta{Index: 0, Text: "hi"}
	ch <- types.PartDelta{Index: 0, Text: "more"}
	ch <- types.MarkerDelta{ToolCallID: "c1", ToolName: "danger"}
	ch <- types.MarkerDelta{ToolCallID: "c2", ToolName: "danger"}
	close(ch)

	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		StreamDeltasResolving(stopEarlyOutput{NewJSONOutput(&bytes.Buffer{}, &bytes.Buffer{})}, AgentHeader{}, ch, func(d types.MarkerDelta) {
			got = append(got, d.ToolCallID)
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StreamDeltasResolving hung after the renderer stopped early")
	}
	if strings.Join(got, ",") != "c1,c2" {
		t.Fatalf("resolved %v, want [c1 c2]", got)
	}
}
