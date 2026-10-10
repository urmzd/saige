package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// batchServer stubs the Message Batches API. The batch reports in_progress
// on the first poll and ended after; results come back in reverse order.
type batchServer struct {
	mu      sync.Mutex
	created map[string]any
	polls   int
	cancels int
	ids     []string
}

func (s *batchServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/batches":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &s.created); err != nil {
				t.Errorf("create body: %v", err)
			}
			for _, q := range s.created["requests"].([]any) {
				s.ids = append(s.ids, q.(map[string]any)["custom_id"].(string))
			}
			_, _ = io.WriteString(w, batchJSON("in_progress", len(s.ids), 0))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_1":
			s.polls++
			if s.polls == 1 {
				_, _ = io.WriteString(w, batchJSON("in_progress", len(s.ids), 0))
				return
			}
			_, _ = io.WriteString(w, batchJSON("ended", 0, 1))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_1/results":
			w.Header().Set("Content-Type", "application/x-jsonl")
			// Reverse order: results arrive in any order.
			lines := []string{
				`{"custom_id":"` + s.ids[3] + `","result":{"type":"expired"}}`,
				`{"custom_id":"` + s.ids[2] + `","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens too large"}}}}`,
				`{"custom_id":"` + s.ids[1] + `","result":{"type":"succeeded","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"tool_use","id":"tu_1","name":"structured_output","input":{"label":"spam"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":8}}}}`,
				`{"custom_id":"` + s.ids[0] + `","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"Paris"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":5}}}}`,
			}
			_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/batches/msgbatch_1/cancel":
			s.cancels++
			_, _ = io.WriteString(w, batchJSON("canceling", 1, 0))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches/msgbatch_404":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"no such batch"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages/batches":
			_, _ = io.WriteString(w, `{"data":[`+batchJSON("ended", 0, 1)+`],"has_more":false,"first_id":"msgbatch_1","last_id":"msgbatch_1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func batchJSON(status string, processing, ended int) string {
	counts := `{"processing":` + itoa(processing) + `,"succeeded":` + itoa(2*ended) + `,"errored":` + itoa(ended) + `,"canceled":0,"expired":` + itoa(ended) + `}`
	endedAt := "null"
	if status == "ended" {
		endedAt = `"2026-10-09T12:30:00Z"`
	}
	return `{"id":"msgbatch_1","type":"message_batch","processing_status":"` + status + `","request_counts":` + counts +
		`,"created_at":"2026-10-09T12:00:00Z","expires_at":"2026-10-10T12:00:00Z","ended_at":` + endedAt +
		`,"cancel_initiated_at":null,"archived_at":null,"results_url":null}`
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestBatchSubmitPollResults runs a batch end to end against the stub: the
// create body carries Messages parameters per request, polling maps the
// status, and out-of-order results map back to the caller's IDs.
func TestBatchSubmitPollResults(t *testing.T) {
	stub := &batchServer{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL), WithMaxTokens(256)))

	schema := &types.ParameterSchema{Type: "object", Required: []string{"label"},
		Properties: map[string]types.PropertyDef{"label": {Type: "string"}}}
	reqs := []types.BatchRequest{
		{CustomID: "capital/fr", Messages: []types.Message{types.SystemMsg(types.Text("Be brief.")), types.UserMsg(types.Text("Capital of France?"))}},
		{CustomID: "classify#1", Messages: []types.Message{types.UserMsg(types.Text("Win a prize now"))}, Schema: schema},
		{CustomID: "too big", Messages: []types.Message{types.UserMsg(types.Text("x"))}},
		{CustomID: "late", Messages: []types.Message{types.UserMsg(types.Text("y"))}},
	}
	r := must.Get(batch.NewRunner(batch.RunnerConfig{Provider: a, Store: batch.NewMemoryStore()}, batch.WithPollInterval(time.Millisecond, time.Millisecond)))
	got, err := r.Run(context.Background(), "job-anthropic", reqs)
	if err != nil {
		t.Fatal(err)
	}

	first := stub.created["requests"].([]any)[0].(map[string]any)
	params := first["params"].(map[string]any)
	if params["model"] != "claude-haiku-5-5" || params["max_tokens"] != float64(256) {
		t.Fatalf("params = %v", params)
	}
	if _, ok := params["stream"]; ok {
		t.Fatal("batch params carry stream")
	}
	if sys := params["system"].([]any); len(sys) != 1 {
		t.Fatalf("system = %v", sys)
	}
	second := stub.created["requests"].([]any)[1].(map[string]any)["params"].(map[string]any)
	if tc := second["tool_choice"].(map[string]any); tc["name"] != "structured_output" {
		t.Fatalf("schema tool_choice = %v", tc)
	}

	if got[0].CustomID != "capital/fr" || got[0].Text() != "Paris" || got[0].Outcome != types.BatchSucceeded {
		t.Fatalf("result 0 = %+v", got[0])
	}
	if u := got[0].Usage; u.PromptTokens != 16 || u.CachedPromptTokens != 5 || u.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", u)
	}
	if got[1].Text() != `{"label":"spam"}` {
		t.Fatalf("structured result = %q", got[1].Text())
	}
	if got[2].Outcome != types.BatchErrored || !errors.Is(got[2].Err, types.ErrInvalidRequest) || !errors.Is(got[2].Err, types.ErrBatchRequest) {
		t.Fatalf("errored result = %+v", got[2])
	}
	if got[3].Outcome != types.BatchExpiredOutcome {
		t.Fatalf("expired result = %+v", got[3])
	}
	if stub.polls < 2 {
		t.Fatalf("polls = %d", stub.polls)
	}
}

// TestBatchRejectsAtSubmit checks D-12: a control the model cannot take, or
// a custom ID the API would refuse, fails before any request.
func TestBatchRejectsAtSubmit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL)))
	seed := int64(7)
	bad := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("x"))}, Options: types.RequestOptions{Seed: &seed}}}
	if _, err := a.Submit(context.Background(), bad, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("seed err = %v, want ErrInvalidModelConfig", err)
	}
	badID := []types.BatchRequest{{CustomID: "has space", Messages: []types.Message{types.UserMsg(types.Text("x"))}}}
	if _, err := a.Submit(context.Background(), badID, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("custom id err = %v, want ErrInvalidModelConfig", err)
	}
}

func TestBatchStatusCancelFind(t *testing.T) {
	stub := &batchServer{ids: []string{"sbx-0"}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "claude-haiku-5-5"}, WithBaseURL(server.URL)))
	ctx := context.Background()
	h := types.BatchHandle{ID: "msgbatch_1"}
	if err := a.Cancel(ctx, h); err != nil || stub.cancels != 1 {
		t.Fatalf("cancel = %v (%d)", err, stub.cancels)
	}
	if _, err := a.Status(ctx, types.BatchHandle{ID: "msgbatch_404"}); !errors.Is(err, types.ErrBatchNotFound) {
		t.Fatalf("404 err = %v, want ErrBatchNotFound", err)
	}
	// The listed batch ended with 4 requests; its first result names one of
	// ours, so the lookup finds it.
	stub.ids = []string{"sbx-0", "sbx-1", "sbx-2", "sbx-3"}
	since, _ := time.Parse(time.RFC3339, "2026-10-09T11:00:00Z")
	found, ok, err := a.FindBatch(ctx, types.BatchQuery{Since: since, Count: 4, CustomIDs: stub.ids})
	if err != nil || !ok || found.ID != "msgbatch_1" {
		t.Fatalf("find = %+v %v %v", found, ok, err)
	}
	if _, ok, err := a.FindBatch(ctx, types.BatchQuery{Since: since, Count: 4, CustomIDs: []string{"other-0"}}); ok || err != nil {
		t.Fatalf("find other = %v %v", ok, err)
	}
	// A batch created before the window is not a candidate.
	if _, ok, _ := a.FindBatch(ctx, types.BatchQuery{Since: since.Add(2 * time.Hour), Count: 4, CustomIDs: stub.ids}); ok {
		t.Fatal("found a batch older than the window")
	}
}

// TestBatchNativeSchema checks that a model that takes native structured
// output gets it in a batch too, as the streaming path sends it.
func TestBatchNativeSchema(t *testing.T) {
	stub := &batchServer{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "claude-sonnet-5-5"}, WithBaseURL(server.URL)))
	schema := &types.ParameterSchema{Type: "object", Required: []string{"label"},
		Properties: map[string]types.PropertyDef{"label": {Type: "string"}}}
	reqs := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("x"))}, Schema: schema}}
	if _, err := a.Submit(context.Background(), reqs, types.BatchSubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	params := stub.created["requests"].([]any)[0].(map[string]any)["params"].(map[string]any)
	format := params["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config.format = %v", format)
	}
	if _, ok := params["tool_choice"]; ok {
		t.Fatal("native schema output also forced a tool")
	}
}
