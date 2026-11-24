package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/connectors/github"
)

// dismissBackend is one API under test with a way to seed reviews and to see
// how many dismissals reached it.
type dismissBackend struct {
	api        github.API
	seed       func(t *testing.T, author, event string) int64
	dismissals func() int
}

func fakeDismissBackend() dismissBackend {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "repo", Number: 7}
	return dismissBackend{
		api: fake,
		seed: func(t *testing.T, author, event string) int64 {
			id, err := fake.SeedReview(ref, author, event, commitSHA)
			if err != nil {
				t.Fatal(err)
			}
			return id
		},
		dismissals: func() int {
			n := 0
			for _, r := range fake.SubmittedReviews() {
				if r.Dismissal != "" {
					n++
				}
			}
			return n
		},
	}
}

type serverReview struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func httpDismissBackend(t *testing.T) dismissBackend {
	var mu sync.Mutex
	var reviews []*serverReview
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const base = "/repos/acme/repo/pulls/7/reviews"
		rest := strings.TrimPrefix(r.URL.Path, base)
		switch {
		case r.Method == http.MethodGet && rest == "":
			_ = json.NewEncoder(w).Encode(reviews)
		case r.Method == http.MethodGet:
			id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "/"), 10, 64)
			for _, v := range reviews {
				if v.ID == id {
					_ = json.NewEncoder(w).Encode(v)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPut && strings.HasSuffix(rest, "/dismissals"):
			var body struct{ Message, Event string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" || body.Event != "DISMISS" {
				t.Errorf("dismissal body %+v, %v", body, err)
			}
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/dismissals"), 10, 64)
			for _, v := range reviews {
				if v.ID == id && (v.State == "APPROVED" || v.State == "CHANGES_REQUESTED") {
					v.State = "DISMISSED"
					puts++
					_ = json.NewEncoder(w).Encode(v)
					return
				}
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	states := map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}
	return dismissBackend{
		api: github.NewHTTPAPIAt(server.URL, "token", server.Client()),
		seed: func(_ *testing.T, author, event string) int64 {
			mu.Lock()
			defer mu.Unlock()
			v := &serverReview{ID: int64(len(reviews) + 1), State: states[event], CommitID: commitSHA}
			v.User.Login = author
			reviews = append(reviews, v)
			return v.ID
		},
		dismissals: func() int { mu.Lock(); defer mu.Unlock(); return puts },
	}
}

// The fake and the HTTP API give the same answers: the app's own approval and
// change request are dismissed; a comment, a dismissed review and another
// login's review are refused before any dismissal is sent.
func TestDismissOnlyTheAppsOwnApprovalOrChangeRequest(t *testing.T) {
	ctx := context.Background()
	for name, backend := range map[string]func(*testing.T) dismissBackend{
		"fake": func(*testing.T) dismissBackend { return fakeDismissBackend() },
		"http": httpDismissBackend,
	} {
		t.Run(name, func(t *testing.T) {
			b := backend(t)
			c, err := github.New(github.Config{API: b.api, Login: "humagen[bot]"})
			if err != nil {
				t.Fatal(err)
			}
			approve := b.seed(t, "humagen[bot]", "APPROVE")
			changes := b.seed(t, "Humagen[bot]", "REQUEST_CHANGES")
			comment := b.seed(t, "humagen[bot]", "COMMENT")
			person := b.seed(t, "alice", "APPROVE")

			own, err := c.OwnReviews(ctx, "acme", "repo", 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(own) != 3 || own[0].ID != approve || own[0].State != "APPROVED" || own[0].CommitID != commitSHA || own[0].User.Login != "humagen[bot]" {
				t.Fatalf("own reviews %+v, want the app's three with ID, state, commit and author", own)
			}

			for _, id := range []int64{approve, changes} {
				if err := c.DismissReview(ctx, "acme", "repo", 7, id, "Superseded by a review of the new head"); err != nil {
					t.Fatalf("dismiss own review %d: %v", id, err)
				}
			}
			for what, id := range map[string]int64{"a comment": comment, "another login's review": person, "a dismissed review": approve} {
				if err := c.DismissReview(ctx, "acme", "repo", 7, id, "x"); !errors.Is(err, github.ErrNotOwnReview) {
					t.Errorf("dismissing %s answered %v, want ErrNotOwnReview", what, err)
				}
			}
			if err := c.DismissReview(ctx, "acme", "repo", 7, changes, " "); err == nil {
				t.Error("dismissed with an empty message")
			}
			if n := b.dismissals(); n != 2 {
				t.Fatalf("%d dismissals reached the API, want 2", n)
			}
			own, _ = c.OwnReviews(ctx, "acme", "repo", 7)
			for _, r := range own[:2] {
				if r.State != "DISMISSED" {
					t.Errorf("review %d is %s after dismissal", r.ID, r.State)
				}
			}
		})
	}
}

func TestDismissNeedsTheAppLoginAndAServedOwner(t *testing.T) {
	ctx := context.Background()
	b := fakeDismissBackend()
	id := b.seed(t, "humagen[bot]", "APPROVE")
	noLogin, _ := github.New(github.Config{API: b.api})
	if err := noLogin.DismissReview(ctx, "acme", "repo", 7, id, "x"); !errors.Is(err, github.ErrNotOwnReview) {
		t.Fatalf("no login answered %v", err)
	}
	if _, err := noLogin.OwnReviews(ctx, "acme", "repo", 7); err == nil {
		t.Fatal("no login listed own reviews")
	}
	other, _ := github.New(github.Config{API: b.api, Login: "humagen[bot]", Owners: []string{"globex"}})
	if err := other.DismissReview(ctx, "acme", "repo", 7, id, "x"); err == nil {
		t.Fatal("dismissed on an owner the connector does not serve")
	}
	if b.dismissals() != 0 {
		t.Fatal("a refused dismissal reached the API")
	}
}

// Dismissal is the host's alone: no model action offers it.
func TestNoModelToolDismisses(t *testing.T) {
	c, err := github.New(github.Config{API: github.NewFake(), Login: "humagen[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range c.Actions() {
		if strings.Contains(strings.ToLower(action.Name+" "+action.Description+" "+string(action.Schema)), "dismiss") {
			t.Fatalf("model action %s offers dismissal", action.Name)
		}
	}
}
