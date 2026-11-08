package credential_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/credential"
)

// Both connectors' real API clients, built with no static token, send the
// token the Source holds for their subject.
func TestConnectorsActWithTheSourcesToken(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/reactions.add":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":7}`))
		}
	}))
	defer srv.Close()

	src := credential.NewStatic()
	src.Set("slack", credential.App("T01"), "xoxb-installed")
	src.Set("github", credential.App("42"), "ghs-installation")
	ctx := context.Background()

	sl := slack.NewHTTPAPIAt(srv.URL, "", credential.Client(src, "slack", credential.App("T01"), srv.Client()))
	if err := sl.AddReaction(ctx, slack.Reaction{Channel: "C02", TS: "1.0", Name: "eyes"}); err != nil {
		t.Fatalf("slack: %v", err)
	}
	gh := github.NewHTTPAPIAt(srv.URL, "", credential.Client(src, "github", credential.App("42"), srv.Client()))
	if _, err := gh.CreateComment(ctx, github.Ref{Owner: "acme", Repo: "svc", Number: 5}, "hi"); err != nil {
		t.Fatalf("github: %v", err)
	}
	if seen["/reactions.add"] != "Bearer xoxb-installed" {
		t.Fatalf("slack sent %q", seen["/reactions.add"])
	}
	if seen["/repos/acme/svc/issues/5/comments"] != "Bearer ghs-installation" {
		t.Fatalf("github sent %v", seen)
	}
}
