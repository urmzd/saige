package google

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// embedTransport answers embedContent requests with a fixed status and body,
// and records each request body.
type embedTransport struct {
	status int
	body   string
	mu     *sync.Mutex
	seen   *[]map[string]any
}

func (t embedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	t.mu.Lock()
	*t.seen = append(*t.seen, body)
	t.mu.Unlock()
	return &http.Response{StatusCode: t.status, Request: req,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(t.body))}, nil
}

func TestEmbedderTaskTypeAndErrors(t *testing.T) {
	const ok = `{"embeddings":[{"values":[0.1,0.2]}]}`
	const limited = `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`
	tests := []struct {
		name          string
		opts          []EmbedderOption
		purpose       types.EmbedPurpose
		status        int
		body          string
		wantTask      string
		wantTransient bool
	}{
		{name: "query purpose", purpose: types.PurposeQuery, status: 200, body: ok, wantTask: "RETRIEVAL_QUERY"},
		{name: "document purpose", purpose: types.PurposeDocument, status: 200, body: ok, wantTask: "RETRIEVAL_DOCUMENT"},
		{name: "no purpose sends none", status: 200, body: ok},
		{name: "fixed task type wins", opts: []EmbedderOption{WithTaskType("CLUSTERING")}, purpose: types.PurposeQuery, status: 200, body: ok, wantTask: "CLUSTERING"},
		{name: "rate limit is transient", status: 429, body: limited, wantTransient: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []map[string]any
			tr := embedTransport{status: tt.status, body: tt.body, mu: &sync.Mutex{}, seen: &seen}
			opts := append(tt.opts, WithEmbedHTTPClient(&http.Client{Transport: tr}))
			e, err := NewEmbedder(context.Background(), Config{APIKey: "k", Model: "text-embedding-004"}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx := types.WithEmbedPurpose(context.Background(), tt.purpose)
			out, err := e.Embed(ctx, []string{"hello"})
			if tt.status != 200 {
				if err == nil || types.IsTransient(err) != tt.wantTransient {
					t.Fatalf("err = %v, transient want %v", err, tt.wantTransient)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || len(out[0]) != 2 {
				t.Fatalf("embeddings = %v", out)
			}
			reqs, _ := seen[0]["requests"].([]any)
			if len(reqs) != 1 {
				t.Fatalf("requests = %v", seen[0])
			}
			got, _ := reqs[0].(map[string]any)["taskType"].(string)
			if got != tt.wantTask {
				t.Fatalf("taskType = %q, want %q (body %v)", got, tt.wantTask, seen[0])
			}
		})
	}
}
