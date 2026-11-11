package github_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
)

func TestFileReadKeepsTheImmutableRefAndEscapesPathComponents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/repos/acme/repo/contents/src/a b?#.go" || r.URL.Query().Get("ref") != commitSHA {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "size": 4, "content": base64.StdEncoding.EncodeToString([]byte("text")), "html_url": "https://github.com/acme/repo/blob/" + commitSHA + "/src/file.go"})
	}))
	defer server.Close()
	api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
	file, err := api.ReadFile(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 1}, commitSHA, "src/a b?#.go")
	if err != nil || file.Content != "text" || file.Commit != commitSHA {
		t.Fatalf("file %+v error %v", file, err)
	}
}

func TestFileReadDoesNotPretendPartialOrBinaryContentIsComplete(t *testing.T) {
	for _, body := range []string{`{"type":"file","encoding":"none","size":999999}`, `{"type":"file","encoding":"base64","size":1,"content":"AA=="}`, `{"type":"dir"}`, `{"type":"file","encoding":"base64","size":1,"content":"invalid"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		api := github.NewHTTPAPIAt(server.URL, "token", server.Client())
		_, err := api.ReadFile(context.Background(), github.Ref{Owner: "acme", Repo: "repo", Number: 1}, commitSHA, "file.go")
		server.Close()
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
