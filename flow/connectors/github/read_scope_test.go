package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/github"
)

func TestCrossSourceReadsRequireAnExactRepositoryGrant(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "docs", Number: 7}
	evidence := github.Context{Subject: github.ContextSubject{Title: "Referenced issue"}, Comments: []github.ContextComment{{ID: 1, Body: "Previous reply", Reactions: &github.ReactionCounts{Total: 4, ThumbsUp: 3, ThumbsDown: 1}}}}
	if err := fake.SeedContext(ref, evidence); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, source, reference string
		grants                  []string
		want                    bool
	}{
		{"Slack with grant", "slack://T01/C01", "https://github.com/acme/docs/issues/7#issuecomment-1", []string{"acme/docs"}, true},
		{"Slack without grant", "slack://T01/C01", "github://acme/docs/issues/7", nil, false},
		{"same repo", "github://acme/docs/issues/9", "github://acme/docs/issues/7", nil, true},
		{"other repo", "github://acme/other/issues/9", "github://acme/docs/issues/7", nil, false},
		{"arbitrary host", "slack://T01/C01", "https://evil.test/acme/docs/issues/7", []string{"acme/docs"}, false},
		{"credential URL", "slack://T01/C01", "https://user@github.com/acme/docs/issues/7", []string{"acme/docs"}, false},
		{"repo lookalike", "slack://T01/C01", "github://acme/docs-other/issues/7", []string{"acme/docs"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := github.New(github.Config{API: fake, ReadRepositories: tc.grants})
			if err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"address": tc.reference})
			result, err := c.Actions()[0].Invoke(context.Background(), address.MustParse(tc.source), args)
			if (err == nil) != tc.want {
				t.Fatalf("result %+v error %v", result, err)
			}
			if tc.want && (!strings.Contains(result.Summary, `"+1":3`) || !strings.Contains(result.Summary, `"-1":1`)) {
				t.Fatalf("feedback missing: %s", result.Summary)
			}
		})
	}
}

func TestReadGrantsNeverAuthorizeWritesFromAnotherSurface(t *testing.T) {
	c, _ := github.New(github.Config{API: github.NewFake(), ReadRepositories: []string{"acme/docs"}})
	for _, action := range c.Actions() {
		if action.Name == "github_comment" {
			_, err := action.Invoke(context.Background(), address.MustParse("slack://T01/C01"), json.RawMessage(`{"body":"write","address":"github://acme/docs/issues/7"}`))
			if err == nil {
				t.Fatal("read grant widened write target")
			}
		}
	}
}
