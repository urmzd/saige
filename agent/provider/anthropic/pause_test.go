package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/provider/internal/streamcheck"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// pauseServer replies to the nth request with the nth fixture (the last one
// once they run out) and records every request body.
func pauseServer(t *testing.T, fixtures ...string) (*httptest.Server, func() [][]byte) {
	t.Helper()
	bodies := make([][]byte, 0, len(fixtures))
	for _, f := range fixtures {
		b, err := os.ReadFile(filepath.Join("testdata", "streams", f+".sse"))
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	var mu sync.Mutex
	var seen [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(seen)
		seen = append(seen, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(bodies[min(n, len(bodies)-1)])
	}))
	t.Cleanup(server.Close)
	return server, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func drainTurn(t *testing.T, a *Adapter) ([]types.AssistantPart, []types.Delta, types.UsageDelta, error) {
	t.Helper()
	ch, err := a.Stream(context.Background(), types.Request{Messages: []types.Message{types.UserMsg(types.Text("when is the next go release?"))}})
	if err != nil {
		t.Fatal(err)
	}
	asm := types.NewPartAssembler()
	var deltas []types.Delta
	var usage types.UsageDelta
	var streamErr error
	for d := range ch {
		deltas = append(deltas, d)
		asm.Push(d)
		switch v := d.(type) {
		case types.UsageDelta:
			usage = usage.Merge(v)
		case types.ErrorDelta:
			streamErr = v.Error
		}
	}
	streamcheck.RunPartConformance(t, deltas)
	if n := asm.Violations(); n != 0 {
		t.Fatalf("assembler violations = %d", n)
	}
	return asm.Parts(), deltas, usage, streamErr
}

// TestPauseTurnContinues checks that a turn the API paused is continued:
// the partial response is sent back as the assistant turn, block for block,
// and the rest of the turn streams on with its parts numbered after the
// ones already sent. Usage covers both requests.
func TestPauseTurnContinues(t *testing.T) {
	server, requests := pauseServer(t, "pause_first", "pause_rest")
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL)))
	parts, deltas, usage, err := drainTurn(t, a)
	if err != nil {
		t.Fatalf("err = %v, want the continued turn to succeed", err)
	}
	wantKinds(t, parts, types.KindText, types.KindServerToolCall, types.KindServerToolResult, types.KindText, types.KindCitation,
		types.KindServerToolCall, types.KindServerToolResult, types.KindText)

	// Indices are the parts' positions, across both responses.
	var starts []int
	for _, d := range deltas {
		if s, ok := d.(types.PartStart); ok {
			starts = append(starts, s.Index)
		}
	}
	for i, idx := range starts {
		if idx != i {
			t.Fatalf("start indices = %v, want 0..%d in order", starts, len(parts)-1)
		}
	}
	if c := parts[4].(types.CitationPart); c.Anchor == nil || c.Anchor.PartIndex != 3 {
		t.Fatalf("citation anchor = %+v, want the text part at 3", c.Anchor)
	}
	// The continuation's result answers the call the first response made,
	// and keeps its kind.
	if r := parts[6].(types.ServerToolResultPart); r.CallID != "srvtoolu_2" || r.ToolKind != types.ServerToolWebSearch {
		t.Fatalf("continued result = %+v", r)
	}
	if got := parts[7].(types.TextPart).Text; got != "Go 1.27 is due in August." {
		t.Fatalf("final text = %q", got)
	}
	if usage.PromptTokens != 100+5 || usage.CompletionTokens != 35 || usage.CachedPromptTokens != 5 || usage.Requests != 2 {
		t.Fatalf("usage = %+v, want both requests added up", usage)
	}

	bodies := requests()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	var second struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 2 || second.Messages[1].Role != "assistant" {
		t.Fatalf("continuation messages = %s", bodies[1])
	}
	var blockTypes []string
	for _, raw := range second.Messages[1].Content {
		var b struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Input     json.RawMessage `json:"input"`
			Citations json.RawMessage `json:"citations"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		blockTypes = append(blockTypes, b.Type)
		if b.Type == "server_tool_use" && b.ID == "srvtoolu_2" && string(b.Input) != `{"query":"go 1.27 date"}` {
			t.Fatalf("pending call replayed with input %s", b.Input)
		}
	}
	want := []string{"text", "server_tool_use", "web_search_tool_result", "text", "server_tool_use"}
	if len(blockTypes) != len(want) {
		t.Fatalf("replayed blocks = %v, want %v", blockTypes, want)
	}
	for i := range want {
		if blockTypes[i] != want[i] {
			t.Fatalf("replayed blocks = %v, want %v", blockTypes, want)
		}
	}
}

// TestPauseTurnGivesUp checks that a turn that stays paused ends with the
// permanent error after the continuations are spent, rather than looping.
func TestPauseTurnGivesUp(t *testing.T) {
	server, requests := pauseServer(t, "pause_first")
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL)))
	_, _, usage, err := drainTurn(t, a)
	if !errors.Is(err, errPausedTurn) || types.KindOf(err) != types.ErrorKindPermanent {
		t.Fatalf("err = %v, want the paused-turn error", err)
	}
	if n := len(requests()); n != maxPauseContinuations+1 {
		t.Fatalf("requests = %d, want %d", n, maxPauseContinuations+1)
	}
	if usage.Requests != maxPauseContinuations+1 {
		t.Fatalf("usage requests = %d", usage.Requests)
	}
}

// TestCitationsSurviveALaterFailure checks that a text block's citations
// are sent when the block stops, so a stream that dies afterwards has
// already delivered them.
func TestCitationsSurviveALaterFailure(t *testing.T) {
	cite := `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","cited_text":"x","url":"https://example.com/a","title":"A","encrypted_index":"e"}}}`
	second := `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`
	events := []sseEvent{{"message_start", evStart}, {"content_block_start", evTextStart},
		{"content_block_delta", cite}, {"content_block_delta", evText("Cited.")}, {"content_block_stop", evBlockStop},
		{"content_block_start", second}}
	a := must.Get(New(Config{APIKey: "test", Model: types.ModelID(testModel)}, WithBaseURL(sseServer(t, true, events...).URL)))
	r := run(t, a, nil)
	if len(r.errs) != 1 {
		t.Fatalf("errors = %v, want the dropped stream", r.errs)
	}
	var got *types.CitationPart
	var secondIndex = -1
	for _, d := range r.deltas {
		switch v := d.(type) {
		case types.PartEnd:
			if c, ok := v.Part.(types.CitationPart); ok {
				got = &c
				if v.Index != 1 {
					t.Fatalf("citation index = %d, want 1, right after its text", v.Index)
				}
			}
		case types.PartStart:
			if v.Kind == types.KindText && v.Index != 0 {
				secondIndex = v.Index
			}
		}
	}
	if got == nil || got.Citation.URI != "https://example.com/a" || got.Anchor == nil || got.Anchor.PartIndex != 0 {
		t.Fatalf("citation = %+v, want it delivered before the failure", got)
	}
	if secondIndex != 2 {
		t.Fatalf("next block index = %d, want 2", secondIndex)
	}
}
