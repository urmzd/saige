package github_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/event"
)

var commitSHA = strings.Repeat("a", 40)

func TestApplicationPolicyOnlySeesVerifiedAndAllowedEvents(t *testing.T) {
	calls := 0
	c, _ := start(t, github.Config{Owners: []string{"acme"}, Automation: func(github.Hook) (string, bool) { calls++; return "review", true }})
	body := `{"action":"labeled","repository":{"name":"repo","owner":{"login":"acme"}},"sender":{"type":"User"},"issue":{"number":42},"label":{"name":"auto"}}`
	unsigned := httptest.NewRequest("POST", "/github", strings.NewReader(body))
	c.ServeHTTP(httptest.NewRecorder(), unsigned)
	c.ServeHTTP(httptest.NewRecorder(), deliver(t, "issues", "blocked", strings.Replace(body, "acme", "other", 1)))
	if calls != 0 {
		t.Fatal("unverified or disallowed event reached application policy")
	}
	c.ServeHTTP(httptest.NewRecorder(), deliver(t, "issues", "allowed", body))
	if calls != 1 {
		t.Fatalf("policy calls=%d", calls)
	}
}

func TestBaseConnectorHasNoProductMutationToolsOrReviewInterpretation(t *testing.T) {
	api := github.NewFake()
	c := sinkConnector(t, api)
	for _, a := range c.Actions() {
		if a.Name == "github_apply_changes" || a.Name == "github_run_checks" {
			t.Fatal("product command exposed by transport")
		}
	}
	if err := c.Deliver(context.Background(), target, ev(1, event.TypeCompleted, `{"text":"ordinary answer"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.Comments()[0].Body, "ordinary answer") {
		t.Fatal("missing ordinary answer")
	}
}
