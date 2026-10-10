// Package legacyrun resumes the in-flight durable runs that the release
// before typed parts recorded (agent/internal/testdata/legacy/durable).
// Both engines' tests use it to prove that such a run replays after an
// upgrade: the recorded model turns, tool output and hook are read from the
// old journal, and only the work after the pending approval runs.
//
// The recorded run: the input is a text and a PNG with bytes; a user input
// hook appends "(tagged)"; turn 1 is reasoning, text, a web search server
// tool and a call to the rich tool snapshot (text, a PNG with bytes, JSON);
// turn 2 calls write, which needs approval, and the run suspended there.
package legacyrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// RunID is the recorded run's ID, and Interrupt its pending approval.
const (
	RunID     = "golden-run"
	Interrupt = "marker/call_write"
)

// PNG is the image bytes the recorded run carried.
var PNG = []byte("\x89PNG\r\n\x1a\nfixture-image")

// Input is the recorded run's input as this release builds it.
func Input() []types.Message {
	src := types.Bytes(types.MediaPNG, PNG)
	src.Filename = "in.png"
	return []types.Message{types.UserMsg(types.Text("Chart it."), types.Image(src))}
}

// Counters records what the resumed run did.
type Counters struct {
	ModelCalls, Snapshots, Writes atomic.Int32

	mu       sync.Mutex
	problems []string
}

func (c *Counters) fail(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// Problems lists what the model saw that differs from the recorded run.
func (c *Counters) Problems() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.problems...)
}

// provider answers the only turn left, turn 3, and checks that the request
// carries the recorded history, upgraded to parts with its bytes.
type provider struct{ c *Counters }

func (p provider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	p.c.ModelCalls.Add(1)
	p.check(req.Messages)
	out := make(chan types.Delta, 8)
	for _, d := range agenttest.TextResponse("done") {
		out <- d
	}
	close(out)
	return out, nil
}

//nolint:gocyclo // one check per recorded part
func (p provider) check(msgs []types.Message) {
	var turns []types.AssistantMessage
	var results []types.ToolResultPart
	inputImage := false
	tagged := false
	for _, m := range msgs {
		switch v := m.(type) {
		case types.AssistantMessage:
			turns = append(turns, v)
		case types.UserMessage:
			for _, part := range v.Parts {
				if img, ok := part.(types.ImagePart); ok && bytes.Equal(img.Source.Inline, PNG) {
					inputImage = true
				}
				if t, ok := part.(types.TextPart); ok && t.Text == "(tagged)" {
					tagged = true
				}
			}
		}
		for _, r := range types.Each[types.ToolResultPart](m) {
			results = append(results, r)
		}
	}
	if len(turns) != 2 {
		p.c.fail("request has %d assistant turns, want the 2 recorded ones", len(turns))
		return
	}
	if !inputImage {
		p.c.fail("the input image lost its bytes")
	}
	if !tagged {
		p.c.fail("the recorded hook rewrite is missing")
	}
	var thinking, server, call bool
	for _, part := range turns[0].Parts {
		switch v := part.(type) {
		case types.ThinkingPart:
			thinking = v.Text == "plan" && v.Signature == "sig-1"
		case types.ServerToolCallPart:
			server = v.ID == "srv_1"
		case types.ToolCallPart:
			n, _ := v.Arguments["n"].(float64)
			call = v.ID == "call_snap" && n == 3
		}
	}
	if !thinking || !server || !call {
		p.c.fail("turn 1 = %#v", turns[0].Parts)
	}
	var snap *types.ToolResultPart
	for i := range results {
		if results[i].CallID == "call_snap" {
			snap = &results[i]
		}
	}
	if snap == nil {
		p.c.fail("no recorded snapshot result")
		return
	}
	media := false
	for _, o := range snap.Parts {
		if img, ok := o.(types.ImagePart); ok && bytes.Equal(img.Source.Inline, PNG) {
			media = true
		}
	}
	if !media || snap.Text() == "" {
		p.c.fail("snapshot result = %#v", snap.Parts)
	}
}

type snapshot struct{ c *Counters }

func (snapshot) Definition() types.ToolDef {
	return types.ToolDef{Name: "snapshot", Description: "image"}
}

func (s snapshot) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.ExecuteRich(ctx, args)
	return r.Text(), err
}

func (s snapshot) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	s.c.Snapshots.Add(1)
	return types.ToolResult{Parts: []types.ToolOutputPart{types.Text("chart attached"), types.Image(types.Bytes(types.MediaPNG, PNG)),
		types.JSONPart{JSON: json.RawMessage(`{"rows":2}`)}}}, nil
}

// Agent builds the recorded run's agent.
func Agent(c *Counters) *agent.Agent {
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) {
		c.Writes.Add(1)
		return "written", nil
	}}
	return agent.NewAgent(agent.AgentConfig{
		Provider:         provider{c},
		SystemPrompt:     "rules",
		Tools:            types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), snapshot{c}),
		MaxParallelTools: 1,
	}, agent.WithHooks(agent.Hooks{Name: "tag", UserInput: func(_ context.Context, e *agent.UserInputEvent) error {
		e.Message.Parts = append(e.Message.Parts, types.Text("(tagged)"))
		return nil
	}}))
}

// Approve is the decision that lets the run continue.
var Approve = types.ApprovalDecision{Approved: true}

// Check returns why a resumed run's outcome differs from the recording,
// or nil.
func Check(c *Counters, final *types.AssistantMessage) []string {
	out := c.Problems()
	if final == nil || types.TextOf(*final) != "done" {
		out = append(out, fmt.Sprintf("final message = %#v", final))
	}
	if n := c.ModelCalls.Load(); n != 1 {
		out = append(out, fmt.Sprintf("model calls = %d, want 1: the recorded turns replay", n))
	}
	if n := c.Snapshots.Load(); n != 0 {
		out = append(out, fmt.Sprintf("snapshot ran %d times, want 0: its output was recorded", n))
	}
	if n := c.Writes.Load(); n != 1 {
		out = append(out, fmt.Sprintf("write ran %d times, want 1", n))
	}
	return out
}
