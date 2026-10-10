package online

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	agenteval "github.com/urmzd/saige/agent/eval"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
)

// Source yields finished runs.
type Source interface {
	// Records returns the runs that finished in w, oldest first.
	Records(ctx context.Context, w Window) ([]Record, error)
	// Lookup returns the run ref names. It returns an error wrapping
	// ErrNotFinished when the node does not end a run.
	Lookup(ctx context.Context, ref Ref) (Record, error)
}

// ErrNotFinished reports a node that is not the end of a finished run: it
// is missing, archived, not an assistant turn, or a turn that still calls
// tools.
var ErrNotFinished = errors.New("online: node does not end a finished run")

// FromPath builds the record of the run that path ends. path runs from the
// root of the conversation to the node that ends the run, which must be an
// active assistant node with no tool calls. The run starts after the last
// user message on the path that carries text; that text is the input.
// Tool calls, their results, the last route, and the first error (a tool
// error or a truncated turn) are read from the nodes in between.
func FromPath(conversation string, path []*types.Node) (Record, error) {
	if len(path) == 0 {
		return Record{}, fmt.Errorf("%w: empty path", ErrNotFinished)
	}
	end := path[len(path)-1]
	if !endsRun(end) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFinished, end.ID)
	}
	start := 0
	var input string
	for i := len(path) - 2; i >= 0; i-- {
		if text, ok := userText(path[i]); ok {
			start, input = i+1, text
			break
		}
	}
	rec := Record{
		Ref:        Ref{Conversation: conversation, Node: string(end.ID)},
		FinishedAt: end.CreatedAt,
		Input:      input,
	}
	byID := map[string]int{}
	for _, n := range path[start:] {
		if n.State == types.NodeArchived {
			continue
		}
		switch m := n.Message.(type) {
		case types.AssistantMessage:
			rec.Turns++
			for _, c := range m.Content {
				switch v := c.(type) {
				case types.ToolUseContent:
					byID[v.ID] = len(rec.ToolCalls)
					rec.ToolCalls = append(rec.ToolCalls, agenteval.ToolCallRecord{
						ID: v.ID, Name: v.Name, Arguments: v.Arguments, ArgumentsError: v.ArgumentsError,
						Exec: agenteval.ExecNotRun,
					})
				case types.RouteContent:
					rec.Model, rec.Preset = v.Model, v.Preset
				case types.TruncationContent:
					rec.setError("turn truncated: " + v.Reason)
				}
			}
		case types.SystemMessage:
			for _, c := range m.Content {
				if v, ok := c.(types.ToolResultContent); ok {
					rec.addResult(byID, v)
				}
			}
		case types.UserMessage:
			for _, c := range m.Content {
				if v, ok := c.(types.ToolResultContent); ok {
					rec.addResult(byID, v)
				}
			}
		}
	}
	rec.Output = assistantText(end.Message.(types.AssistantMessage))
	return rec, nil
}

func (r *Record) setError(msg string) {
	if r.Error == "" {
		r.Error = msg
	}
}

func (r *Record) addResult(byID map[string]int, res types.ToolResultContent) {
	idx, ok := byID[res.ToolCallID]
	if !ok {
		return
	}
	call := &r.ToolCalls[idx]
	call.Exec = agenteval.ExecFinished
	call.Version = res.ToolVersion
	if res.IsError {
		call.Error = res.Text
		r.setError(fmt.Sprintf("tool %s: %s", call.Name, res.Text))
	} else {
		call.Result = res.Text
	}
}

// endsRun reports whether n is an active assistant node without tool calls.
func endsRun(n *types.Node) bool {
	if n == nil || n.State != types.NodeActive {
		return false
	}
	m, ok := n.Message.(types.AssistantMessage)
	if !ok {
		return false
	}
	for _, c := range m.Content {
		if _, isCall := c.(types.ToolUseContent); isCall {
			return false
		}
	}
	return true
}

// userText returns the text of a user message node, ignoring nodes that
// only carry tool results, feedback, or other metadata.
func userText(n *types.Node) (string, bool) {
	if n.State == types.NodeArchived {
		return "", false
	}
	m, ok := n.Message.(types.UserMessage)
	if !ok {
		return "", false
	}
	var parts []string
	for _, c := range m.Content {
		if t, ok := c.(types.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func assistantText(m types.AssistantMessage) string {
	var b strings.Builder
	for _, c := range m.Content {
		if t, ok := c.(types.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// TreeSource reads finished runs from conversation trees in memory, keyed
// by conversation ID. Every branch is read, so a run on a branch that is no
// longer active is still found.
type TreeSource map[string]*tree.Tree

// Records implements [Source].
func (s TreeSource) Records(_ context.Context, w Window) ([]Record, error) {
	var out []Record
	for _, conv := range sortedKeys(s) {
		t := s[conv]
		root := t.Root()
		if root == nil {
			continue
		}
		var walk func(path []*types.Node) error
		walk = func(path []*types.Node) error {
			n := path[len(path)-1]
			if endsRun(n) && w.Contains(n.CreatedAt) {
				rec, err := FromPath(conv, path)
				if err != nil {
					return err
				}
				out = append(out, rec)
			}
			children, err := t.Children(n.ID)
			if err != nil {
				return err
			}
			for _, c := range children {
				if err := walk(append(slices.Clip(path), c)); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk([]*types.Node{root}); err != nil {
			return nil, fmt.Errorf("online: conversation %s: %w", conv, err)
		}
	}
	sortRecords(out)
	return out, nil
}

// Lookup implements [Source].
func (s TreeSource) Lookup(_ context.Context, ref Ref) (Record, error) {
	t, ok := s[ref.Conversation]
	if !ok {
		return Record{}, fmt.Errorf("%w: unknown conversation %q", ErrNotFinished, ref.Conversation)
	}
	ids, err := t.Path(types.NodeID(ref.Node))
	if err != nil {
		return Record{}, fmt.Errorf("%w: %w", ErrNotFinished, err)
	}
	// Rebuild the node path from the root by following children.
	path := []*types.Node{t.Root()}
	for _, id := range ids[1:] {
		children, err := t.Children(path[len(path)-1].ID)
		if err != nil {
			return Record{}, err
		}
		idx := slices.IndexFunc(children, func(n *types.Node) bool { return n.ID == id })
		if idx < 0 {
			return Record{}, fmt.Errorf("%w: node %s not under its parent", ErrNotFinished, id)
		}
		path = append(path, children[idx])
	}
	rec, err := FromPath(ref.Conversation, path)
	if err != nil {
		return Record{}, err
	}
	rec.Ref.TraceID, rec.Ref.SpanID = ref.TraceID, ref.SpanID
	return rec, nil
}

// RecordSource serves records built elsewhere, such as with [FromAgentRun]
// from a collected agent stream.
type RecordSource []Record

// Records implements [Source].
func (s RecordSource) Records(_ context.Context, w Window) ([]Record, error) {
	var out []Record
	for _, r := range s {
		if w.Contains(r.FinishedAt) {
			out = append(out, r)
		}
	}
	sortRecords(out)
	return out, nil
}

// Lookup implements [Source].
func (s RecordSource) Lookup(_ context.Context, ref Ref) (Record, error) {
	for _, r := range s {
		if r.Ref.Conversation == ref.Conversation && r.Ref.Node == ref.Node {
			return r, nil
		}
	}
	return Record{}, fmt.Errorf("%w: %s/%s", ErrNotFinished, ref.Conversation, ref.Node)
}

// FromAgentRun builds a record from a run collected with
// agent/eval.CollectAgentRun: the route of its last provider call gives the
// model and preset, its tool calls and usage carry over, and the first
// tool or stream error becomes the record's error.
func FromAgentRun(ref Ref, input string, finished time.Time, run agenteval.AgentRun) Record {
	rec := Record{
		Ref:        ref,
		FinishedAt: finished,
		Input:      input,
		Output:     run.Text,
		ToolCalls:  slices.Clone(run.ToolCalls),
		Turns:      run.TurnCount,
		Timing:     agentTiming(run),
	}
	if n := len(run.Routes); n > 0 {
		rec.Model, rec.Preset = run.Routes[n-1].Model, run.Routes[n-1].Preset
	} else if len(run.Models) > 0 {
		rec.Model = run.Models[len(run.Models)-1]
	}
	if len(run.Timing.Errors) > 0 {
		rec.setError(run.Timing.Errors[0])
	}
	for _, c := range run.ToolCalls {
		if c.Error != "" {
			rec.setError(fmt.Sprintf("tool %s: %s", c.Name, c.Error))
		}
	}
	return rec
}

// agentTiming is the observation timing agent/eval.AnnotateObservation
// records for run.
func agentTiming(run agenteval.AgentRun) eval.ObservationTiming {
	t := eval.ObservationTiming{
		TotalMs:      run.TotalMs,
		TTFTMs:       run.Timing.TTFTMs,
		TTLTMs:       run.Timing.TTLTMs,
		MedianITL:    run.Timing.MedianITL,
		InputTokens:  run.Timing.InputTokens,
		OutputTokens: run.Timing.OutputTokens,
	}
	if run.CostUSD != nil {
		t.SetCostUSD(*run.CostUSD)
	}
	return t
}

// sortRecords orders records by finish time, then conversation and node.
func sortRecords(recs []Record) {
	slices.SortStableFunc(recs, func(a, b Record) int {
		if c := a.FinishedAt.Compare(b.FinishedAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.Ref.Conversation, b.Ref.Conversation); c != 0 {
			return c
		}
		return strings.Compare(a.Ref.Node, b.Ref.Node)
	})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
