package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// batchStub stubs the Files and Batch APIs. Output and error lines come
// back in reverse order.
type batchStub struct {
	mu       sync.Mutex
	lines    []map[string]any
	create   map[string]any
	polls    int
	purpose  string
	endpoint string
}

func (s *batchStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			s.purpose = r.FormValue("purpose")
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("file: %v", err)
				return
			}
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				var l map[string]any
				if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
					t.Errorf("input line: %v", err)
				}
				s.lines = append(s.lines, l)
			}
			_, _ = io.WriteString(w, `{"id":"file-in","object":"file","bytes":1,"created_at":1,"filename":"batch.jsonl","purpose":"batch"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/batches"):
			_ = json.NewDecoder(r.Body).Decode(&s.create)
			s.endpoint, _ = s.create["endpoint"].(string)
			_, _ = io.WriteString(w, s.batch("validating"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batches/batch_1"):
			s.polls++
			status := "completed"
			if s.polls == 1 {
				status = "in_progress"
			}
			_, _ = io.WriteString(w, s.batch(status))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files/file-out/content"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, s.output())
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files/file-err/content"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, `{"id":"r3","custom_id":"`+s.id(3)+`","response":null,"error":{"code":"batch_expired","message":"This request could not be executed before the completion window expired."}}`+"\n"+
				`{"id":"r2","custom_id":"`+s.id(2)+`","response":{"status_code":400,"request_id":"q","body":{"error":{"message":"bad schema","type":"invalid_request_error","code":"invalid_value"}}},"error":null}`+"\n")
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batches"):
			_, _ = io.WriteString(w, `{"object":"list","data":[`+s.batch("in_progress")+`],"has_more":false}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/batches/batch_1/cancel"):
			_, _ = io.WriteString(w, s.batch("cancelling"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func (s *batchStub) id(i int) string {
	if i < len(s.lines) {
		return s.lines[i]["custom_id"].(string)
	}
	return ""
}

func (s *batchStub) batch(status string) string {
	b := map[string]any{"id": "batch_1", "object": "batch", "endpoint": s.endpoint, "input_file_id": "file-in",
		"completion_window": "24h", "status": status, "created_at": 1791547200, "metadata": map[string]string{batchTagKey: "tag-1"},
		"request_counts": map[string]int{"total": 4, "completed": 2, "failed": 2}}
	if status == "completed" {
		b["output_file_id"], b["error_file_id"], b["completed_at"] = "file-out", "file-err", 1791549000
	}
	raw, _ := json.Marshal(b)
	return string(raw)
}

func (s *batchStub) output() string {
	if s.endpoint == "/v1/responses" {
		return `{"id":"r1","custom_id":"` + s.id(1) + `","response":{"status_code":200,"request_id":"q1","body":{"id":"resp_2","model":"gpt-6-luna","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":0}}}},"error":null}` + "\n" +
			`{"id":"r0","custom_id":"` + s.id(0) + `","response":{"status_code":200,"request_id":"q0","body":{"id":"resp_1","model":"gpt-6-luna","status":"completed","output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"Paris"}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14,"input_tokens_details":{"cached_tokens":4}}}},"error":null}` + "\n"
	}
	return `{"id":"r1","custom_id":"` + s.id(1) + `","response":{"status_code":200,"request_id":"q1","body":{"id":"cc_2","model":"gpt-6-luna","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}},"error":null}` + "\n" +
		`{"id":"r0","custom_id":"` + s.id(0) + `","response":{"status_code":200,"request_id":"q0","body":{"id":"cc_1","model":"gpt-6-luna","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Paris"}}],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":4}}}},"error":null}` + "\n"
}

func batchRequests() []types.BatchRequest {
	tool := types.ToolDef{Name: "lookup", Description: "look up", Parameters: types.ParameterSchema{Type: "object",
		Properties: map[string]types.PropertyDef{"q": {Type: "string"}}, Required: []string{"q"}}}
	return []types.BatchRequest{
		{CustomID: "capital", Messages: []types.Message{types.UserMsg(types.Text("Capital of France?"))}},
		{CustomID: "tool", Messages: []types.Message{types.UserMsg(types.Text("look up x"))}, Tools: []types.ToolDef{tool}},
		{CustomID: "bad", Messages: []types.Message{types.UserMsg(types.Text("y"))}},
		{CustomID: "late", Messages: []types.Message{types.UserMsg(types.Text("z"))}},
	}
}

func checkBatchResults(t *testing.T, got []types.BatchResult) {
	t.Helper()
	if got[0].CustomID != "capital" || got[0].Text() != "Paris" || got[0].Usage.CachedPromptTokens != 4 {
		t.Fatalf("result 0 = %+v", got[0])
	}
	calls := 0
	for _, c := range got[1].Message.Parts {
		if tu, ok := c.(types.ToolCallPart); ok && tu.Name == "lookup" && tu.Arguments["q"] == "x" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("result 1 = %+v, want one lookup call", got[1].Message)
	}
	if got[2].Outcome != types.BatchErrored || !errors.Is(got[2].Err, types.ErrInvalidRequest) {
		t.Fatalf("result 2 = %+v", got[2])
	}
	if got[3].Outcome != types.BatchExpiredOutcome {
		t.Fatalf("result 3 = %+v", got[3])
	}
}

// TestBatchChatCompletions runs a Chat Completions batch against the stub:
// the uploaded file holds one POST per request without stream options, the
// tag rides in metadata, and the output and error files map back by ID.
func TestBatchChatCompletions(t *testing.T) {
	stub := &batchStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "gpt-6-luna"}, WithBaseURL(server.URL), WithMaxTokens(64)))
	r := batch.NewRunner(a, batch.NewMemoryStore(), batch.WithPollInterval(time.Millisecond, time.Millisecond))
	got, err := r.Run(context.Background(), "job-openai", batchRequests())
	if err != nil {
		t.Fatal(err)
	}
	if stub.purpose != "batch" || stub.endpoint != "/v1/chat/completions" || stub.create["completion_window"] != "24h" {
		t.Fatalf("upload purpose %q, create %v", stub.purpose, stub.create)
	}
	if md := stub.create["metadata"].(map[string]any); md[batchTagKey] == "" {
		t.Fatalf("metadata = %v", md)
	}
	line := stub.lines[0]
	body := line["body"].(map[string]any)
	if line["method"] != "POST" || line["url"] != "/v1/chat/completions" || body["model"] != "gpt-6-luna" || body["max_completion_tokens"] != float64(64) {
		t.Fatalf("line = %v", line)
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatal("batch body carries stream options")
	}
	checkBatchResults(t, got)
}

// TestBatchResponses runs the same batch on /v1/responses.
func TestBatchResponses(t *testing.T) {
	stub := &batchStub{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	ra := must.Get(NewResponses(Config{APIKey: "k", Model: "gpt-6-luna"}, WithBaseURL(server.URL)))
	r := batch.NewRunner(ra, batch.NewMemoryStore(), batch.WithPollInterval(time.Millisecond, time.Millisecond))
	got, err := r.Run(context.Background(), "job-responses", batchRequests())
	if err != nil {
		t.Fatal(err)
	}
	if stub.endpoint != "/v1/responses" || stub.lines[0]["url"] != "/v1/responses" {
		t.Fatalf("endpoint = %q", stub.endpoint)
	}
	if body := stub.lines[0]["body"].(map[string]any); body["store"] != false {
		t.Fatalf("body = %v, want store false", body)
	}
	checkBatchResults(t, got)
}

// TestBatchRejectsAtSubmit checks D-12 on both endpoints: a control the API
// cannot take fails before the input file is uploaded.
func TestBatchRejectsAtSubmit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	topK := 5.0
	reqs := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("x"))}, Options: types.RequestOptions{TopK: &topK}}}
	if _, err := must.Get(New(Config{APIKey: "k", Model: "gpt-6-luna"}, WithBaseURL(server.URL))).Submit(context.Background(), reqs, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("chat err = %v", err)
	}
	stop := []types.BatchRequest{{CustomID: "a", Messages: []types.Message{types.UserMsg(types.Text("x"))}, Options: types.RequestOptions{StopSequences: []string{"END"}}}}
	if _, err := must.Get(NewResponses(Config{APIKey: "k", Model: "gpt-6-luna"}, WithBaseURL(server.URL))).Submit(context.Background(), stop, types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("responses err = %v", err)
	}
}

func TestBatchFindAndCancel(t *testing.T) {
	stub := &batchStub{endpoint: "/v1/responses"}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	a := must.Get(New(Config{APIKey: "k", Model: "gpt-6-luna"}, WithBaseURL(server.URL)))
	ctx := context.Background()
	since := time.Unix(1791547200, 0).Add(-time.Minute)
	h, ok, err := a.FindBatch(ctx, types.BatchQuery{Tag: "tag-1", Since: since})
	if err != nil || !ok || h.ID != "batch_1" || h.Meta[metaEndpoint] != "/v1/responses" {
		t.Fatalf("find = %+v %v %v", h, ok, err)
	}
	if _, ok, _ := a.FindBatch(ctx, types.BatchQuery{Tag: "other", Since: since}); ok {
		t.Fatal("found a batch with another tag")
	}
	if err := a.Cancel(ctx, h); err != nil {
		t.Fatal(err)
	}
}
