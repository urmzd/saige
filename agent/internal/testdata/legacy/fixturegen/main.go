// Command fixturegen wrote the stored-data goldens in the directory above:
// a tree document, a file WAL, Postgres node rows and durable journals, in
// the formats releases before typed parts wrote. It compiles only against
// that code (v0.32.0), not this tree, which is why it lives in testdata.
// To regenerate, check out v0.32.0, copy this file to
// agent/internal/fixturegen/main.go and run, against a migrated database:
//
//	go run ./agent/internal/fixturegen -out <this directory> -postgres <url>
package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/urmzd/duraturo/pkg/ledger"
	"github.com/urmzd/duraturo/pkg/queue"
	"github.com/urmzd/duraturo/pkg/run"
	"github.com/urmzd/duraturo/pkg/worker"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/durable/duraturo"
	"github.com/urmzd/saige/agent/durable/local"
	_ "github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/pgstore"
	"github.com/urmzd/saige/agent/store/filewal"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/postgres"
)

var png = []byte("\x89PNG\r\n\x1a\nfixture-image")

func ptr[T any](v T) *T { return &v }

func main() {
	out := flag.String("out", "", "output directory")
	pg := flag.String("postgres", "", "Postgres URL for node rows")
	flag.Parse()
	if *out == "" {
		log.Fatal("-out required")
	}
	ctx := context.Background()
	must(writeTree(ctx, *out))
	must(writeWAL(ctx, *out))
	if *pg != "" {
		must(writeRows(ctx, *out, *pg))
	}
	must(writeGob(*out))
	must(writeLocal(ctx, *out))
	must(writeDuraturo(ctx, *out))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// ── Messages with every stored content kind ─────────────────────────

func systemMsg() types.SystemMessage {
	return types.SystemMessage{Content: []types.SystemContent{
		types.TextContent{Text: "You are terse."},
		types.ConfigContent{Model: "gpt-x", MaxIter: 3, Compact: &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 10, KeepLast: 2},
			ToolChoice: &types.ToolChoice{Mode: types.ToolChoiceAuto}, Dials: &types.Dials{Creativity: ptr(types.CreativityFocused), MaxOutput: ptr(int64(512))},
			Reason: "fixture"},
		types.HandoffContent{To: "b", From: "a", Reason: "r", Message: "m", Context: "c"},
		types.RouteContent{Profile: "fast", Provider: "openai", Model: "gpt-x", Reason: "primary", Preset: "p", ConfigHash: "h",
			CatalogRevision: "rev", Options: &types.RequestOptions{Temperature: ptr(0.2), MaxOutputTokens: ptr(int64(100))}},
		types.ApprovalContent{Event: types.ApprovalEventGranted, Tool: "write", ToolCallID: "call_w", Grant: &types.Grant{ID: "g1", Scope: types.GrantScope("session")}, Approver: "ops", Reason: "ok"},
		types.GuardrailContent{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionBlock, Reason: "ssn"},
		types.CompactionContent{Strategy: "summary", Steps: []string{"summary"}, Trigger: types.CompactionTriggerRule, TokensBefore: 100, TokensAfter: 10,
			FromBranch: "main", Kept: []types.NodeID{"k"}, SummaryNode: "s"},
	}}
}

func toolResults() types.SystemMessage {
	return types.SystemMessage{Content: []types.SystemContent{
		types.ToolResultContent{ToolCallID: "call_plain", Text: "plain result"},
		types.ToolResultContent{ToolCallID: "call_err", Text: "boom", IsError: true},
		types.ToolResultContent{ToolCallID: "call_rich", Text: "chart attached", Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockText, Text: "chart attached"},
			{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, Data: png},
			{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, URI: "https://example.com/chart.png", Filename: "chart.png"},
			{Kind: types.ToolResultBlockFile, MediaType: types.MediaPDF, URI: "s3://bucket/report.pdf", Filename: "report.pdf"},
			{Kind: types.ToolResultBlockJSON, JSON: json.RawMessage(`{"rows":2}`)},
		}},
		types.ToolResultContent{ToolCallID: "call_media_only", Text: "see image", Blocks: []types.ToolResultBlock{
			{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, Data: png},
		}},
		types.ToolResultContent{ToolCallID: "call_cited", Text: "sourced", Citations: []types.Citation{types.NewCitation(types.CitationWeb, "https://example.com", "Example")},
			ToolVersion: "2"},
	}}
}

func userMsg() types.UserMessage {
	return types.UserMessage{Content: []types.UserContent{
		types.TextContent{Text: "Describe these."},
		types.FileContent{URI: "https://example.com/a.png", MediaType: types.MediaPNG, Filename: "a.png"},
		types.FileContent{MediaType: types.MediaPDF, Data: []byte("%PDF-1.7 fixture"), Filename: "inline.pdf"},
		types.FileContent{URI: "gs://bucket/clip.mp4", MediaType: types.MediaMP4},
		types.SteerContent{ID: "sub-1"},
		types.GuardrailContent{Guardrail: "pii", Phase: types.GuardrailPhaseInput, Action: types.GuardrailActionRewrite},
		types.ConfigContent{MaxIter: 2},
		types.HandoffContent{To: "a"},
	}}
}

func userToolResults() types.UserMessage {
	return types.UserMessage{Content: []types.UserContent{
		types.ToolResultContent{ToolCallID: "call_human", Text: "human answer"},
	}}
}

func assistantMsg() types.AssistantMessage {
	return types.AssistantMessage{Content: []types.AssistantContent{
		types.ThinkingContent{Thinking: "plan", Signature: "sig-1"},
		types.TextContent{Text: "Looking."},
		types.ServerToolContent{ID: "srv_1", Kind: types.ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go", "max": 3},
			Text: "results", Result: json.RawMessage(`{"hits":[{"url":"https://go.dev"}]}`),
			Files: []types.FileContent{{URI: "https://example.com/chart.png", MediaType: types.MediaPNG}}},
		types.ServerToolContent{ID: "srv_2", Kind: types.ServerToolCodeExecution, Name: "code_execution", Input: map[string]any{"code": "1+1"}},
		types.ToolUseContent{ID: "call_rich", Name: "snapshot", Arguments: map[string]any{"n": 3, "big": 12345678901234, "f": 1.5, "list": []any{1, map[string]any{"k": "v"}}}},
		types.ToolUseContent{ID: "call_bad", Name: "snapshot", ArgumentsError: "unexpected end of JSON input"},
		types.RouteContent{Provider: "anthropic", Model: "claude-x"},
		types.GuardrailContent{Guardrail: "tone", Phase: types.GuardrailPhaseOutput, Action: types.GuardrailActionPass},
		types.TruncationContent{Reason: "max_tokens"},
	}}
}

// buildTree adds every message kind to a tree with a side branch, a
// checkpoint, feedback and an archived node.
func buildTree(ctx context.Context, opts ...tree.Option) (*tree.Tree, error) {
	tr, err := tree.New(systemMsg(), opts...)
	if err != nil {
		return nil, err
	}
	root := tr.Root()
	u, err := tr.AddChild(ctx, root.ID, userMsg())
	if err != nil {
		return nil, err
	}
	a, err := tr.AddChild(ctx, u.ID, assistantMsg())
	if err != nil {
		return nil, err
	}
	tres, err := tr.AddChild(ctx, a.ID, toolResults())
	if err != nil {
		return nil, err
	}
	if _, err := tr.AddChild(ctx, tres.ID, userToolResults()); err != nil {
		return nil, err
	}
	if _, err := tr.AddFeedback(ctx, a.ID, types.UserMessage{Content: []types.UserContent{
		types.FeedbackContent{TargetNodeID: string(a.ID), Rating: types.RatingPositive, Comment: "good"},
	}}); err != nil {
		return nil, err
	}
	if _, err := tr.CheckpointContext(ctx, "main", "after-tools"); err != nil {
		return nil, err
	}
	_, side, err := tr.Branch(ctx, u.ID, "alt", types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "alternative"}}})
	if err != nil {
		return nil, err
	}
	if err := tr.ArchiveContext(ctx, side.ID, "fixture", false); err != nil {
		return nil, err
	}
	return tr, nil
}

func writeTree(ctx context.Context, out string) error {
	tr, err := buildTree(ctx)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(out, "tree", "v1_tree.json"), raw); err != nil {
		return err
	}
	// Each message alone, as pgstore and filewal store it.
	msgs := []struct {
		Name    string          `json:"name"`
		Role    string          `json:"role"`
		Message json.RawMessage `json:"message"`
	}{}
	for _, m := range []struct {
		name string
		msg  types.Message
	}{{"system", systemMsg()}, {"tool_results", toolResults()}, {"user", userMsg()}, {"user_tool_results", userToolResults()}, {"assistant", assistantMsg()}} {
		b, err := tree.MarshalMessage(m.msg)
		if err != nil {
			return err
		}
		msgs = append(msgs, struct {
			Name    string          `json:"name"`
			Role    string          `json:"role"`
			Message json.RawMessage `json:"message"`
		}{m.name, string(m.msg.Role()), b})
	}
	raw, err = json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(out, "tree", "v1_messages.json"), raw)
}

func writeWAL(ctx context.Context, out string) error {
	dir := filepath.Join(out, "filewal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "v1.wal")
	_ = os.Remove(path)
	w, err := filewal.New(path)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	_, err = buildTree(ctx, tree.WithWAL(w))
	return err
}

func writeRows(ctx context.Context, out, url string) error {
	pool, err := postgres.NewPool(ctx, postgres.Config{URL: url})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.RunMigrations(ctx, pool, postgres.MigrationOptions{}); err != nil {
		return err
	}
	const conv = "conv-golden-v1"
	for _, q := range []string{
		`DELETE FROM agent_node WHERE conversation_id = $1`,
		`DELETE FROM agent_branch WHERE conversation_id = $1`,
		`DELETE FROM agent_checkpoint WHERE conversation_id = $1`,
		`DELETE FROM agent_conversation WHERE conversation_id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, conv); err != nil {
			return err
		}
	}
	tr, err := buildTree(ctx, tree.WithStore(pgstore.NewStore(pool, conv, nil)))
	if err != nil {
		return err
	}
	dump := map[string]json.RawMessage{"root_id": json.RawMessage(fmt.Sprintf("%q", tr.Root().ID))}
	for _, table := range []string{"agent_node", "agent_branch", "agent_checkpoint", "agent_conversation"} {
		var rows json.RawMessage
		q := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(t) - 'id' ORDER BY t::text), '[]'::jsonb) FROM %s t WHERE conversation_id = $1`, table)
		if err := pool.QueryRow(ctx, q, conv).Scan(&rows); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		dump[table] = rows
	}
	raw, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(out, "pgstore", "v1_rows.json"), raw)
}

// ── Durable records ─────────────────────────────────────────────────

// recordedResult has the fields of the duraturo batch record.
type recordedResult struct {
	CustomID     string
	Outcome      types.BatchOutcome
	Message      types.AssistantMessage
	FinishReason string
	Usage        types.UsageDelta
	Err          string
}

func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}

func writeGob(out string) error {
	blobs := map[string]any{
		"input_messages.gob": []types.Message{systemMsg(), userMsg(), assistantMsg(), toolResults(), userToolResults()},
		"final_message.gob":  assistantMsg(),
		"step_llm.gob": types.StepResult{Kind: types.StepKindLLM, Message: ptr(assistantMsg()),
			Usage:   &types.UsageDelta{AccountingID: "acct", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
			Receipt: &types.BudgetReceipt{ID: "r1", Model: "gpt-x", Usage: types.TokenUsage{InputTokens: 10, OutputTokens: 5}}},
		"step_tool.gob": types.StepResult{Kind: types.StepKindTool, ToolCallID: "call_rich", ToolResult: "chart attached",
			ToolBlocks: toolResults().Content[2].(types.ToolResultContent).Blocks},
		"step_tool_error.gob": types.StepResult{Kind: types.StepKindTool, ToolCallID: "call_err", ToolError: "boom"},
		"step_approval.gob":   types.StepResult{Kind: types.StepKindApproval, Approval: &types.ApprovalVerdict{Outcome: types.VerdictAsk, Reason: "needs review"}},
		"step_hook.gob": types.StepResult{Kind: types.StepKindHook, Hook: &types.HookRecord{Name: "redact", Changed: true, Message: ptr(userMsg()),
			Arguments: map[string]any{"q": "x", "n": 2}, Text: "t", Action: types.GuardrailActionRewrite}},
		"batch_results.gob": []recordedResult{{CustomID: "b1", Outcome: types.BatchOutcome("succeeded"), Message: assistantMsg(), FinishReason: "stop",
			Usage: types.UsageDelta{PromptTokens: 1}}, {CustomID: "b2", Outcome: types.BatchOutcome("errored"), Err: "bad"}},
	}
	for name, v := range blobs {
		raw, err := encode(v)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := writeFile(filepath.Join(out, "durable", "gob", name), raw); err != nil {
			return err
		}
	}
	return nil
}

// provider scripts three turns: reasoning, a server tool and a rich tool
// call; then a call that needs approval; then the answer.
type provider struct{ calls *atomic.Int32 }

func (p provider) ChatStream(_ context.Context, m []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.calls.Add(1)
	turns := 0
	for _, msg := range m {
		if _, ok := msg.(types.AssistantMessage); ok {
			turns++
		}
	}
	var ds []types.Delta
	switch turns {
	case 0:
		ds = []types.Delta{
			types.ThinkingStartDelta{}, types.ThinkingContentDelta{Content: "plan"}, types.ThinkingEndDelta{Signature: "sig-1"},
			types.TextStartDelta{}, types.TextContentDelta{Content: "Looking."}, types.TextEndDelta{},
			types.ServerToolCallDelta{ID: "srv_1", Kind: types.ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go"}},
			types.ServerToolResultDelta{ID: "srv_1", Kind: types.ServerToolWebSearch, Text: "results", Result: json.RawMessage(`{"hits":1}`),
				Files: []types.FileContent{{URI: "https://example.com/chart.png", MediaType: types.MediaPNG}}},
			types.ToolCallStartDelta{ID: "call_snap", Name: "snapshot"},
			types.ToolCallEndDelta{ID: "call_snap", Arguments: map[string]any{"n": 3.0, "list": []any{1.0, map[string]any{"k": "v"}}}},
		}
	case 1:
		ds = []types.Delta{
			types.ToolCallStartDelta{ID: "call_write", Name: "write"},
			types.ToolCallEndDelta{ID: "call_write", Arguments: map[string]any{"value": "original"}},
		}
	default:
		ds = []types.Delta{types.TextStartDelta{}, types.TextContentDelta{Content: "done"}, types.TextEndDelta{}}
	}
	out := make(chan types.Delta, len(ds))
	for _, d := range ds {
		out <- d
	}
	close(out)
	return out, nil
}

type snapshot struct{}

func (snapshot) Definition() types.ToolDef {
	return types.ToolDef{Name: "snapshot", Description: "image"}
}
func (snapshot) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := snapshot{}.ExecuteRich(ctx, args)
	return r.Text, err
}
func (snapshot) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ToolResult{Text: "chart attached", Blocks: []types.ToolResultBlock{
		{Kind: types.ToolResultBlockText, Text: "chart attached"},
		{Kind: types.ToolResultBlockImage, MediaType: types.MediaPNG, Data: png},
		{Kind: types.ToolResultBlockJSON, JSON: json.RawMessage(`{"rows":2}`)},
	}}, nil
}

// runInput carries a file with bytes, so the journal holds them.
func runInput() []types.Message {
	return []types.Message{types.UserMessage{Content: []types.UserContent{
		types.TextContent{Text: "Chart it."},
		types.FileContent{MediaType: types.MediaPNG, Data: png, Filename: "in.png"},
	}}}
}

func newAgent(calls *atomic.Int32) *agent.Agent {
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { return "written", nil }}
	return agent.NewAgent(agent.AgentConfig{
		Provider:         provider{calls},
		SystemPrompt:     "rules",
		Tools:            types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), snapshot{}),
		MaxParallelTools: 1,
	}, agent.WithHooks(agent.Hooks{Name: "tag", UserInput: func(_ context.Context, e *agent.UserInputEvent) error {
		e.Message.Content = append(e.Message.Content, types.TextContent{Text: "(tagged)"})
		return nil
	}}))
}

func writeLocal(ctx context.Context, out string) error {
	dir := filepath.Join(out, "durable", "local")
	_ = os.RemoveAll(dir)
	var calls atomic.Int32
	engine := local.New(dir)
	_, err := engine.Run(ctx, "golden-run", "v1", func() *agent.Agent { return newAgent(&calls) }, runInput())
	if !errors.Is(err, types.ErrSuspended) {
		return fmt.Errorf("local run: want suspended, got %v", err)
	}
	// The lock file is the worker lease, not data.
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "worker.lock" {
			return os.Remove(path)
		}
		return err
	})
}

func writeDuraturo(ctx context.Context, out string) error {
	lgr := ledger.NewMemory()
	e := duraturo.New(lgr, queue.NewMemory())
	var calls atomic.Int32
	wf := e.Register("", func(string) *agent.Agent { return newAgent(&calls) })
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := e.Worker(worker.WithLeaseTTL(300*time.Millisecond), worker.WithJanitorEvery(20*time.Millisecond),
		worker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	go func() { _ = w.Run(wctx) }()
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	defer rcancel()
	if _, err := e.Run(rctx, wf, "golden-run", runInput()); !errors.Is(err, types.ErrSuspended) {
		return fmt.Errorf("duraturo run: want suspended, got %v", err)
	}
	// Let the worker finish parking the run.
	time.Sleep(200 * time.Millisecond)
	cancel()
	r, recs, err := lgr.Load(ctx, "golden-run")
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(struct {
		Run     run.Run      `json:"run"`
		Records []run.Record `json:"records"`
	}{r, recs}, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(out, "durable", "duraturo", "v1_ledger.json"), raw)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
