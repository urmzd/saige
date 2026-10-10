// Package agenttest provides testing utilities for the agent SDK.
package agenttest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// ScriptedProvider replays predefined delta sequences, one per Stream call.
// Thread-safe for concurrent use.
//
// Part indices in a response are renumbered as it streams: each PartStart
// gets the next free index, and later deltas for the same index follow it.
// Sequences from TextResponse, ToolCallResponse and other helpers can
// therefore be concatenated even though each starts at index 0.
//
// It implements types.OptionsProvider, so per-request controls such as a tool
// choice reach it, and it records every request in Calls.
type ScriptedProvider struct {
	mu        sync.Mutex
	call      int
	Responses [][]types.Delta
	// Errors, when set, makes call i fail with Errors[i] before streaming.
	// A nil entry streams Responses[i] as usual.
	Errors []error
	// Calls records each request in order.
	Calls []ScriptedCall
}

// ScriptedCall is one request a ScriptedProvider received.
type ScriptedCall struct {
	Messages []types.Message
	Tools    []types.ToolDef
	Options  *types.RequestOptions // nil for a call without options
}

// Stream implements types.Provider.
func (p *ScriptedProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	var opts *types.RequestOptions
	if req.Options != nil {
		o := *req.Options
		opts = &o
	}
	return p.stream(ctx, req.Messages, req.Tools, opts)
}

// SupportsOptions implements types.OptionsProvider.
func (p *ScriptedProvider) SupportsOptions() bool { return true }

// CallCount returns how many requests the provider has received.
func (p *ScriptedProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call
}

// Requests returns a copy of the recorded requests.
func (p *ScriptedProvider) Requests() []ScriptedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ScriptedCall(nil), p.Calls...)
}

func (p *ScriptedProvider) stream(_ context.Context, messages []types.Message, tools []types.ToolDef, opts *types.RequestOptions) (<-chan types.Delta, error) {
	p.mu.Lock()
	idx := p.call
	p.call++
	p.Calls = append(p.Calls, ScriptedCall{
		Messages: append([]types.Message(nil), messages...),
		Tools:    append([]types.ToolDef(nil), tools...),
		Options:  opts,
	})
	var err error
	if idx < len(p.Errors) {
		err = p.Errors[idx]
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}

	ch := make(chan types.Delta, 64)
	go func() {
		defer close(ch)
		if idx < len(p.Responses) {
			re := Reindexer()
			for _, d := range p.Responses[idx] {
				ch <- re(d)
			}
		}
	}()
	return ch, nil
}

// Reindexer returns a function that renumbers part indices in a delta
// stream: each PartStart takes the next free index, and PartDelta and
// PartEnd follow the latest start of their original index. It lets
// sequences built separately, each from index 0, play as one stream.
func Reindexer() func(types.Delta) types.Delta {
	next := 0
	at := map[int]int{}
	return func(d types.Delta) types.Delta {
		switch v := d.(type) {
		case types.PartStart:
			at[v.Index] = next
			v.Index = next
			next++
			return v
		case types.PartDelta:
			if i, ok := at[v.Index]; ok {
				v.Index = i
			}
			return v
		case types.PartEnd:
			if i, ok := at[v.Index]; ok {
				v.Index = i
			}
			return v
		}
		return d
	}
}

// TextResponse creates a delta sequence for a simple text response.
func TextResponse(text string) []types.Delta {
	return types.PartDeltas(0, types.Text(text))
}

// ToolCallResponse creates a delta sequence for a tool call.
func ToolCallResponse(id, name string, args map[string]any) []types.Delta {
	return []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindToolCall, ID: id, Name: name},
		types.PartEnd{Index: 0, Part: types.ToolCallPart{ID: id, Name: name, Arguments: args}},
	}
}

// CollectDeltas drains a delta channel into a slice.
func CollectDeltas(ch <-chan types.Delta) []types.Delta {
	var deltas []types.Delta
	for d := range ch {
		deltas = append(deltas, d)
	}
	return deltas
}

// CollectText drains a delta channel and returns concatenated text content.
func CollectText(ch <-chan types.Delta) string {
	var sb strings.Builder
	for d := range ch {
		if pd, ok := d.(types.PartDelta); ok {
			sb.WriteString(pd.Text)
		}
	}
	return sb.String()
}

// CollectToolCalls drains a delta channel and returns all completed tool calls.
func CollectToolCalls(ch <-chan types.Delta) []types.ToolCallPart {
	asm := types.NewPartAssembler()
	for d := range ch {
		asm.Push(d)
	}
	var calls []types.ToolCallPart
	for _, p := range asm.Parts() {
		if tc, ok := p.(types.ToolCallPart); ok {
			calls = append(calls, tc)
		}
	}
	return calls
}

// AssertTextContains verifies the delta channel produces text containing substr.
func AssertTextContains(t *testing.T, ch <-chan types.Delta, substr string) {
	t.Helper()
	text := CollectText(ch)
	if !strings.Contains(text, substr) {
		t.Errorf("expected text to contain %q, got %q", substr, text)
	}
}

// AssertToolCalled verifies a specific tool was called in the deltas.
func AssertToolCalled(t *testing.T, deltas []types.Delta, name string) {
	t.Helper()
	for _, d := range deltas {
		if v, ok := d.(types.PartStart); ok && v.Kind == types.KindToolCall && v.Name == name {
			return
		}
	}
	t.Errorf("expected tool %q to be called, but it was not", name)
}

// AssertNoErrors verifies no error deltas were emitted.
func AssertNoErrors(t *testing.T, deltas []types.Delta) {
	t.Helper()
	for _, d := range deltas {
		if v, ok := d.(types.ErrorDelta); ok {
			t.Errorf("unexpected error delta: %v", v.Error)
		}
	}
}

// AssertDone verifies a DoneDelta was emitted.
func AssertDone(t *testing.T, deltas []types.Delta) {
	t.Helper()
	for _, d := range deltas {
		if _, ok := d.(types.DoneDelta); ok {
			return
		}
	}
	t.Error("expected DoneDelta but none was found")
}

// MockTool is a test tool with configurable behavior.
type MockTool struct {
	Def    types.ToolDef
	Result string
	Err    error
	Calls  []map[string]any // records all calls made
	mu     sync.Mutex
}

// Definition implements types.Tool.
func (t *MockTool) Definition() types.ToolDef { return t.Def }

// Execute implements types.Tool.
func (t *MockTool) Execute(_ context.Context, args map[string]any) (string, error) {
	t.mu.Lock()
	t.Calls = append(t.Calls, args)
	t.mu.Unlock()
	return t.Result, t.Err
}

// CallCount returns the number of times the tool was called.
func (t *MockTool) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.Calls)
}
