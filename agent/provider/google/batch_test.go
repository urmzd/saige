package google

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func redirectTo(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	target, _ := url.Parse(srv.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
}

func batchReqs() []types.BatchRequest {
	return []types.BatchRequest{
		{CustomID: "capital", Messages: []types.Message{types.SystemMsg(types.Text("Be brief.")), types.UserMsg(types.Text("Capital of France?"))}},
		{CustomID: "label", Messages: []types.Message{types.UserMsg(types.Text("Win a prize"))},
			Schema: &types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"label": {Type: "string"}}}},
		{CustomID: "bad", Messages: []types.Message{types.UserMsg(types.Text("x"))}},
	}
}

const geminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"%s"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":15},"modelVersion":"gemini-3.1-flash-lite"}`

func geminiText(text string) string { return strings.Replace(geminiResponse, "%s", text, 1) }

// TestGeminiBatch runs a batch on the Gemini API against a stub: inline
// requests carry their custom ID in metadata, and inline responses map back.
func TestGeminiBatch(t *testing.T) {
	var mu sync.Mutex
	var created map[string]any
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":batchGenerateContent"):
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = io.WriteString(w, `{"name":"batches/b1","metadata":{"state":"BATCH_STATE_PENDING","displayName":"tag","createTime":"2026-10-09T12:00:00Z"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batches/b1"):
			polls++
			if polls == 1 {
				_, _ = io.WriteString(w, `{"name":"batches/b1","metadata":{"state":"BATCH_STATE_RUNNING"}}`)
				return
			}
			reqs := created["batch"].(map[string]any)["inputConfig"].(map[string]any)["requests"].(map[string]any)["requests"].([]any)
			key := func(i int) string {
				return reqs[i].(map[string]any)["metadata"].(map[string]any)[batchKey].(string)
			}
			_, _ = io.WriteString(w, `{"name":"batches/b1","metadata":{"state":"BATCH_STATE_SUCCEEDED","output":{"inlinedResponses":{"inlinedResponses":[`+
				`{"metadata":{"`+batchKey+`":"`+key(0)+`"},"response":`+geminiText("Paris")+`},`+
				`{"metadata":{"`+batchKey+`":"`+key(1)+`"},"response":`+geminiText(`{\"label\":\"spam\"}`)+`},`+
				`{"metadata":{"`+batchKey+`":"`+key(2)+`"},"error":{"code":3,"message":"invalid argument"}}]}}}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/batches/b1:cancel"):
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a, err := New(context.Background(), Config{APIKey: "key", Model: "gemini-3.1-flash-lite"}, WithHTTPClient(redirectTo(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	r := must.Get(batch.NewRunner(batch.RunnerConfig{Provider: a, Store: batch.NewMemoryStore()}, batch.WithPollInterval(time.Millisecond, time.Millisecond)))
	got, err := r.Run(context.Background(), "job-gemini", batchReqs())
	if err != nil {
		t.Fatal(err)
	}
	inline := created["batch"].(map[string]any)["inputConfig"].(map[string]any)["requests"].(map[string]any)["requests"].([]any)
	if len(inline) != 3 {
		t.Fatalf("inline requests = %d", len(inline))
	}
	second := inline[1].(map[string]any)["request"].(map[string]any)
	if gc := second["generationConfig"].(map[string]any); gc["responseMimeType"] != "application/json" {
		t.Fatalf("schema request = %v", second)
	}
	if got[0].Text() != "Paris" || got[0].Usage.CompletionTokens != 5 || got[0].Usage.PromptTokens != 10 {
		t.Fatalf("result 0 = %+v", got[0])
	}
	if got[1].Text() != `{"label":"spam"}` {
		t.Fatalf("result 1 = %q", got[1].Text())
	}
	if got[2].Outcome != types.BatchErrored || got[2].Err == nil {
		t.Fatalf("result 2 = %+v", got[2])
	}
	if err := a.Cancel(context.Background(), types.BatchHandle{ID: "batches/b1"}); err != nil {
		t.Fatal(err)
	}
}

// TestVertexBatch runs a batch through Vertex AI batch prediction against
// stubs of Cloud Storage and the batchPredictionJobs API. Output lines come
// back in reverse order and map back through the request labels.
func TestVertexBatch(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]string{}
	var job map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/bkt/o":
			body, _ := io.ReadAll(r.Body)
			objects[r.URL.Query().Get("name")] = string(body)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/bkt/o":
			prefix := r.URL.Query().Get("prefix")
			var items []string
			for name := range objects {
				if strings.HasPrefix(name, prefix) {
					items = append(items, `{"name":"`+name+`"}`)
				}
			}
			_, _ = io.WriteString(w, `{"items":[`+strings.Join(items, ",")+`]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/bkt/o/"):
			name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b/bkt/o/"))
			_, _ = io.WriteString(w, objects[name])
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/batchPredictionJobs"):
			_ = json.NewDecoder(r.Body).Decode(&job)
			_, _ = io.WriteString(w, `{"name":"projects/proj/locations/us-central1/batchPredictionJobs/42","state":"JOB_STATE_PENDING"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/batchPredictionJobs/42"):
			out := job["outputConfig"].(map[string]any)["gcsDestination"].(map[string]any)["outputUriPrefix"].(string)
			dir := strings.TrimPrefix(out, "gs://bkt/") + "/prediction-model-1/"
			in := strings.Split(strings.TrimSpace(objects[strings.TrimPrefix(job["inputConfig"].(map[string]any)["gcsSource"].(map[string]any)["uris"].([]any)[0].(string), "gs://bkt/")]), "\n")
			var lines []string
			for i := len(in) - 1; i >= 0; i-- {
				var l map[string]any
				_ = json.Unmarshal([]byte(in[i]), &l)
				reqJSON, _ := json.Marshal(l["request"])
				resp := geminiText("answer")
				status := `""`
				if i == 2 {
					resp, status = `{}`, `"Bad Request: 400 INVALID_ARGUMENT"`
				}
				lines = append(lines, `{"status":`+status+`,"processed_time":"2026-10-09T12:10:00Z","request":`+string(reqJSON)+`,"response":`+resp+`}`)
			}
			objects[dir+"predictions.jsonl"] = strings.Join(lines, "\n") + "\n"
			_, _ = io.WriteString(w, `{"name":"projects/proj/locations/us-central1/batchPredictionJobs/42","state":"JOB_STATE_SUCCEEDED",`+
				`"outputConfig":{"predictionsFormat":"jsonl","gcsDestination":{"outputUriPrefix":"`+out+`"}},"completionStats":{"successfulCount":"2","failedCount":"1"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a, err := New(context.Background(), Config{Model: "gemini-3.1-flash-lite"},
		WithVertex("proj", "us-central1"), WithHTTPClient(redirectTo(t, srv)), WithCredentials(testCredentials("tok")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit(context.Background(), batchReqs(), types.BatchSubmitOptions{}); !errors.Is(err, types.ErrInvalidModelConfig) {
		t.Fatalf("adapter on vertex err = %v, want a pointer to NewVertexBatch", err)
	}
	vb, err := NewVertexBatch(a, "gs://bkt/saige", WithStorageEndpoint(srv.URL), WithStorageClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	r := must.Get(batch.NewRunner(batch.RunnerConfig{Provider: vb, Store: batch.NewMemoryStore()}, batch.WithPollInterval(time.Millisecond, time.Millisecond)))
	got, err := r.Run(context.Background(), "job-vertex", batchReqs())
	if err != nil {
		t.Fatal(err)
	}
	if model := job["model"]; !strings.HasSuffix(model.(string), "gemini-3.1-flash-lite") {
		t.Fatalf("job model = %v", model)
	}
	if got[0].Text() != "answer" || got[1].Text() != "answer" {
		t.Fatalf("results = %+v", got)
	}
	if got[2].Outcome != types.BatchErrored || !strings.Contains(got[2].Err.Error(), "INVALID_ARGUMENT") {
		t.Fatalf("result 2 = %+v", got[2])
	}
	if _, err := NewVertexBatch(a, "s3://nope"); err == nil {
		t.Fatal("accepted a non-gs location")
	}
}
