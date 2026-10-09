package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/selector/rank"
	"github.com/urmzd/saige/agent/types"
)

// Names of the tools that manage sub-agent handles. They are registered once
// on an agent with at least one SubAgentSpawn definition.
const (
	AwaitSubAgentTool  = "await_subagent"
	SendSubAgentTool   = "send_subagent"
	CancelSubAgentTool = "cancel_subagent"
	ListSubAgentsTool  = "list_subagents"
	SearchSubAgentTool = "search_subagent"
	ReadSubAgentTool   = "read_subagent"
)

// Argument names shared by the built-in delegation, handoff and clarify tools.
const (
	argTask     = "task"
	argMessage  = "message"
	argHandle   = "handle"
	argQuestion = "question"
)

// budgetToolName names the synthetic tool call that carries a budget approval.
const budgetToolName = "budget"

// Bounds on what the transcript tools return.
const (
	defaultTranscriptHits   = 5
	maxTranscriptHits       = 20
	defaultTranscriptWindow = 5
	maxTranscriptWindow     = 20
	transcriptSnippetChars  = 300
	transcriptMessageChars  = 4000
)

// isSubAgentControlTool reports whether name is one of the handle tools.
func isSubAgentControlTool(name string) bool {
	switch name {
	case AwaitSubAgentTool, SendSubAgentTool, CancelSubAgentTool, ListSubAgentsTool, SearchSubAgentTool, ReadSubAgentTool:
		return true
	}
	return false
}

// registerSubAgentControls adds the handle tools to registry unless they are
// already there.
func registerSubAgentControls(registry *types.ToolRegistry) {
	if _, ok := registry.Get(AwaitSubAgentTool); ok {
		return
	}
	handle := types.PropertyDef{Type: types.SchemaString, Description: "The handle returned by spawn_<name>, or the ID of a finished delegate_to_<name> call."}
	object := func(required []string, props map[string]types.PropertyDef) types.ParameterSchema {
		return types.ParameterSchema{Type: types.SchemaObject, Required: required, Properties: props}
	}
	for _, t := range []*handleTool{
		{
			def: types.ToolDef{
				Name:        AwaitSubAgentTool,
				Description: "Wait for a spawned sub-agent to finish and return its result. Its result is then not delivered again as a message.",
				Parameters:  object([]string{argHandle}, map[string]types.PropertyDef{argHandle: handle}),
				Capability:  types.ToolCapabilityRead,
			},
			run: awaitSubAgent,
		},
		{
			def: types.ToolDef{
				Name:        SendSubAgentTool,
				Description: "Send a message to a running sub-agent. It reads the message at its next step.",
				Parameters: object([]string{argHandle, argMessage}, map[string]types.PropertyDef{
					argHandle:  handle,
					argMessage: {Type: types.SchemaString, Description: "Instructions or information for the sub-agent."},
				}),
			},
			run: sendSubAgent,
		},
		{
			def: types.ToolDef{
				Name:        CancelSubAgentTool,
				Description: "Stop a running sub-agent. Its result is not delivered.",
				Parameters:  object([]string{argHandle}, map[string]types.PropertyDef{argHandle: handle}),
			},
			run: cancelSubAgent,
		},
		{
			def: types.ToolDef{
				Name:        ListSubAgentsTool,
				Description: "List the sub-agents of this run with their handles and status.",
				Parameters:  object(nil, map[string]types.PropertyDef{}),
				Capability:  types.ToolCapabilityRead,
			},
			run: listSubAgents,
		},
		{
			def: types.ToolDef{
				Name:        SearchSubAgentTool,
				Description: "Search a finished sub-agent's transcript. Returns the best matching messages by index with a short excerpt.",
				Parameters: object([]string{argHandle, "query"}, map[string]types.PropertyDef{
					argHandle: handle,
					"query":   {Type: types.SchemaString, Description: "Words to look for."},
					"k":       {Type: types.SchemaInteger, Description: fmt.Sprintf("Number of matches, default %d, at most %d.", defaultTranscriptHits, maxTranscriptHits)},
				}),
				Capability: types.ToolCapabilityRead,
			},
			run: searchSubAgent,
		},
		{
			def: types.ToolDef{
				Name:        ReadSubAgentTool,
				Description: "Read messages from a finished sub-agent's transcript, starting at an index.",
				Parameters: object([]string{argHandle}, map[string]types.PropertyDef{
					argHandle: handle,
					"index":   {Type: types.SchemaInteger, Description: "First message index, default 0."},
					"window":  {Type: types.SchemaInteger, Description: fmt.Sprintf("Number of messages, default %d, at most %d.", defaultTranscriptWindow, maxTranscriptWindow)},
				}),
				Capability: types.ToolCapabilityRead,
			},
			run: readSubAgent,
		},
	} {
		registry.Register(t)
	}
}

// handleTool is one of the handle tools. It reads the run's registry from
// the call's context, which the agent loop provides.
type handleTool struct {
	def types.ToolDef
	run func(ctx context.Context, r *spawnRegistry, args map[string]any) (string, error)
}

func (t *handleTool) Definition() types.ToolDef { return t.def }

func (t *handleTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r := spawnsFrom(ctx)
	if r == nil {
		return "", ErrSpawnUnsupported
	}
	return t.run(ctx, r, args)
}

func handleArg(r *spawnRegistry, args map[string]any) (*SubAgentHandle, error) {
	return r.get(stringArg(args, argHandle))
}

func awaitSubAgent(ctx context.Context, r *spawnRegistry, args map[string]any) (string, error) {
	h, err := handleArg(r, args)
	if err != nil {
		return "", err
	}
	result, err := h.Wait(ctx)
	if ctx.Err() != nil {
		return "", fmt.Errorf("sub-agent %s is still running: %w", h.id, ctx.Err())
	}
	markDelivered(h)
	if err != nil {
		return "", fmt.Errorf("sub-agent %s failed: %w", h.id, err)
	}
	return result.Output, nil
}

func sendSubAgent(_ context.Context, r *spawnRegistry, args map[string]any) (string, error) {
	h, err := handleArg(r, args)
	if err != nil {
		return "", err
	}
	message := stringArg(args, argMessage)
	if strings.TrimSpace(message) == "" {
		return "", errors.New("message is empty")
	}
	if _, err := h.Send(types.NewUserMessage(message)); err != nil {
		return "", fmt.Errorf("sub-agent %s: %w", h.id, err)
	}
	return "Message delivered to " + h.id + ".", nil
}

func cancelSubAgent(_ context.Context, r *spawnRegistry, args map[string]any) (string, error) {
	h, err := handleArg(r, args)
	if err != nil {
		return "", err
	}
	// The status check and the delivery mark share one critical section, so
	// a child that finishes in between keeps its result for injection.
	h.mu.Lock()
	status := h.status
	if status == HandleRunning {
		h.delivered = true
	}
	h.mu.Unlock()
	if status != HandleRunning {
		return "Sub-agent " + h.id + " had already finished: " + string(status) + ".", nil
	}
	h.Cancel()
	return "Sub-agent " + h.id + " cancelled.", nil
}

// handleSummary is one row of list_subagents.
type handleSummary struct {
	Handle string       `json:"handle"`
	Name   string       `json:"name"`
	Status HandleStatus `json:"status"`
	Task   string       `json:"task"`
}

func listSubAgents(_ context.Context, r *spawnRegistry, _ map[string]any) (string, error) {
	r.mu.Lock()
	handles := append([]*SubAgentHandle(nil), r.order...)
	r.mu.Unlock()
	rows := make([]handleSummary, 0, len(handles))
	for _, h := range handles {
		rows = append(rows, handleSummary{Handle: h.id, Name: h.name, Status: h.Status(), Task: truncateText(h.task, transcriptSnippetChars)})
	}
	raw, err := json.Marshal(rows)
	return string(raw), err
}

// transcriptEntry is one message of a child's transcript as the transcript
// tools see it.
type transcriptEntry struct {
	index int
	role  types.Role
	text  string
}

// transcript renders a finished child's messages for search and reading.
func transcript(h *SubAgentHandle) ([]transcriptEntry, error) {
	select {
	case <-h.done:
	default:
		return nil, fmt.Errorf("sub-agent %s is still running; await it first", h.id)
	}
	h.mu.Lock()
	result := cloneSubAgentResult(h.result)
	h.mu.Unlock()
	if len(result.Trace) == 0 {
		return nil, fmt.Errorf("sub-agent %s has no transcript", h.id)
	}
	messages, err := result.Messages()
	if err != nil {
		return nil, err
	}
	entries := make([]transcriptEntry, len(messages))
	for i, m := range messages {
		entries[i] = transcriptEntry{index: i, role: m.Role(), text: transcriptText(m)}
	}
	return entries, nil
}

// transcriptText renders a message's text, tool calls, and tool results.
func transcriptText(m types.Message) string {
	var parts []string
	add := func(c any) {
		switch v := c.(type) {
		case types.TextContent:
			parts = append(parts, v.Text)
		case types.ToolUseContent:
			args, _ := json.Marshal(v.Arguments)
			parts = append(parts, fmt.Sprintf("[call %s %s]", v.Name, args))
		case types.ToolResultContent:
			parts = append(parts, "[result] "+v.Text)
		}
	}
	switch v := m.(type) {
	case types.SystemMessage:
		for _, c := range v.Content {
			add(c)
		}
	case types.UserMessage:
		for _, c := range v.Content {
			add(c)
		}
	case types.AssistantMessage:
		for _, c := range v.Content {
			add(c)
		}
	}
	return strings.Join(parts, "\n")
}

func searchSubAgent(ctx context.Context, r *spawnRegistry, args map[string]any) (string, error) {
	h, err := handleArg(r, args)
	if err != nil {
		return "", err
	}
	entries, err := transcript(h)
	if err != nil {
		return "", err
	}
	k := boundedInt(args["k"], defaultTranscriptHits, maxTranscriptHits)
	if k == 0 {
		k = defaultTranscriptHits
	}
	hits, err := rank.NewBM25(func(e transcriptEntry) string { return e.text }).Select(ctx, stringArg(args, "query"), entries, k)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "No matching messages.", nil
	}
	var b strings.Builder
	for _, e := range hits {
		fmt.Fprintf(&b, "[%d] %s: %s\n", e.index, e.role, truncateText(e.text, transcriptSnippetChars))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func readSubAgent(_ context.Context, r *spawnRegistry, args map[string]any) (string, error) {
	h, err := handleArg(r, args)
	if err != nil {
		return "", err
	}
	entries, err := transcript(h)
	if err != nil {
		return "", err
	}
	start := boundedInt(args["index"], 0, len(entries))
	window := boundedInt(args["window"], defaultTranscriptWindow, maxTranscriptWindow)
	if window == 0 {
		window = defaultTranscriptWindow
	}
	if start >= len(entries) {
		return "", fmt.Errorf("index %d is past the last message (%d messages)", start, len(entries))
	}
	end := min(start+window, len(entries))
	var b strings.Builder
	for _, e := range entries[start:end] {
		fmt.Fprintf(&b, "[%d] %s:\n%s\n\n", e.index, e.role, truncateText(e.text, transcriptMessageChars))
	}
	fmt.Fprintf(&b, "(messages %d to %d of %d)", start, end-1, len(entries))
	return b.String(), nil
}

// boundedInt reads a non-negative integer argument, using def when it is
// absent and capping it at limit.
func boundedInt(v any, def, limit int) int {
	n := def
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			n = int(i)
		}
	}
	if n < 0 {
		n = def
	}
	return min(n, limit)
}

func truncateText(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "..."
}
