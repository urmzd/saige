package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

type suggestionFixtureAPI struct {
	*Fake
	head        string
	changedHead string
	commit      SuggestionCommit
	files       map[string]string
	headReads   int
	fileErr     error
}

func (a *suggestionFixtureAPI) PullHead(context.Context, Ref) (string, error) {
	a.headReads++
	if a.headReads > 1 && a.changedHead != "" {
		return a.changedHead, nil
	}
	return a.head, nil
}
func (a *suggestionFixtureAPI) SuggestionCommit(context.Context, Ref, string) (SuggestionCommit, error) {
	return a.commit, nil
}
func (a *suggestionFixtureAPI) ReadFile(_ context.Context, _ Ref, sha, path string) (FileContent, error) {
	return FileContent{Path: path, Commit: sha, Content: a.files[sha]}, a.fileErr
}

func TestSuggestionCommittedNeedsExactImmutableEvidence(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, test := range []struct {
		name   string
		change func(*suggestionFixtureAPI)
		want   bool
	}{
		{"exact", func(*suggestionFixtureAPI) {}, true},
		{"head only", func(a *suggestionFixtureAPI) { a.files[head] = "prefix\nunrelated\nsuffix\n" }, false},
		{"additional edit in same file", func(a *suggestionFixtureAPI) { a.files[head] = "changed prefix\nfixed\nsuffix\n" }, false},
		{"nonadjacent commit", func(a *suggestionFixtureAPI) { a.commit.Parents[0].SHA = strings.Repeat("c", 40) }, false},
		{"missing patch", func(a *suggestionFixtureAPI) { a.commit.Files[0].Patch = "" }, false},
		{"head moved during reads", func(a *suggestionFixtureAPI) { a.changedHead = strings.Repeat("c", 40) }, false},
		{"bot author is not a human attribution", func(a *suggestionFixtureAPI) { a.commit.Author = CommitPerson{Login: "humagen[bot]", Type: "Bot"} }, true},
		{"forged author is not a human attribution", func(a *suggestionFixtureAPI) { a.commit.Author = CommitPerson{Login: "forged-person", Type: "User"} }, true},
		{"incomplete file", func(a *suggestionFixtureAPI) { a.fileErr = errors.New("file too large") }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &suggestionFixtureAPI{Fake: NewFake(), head: head, files: map[string]string{base: "prefix\nbroken\nsuffix\n", head: "prefix\nfixed\nsuffix\n"}}
			api.commit = SuggestionCommit{SHA: head, Author: CommitPerson{Login: "alice", Type: "User"}, Committer: CommitPerson{Login: "web-flow", Type: "User"}, Files: []ContextFile{{Filename: "a.go", Status: "modified", Patch: "@@ -2 +2 @@\n-broken\n+fixed"}}}
			api.commit.Parents = append(api.commit.Parents, struct {
				SHA string `json:"sha"`
			}{base})
			test.change(api)
			seen := []event.Feedback{}
			st := deliverystate.NewMemory()
			cfg := Config{API: api, Tenant: "t", DeliveryState: st, Login: "humagen[bot]", Feedback: connectors.FeedbackHandler{Record: func(context.Context, string, string, event.Feedback) error {
				t.Fatal("commit evidence reached human rating callback")
				return nil
			}, Evidence: func(_ context.Context, _ string, _ string, b event.Feedback) error {
				seen = append(seen, b)
				return nil
			}}}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			target := address.MustParse("github://acme/repo/pull/7?review_comment=42")
			comment := ReviewComment{FindingID: "f", Path: "a.go", Side: "RIGHT", Line: 2, Body: "Fix retry\n\n```suggestion\nfixed\n```"}
			if err := c.rememberSuggestion(t.Context(), target, comment, base, render.View{RunID: "run"}); err != nil {
				t.Fatal(err)
			}
			resolved := connectors.FeedbackTarget{Found: true, Run: "run", Finding: "f", Message: target.String()}
			err = c.pollSuggestion(t.Context(), target, resolved)
			if api.fileErr == nil && err != nil {
				t.Fatal(err)
			}
			if (len(seen) == 1) != test.want {
				t.Fatalf("feedback=%+v err=%v", seen, err)
			}
			if test.want {
				if seen[0].Actor.ID != "commit:"+head || !seen[0].Actor.Bot || seen[0].Reasons[0] != "suggestion_committed" || !strings.Contains(seen[0].Comment, head) {
					t.Fatal(seen)
				}
				c, _ = New(cfg)
				if err := c.pollSuggestion(t.Context(), target, resolved); err != nil {
					t.Fatal(err)
				}
				if len(seen) != 1 {
					t.Fatal("restart duplicated verified commit")
				}
			}
		})
	}
}

func TestExactSuggestionDeletionPreservesOtherLines(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	c := SuggestionCommit{SHA: head, Files: []ContextFile{{Filename: "a", Status: "modified", Patch: "@@ -2 +1,0 @@\n-remove"}}}
	c.Parents = append(c.Parents, struct {
		SHA string `json:"sha"`
	}{base})
	e := suggestionEvidence{Commit: base, Path: "a", Start: 2, End: 2}
	if !exactSuggestion(e, c, FileContent{Commit: base, Path: "a", Content: "keep\nremove\nlast\n"}, FileContent{Commit: head, Path: "a", Content: "keep\nlast\n"}) {
		t.Fatal("exact deletion not recognized")
	}
}
