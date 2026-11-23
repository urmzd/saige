package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/github"
)

var commentsRef = github.Ref{Owner: "acme", Repo: "repo", Number: 7}

func everyComment(github.ContextComment) bool { return true }

// counted is a server and the requests it was sent, as "path?page".
type counted struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
	auth  []string
}

func count(t *testing.T, handle http.HandlerFunc) *counted {
	t.Helper()
	c := &counted{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.asked = append(c.asked, r.URL.Query().Get("page"))
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *counted) pages() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fmt.Sprint(c.asked)
}

func writeComments(w http.ResponseWriter, from, to int) {
	out := []map[string]any{}
	for i := from; i <= to; i++ {
		out = append(out, map[string]any{"id": i, "body": fmt.Sprintf("note-%03d", i), "user": map[string]any{"login": "ana"}})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// commentsServer serves acme/repo#7's n comments, 30 to a page, oldest
// first. Its Link header names another server, elsewhere, which counts what
// it is sent.
func commentsServer(t *testing.T, n int) (api *github.HTTPAPI, srv, elsewhere *counted) {
	t.Helper()
	elsewhere = count(t, func(w http.ResponseWriter, _ *http.Request) { writeComments(w, 1, 0) })
	srv = count(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/repo/issues/7/comments" || r.URL.Query().Get("per_page") != "30" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if last := max(1, (n+29)/30); last > 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%[1]s/repositories/1/issues/7/comments?per_page=30&page=2>; rel="next", <%[1]s/repositories/1/issues/7/comments?per_page=30&page=%[2]d>; rel="last"`, elsewhere.URL, last))
		}
		writeComments(w, (page-1)*30+1, min(n, page*30))
	})
	return github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()), srv, elsewhere
}

// The latest comments are read in at most three requests, whatever the
// number of comments: page 1 for the Link header, the last page, and the one
// before it only when the last is short. Only a page number is read from
// the Link header: every request goes to the API's own base.
func TestTheLatestCommentsAreReadThroughTheLastPage(t *testing.T) {
	for _, tc := range []struct {
		n           int
		pages       string
		first, last int
	}{
		{0, "[1]", 0, 0},
		{3, "[1]", 1, 3},
		{30, "[1]", 1, 30},
		{33, "[1 2]", 1, 33},
		{62, "[1 3 2]", 31, 62},
		{90, "[1 3]", 61, 90},
		{300, "[1 10]", 271, 300},
		{301, "[1 11 10]", 271, 301},
		{299999, "[1 10000]", 299971, 299999},
	} {
		api, srv, elsewhere := commentsServer(t, tc.n)
		got, err := api.LatestComments(context.Background(), commentsRef, 5, everyComment)
		if err != nil {
			t.Fatalf("%d comments: %v", tc.n, err)
		}
		if srv.pages() != tc.pages || elsewhere.pages() != "[]" {
			t.Errorf("%d comments read pages %s, want %s; the Link header's host was sent %s", tc.n, srv.pages(), tc.pages, elsewhere.pages())
		}
		if tc.n == 0 {
			if len(got) != 0 {
				t.Errorf("no comments read %d", len(got))
			}
			continue
		}
		if len(got) > 60 || got[0].ID != int64(tc.first) || got[len(got)-1].ID != int64(tc.last) {
			t.Errorf("%d comments gave %d: %d..%d, want %d..%d", tc.n, len(got), got[0].ID, got[len(got)-1].ID, tc.first, tc.last)
		}
	}
}

// A last page with enough comments kept needs no page before it.
func TestAShortLastPageOfKeptCommentsReadsThePageBefore(t *testing.T) {
	api, srv, _ := commentsServer(t, 64)
	if _, err := api.LatestComments(context.Background(), commentsRef, 3, nil); err != nil || srv.pages() != "[1 3]" {
		t.Fatalf("pages %s, %v", srv.pages(), err)
	}
	api, srv, _ = commentsServer(t, 64)
	if _, err := api.LatestComments(context.Background(), commentsRef, 3, func(c github.ContextComment) bool { return c.ID < 60 }); err != nil || srv.pages() != "[1 3 2]" {
		t.Fatalf("pages %s, %v", srv.pages(), err)
	}
}

func TestLastPageReadsOnlyABoundedPageNumber(t *testing.T) {
	for link, want := range map[string]int{
		`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`: 9,
		`<https://api.github.com/x?page=2>; rel="next"`:                                                0,
		`<https://api.github.com/x?page=10000>; rel="last"`:                                            10000,
		`<https://api.github.com/x?page=10001>; rel="last"`:                                            0,
		`<https://api.github.com/x?page=99999999>; rel="last"`:                                         0,
		`<https://api.github.com/x?page=-1>; rel="last"`:                                               0,
		`<https://api.github.com/x?page=two>; rel="last"`:                                              0,
		`<://bad>; rel="last"`:                                                                         0,
		``:                                                                                             0,
	} {
		if got := github.LastPage(link); got != want {
			t.Errorf("LastPage(%q) = %d, want %d", link, got, want)
		}
	}
}

// A Link header naming a page past the bound is read as naming none, and a
// page is cut to its size however many comments the answer holds.
func TestTheLatestCommentsAreBounded(t *testing.T) {
	srv := count(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/x?page=10001>; rel="last"`)
		writeComments(w, 1, 500)
	})
	got, err := github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()).LatestComments(context.Background(), commentsRef, 1000, everyComment)
	if err != nil || len(got) != 30 || srv.pages() != "[1]" {
		t.Fatalf("%d comments, pages %s, %v", len(got), srv.pages(), err)
	}

	// An answer past the body bound is not read to its end.
	big := count(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"body":"` + strings.Repeat("x", 5<<20) + `"}]`))
	})
	if got, err := github.NewHTTPAPIAt(big.URL, "tok", big.Client()).LatestComments(context.Background(), commentsRef, 5, nil); err == nil {
		t.Fatalf("read %d comments from an answer past the bound", len(got))
	}

	for _, ref := range []github.Ref{{Owner: "acme", Repo: "repo"}, {Owner: "acme", Repo: "..", Number: 7}, {Owner: "a/b", Repo: "repo", Number: 7}, {Owner: "acme", Repo: "repo?x", Number: 7}} {
		if _, err := github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()).LatestComments(context.Background(), ref, 5, nil); err == nil {
			t.Errorf("read comments of %+v", ref)
		}
	}
	if srv.pages() != "[1]" {
		t.Fatalf("an invalid reference was requested: %s", srv.pages())
	}
}

// Comments deleted between page 1 and the last page leave the pages apart:
// an empty last page, or a short page before it, is ErrCommentsChanged, never
// a read of no comments or of too few.
func TestCommentsDeletedWhileReadAreAChange(t *testing.T) {
	for name, sizes := range map[string]map[int]int{
		"an empty last page":        {1: 30, 4: 0},
		"a short page before it":    {1: 30, 4: 2, 3: 10},
		"a last page gone entirely": {1: 30},
	} {
		srv := count(t, func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			// Page 1 was read when there were four pages.
			w.Header().Set("Link", `<https://api.github.com/x?per_page=30&page=4>; rel="last"`)
			writeComments(w, page*100, page*100+sizes[page]-1)
		})
		got, err := github.NewHTTPAPIAt(srv.URL, "", srv.Client()).LatestComments(context.Background(), commentsRef, 5, everyComment)
		if !errors.Is(err, github.ErrCommentsChanged) || got != nil {
			t.Errorf("%s: %d comments, %v; want ErrCommentsChanged", name, len(got), err)
		}
	}
}

// A redirect that stays on the API's host is followed with the token; one
// that leaves it is refused and the other host is sent nothing.
func TestTheLatestCommentsStayOnTheAPIHost(t *testing.T) {
	elsewhere := count(t, func(w http.ResponseWriter, _ *http.Request) { writeComments(w, 1, 3) })
	for _, tc := range []struct {
		name, to string
		want     bool
	}{
		{"the same host", "/repositories/1/issues/7/comments?per_page=30&page=1", true},
		{"another host", elsewhere.URL + "/repos/acme/repo/issues/7/comments?per_page=30&page=1", false},
		{"another scheme", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var srv *counted
			srv = count(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/repositories/") {
					writeComments(w, 1, 3)
					return
				}
				to := tc.to
				if to == "" {
					to = "https" + strings.TrimPrefix(srv.URL, "http") + "/repositories/1/issues/7/comments?page=1"
				}
				http.Redirect(w, r, to, http.StatusMovedPermanently)
			})
			got, err := github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()).LatestComments(context.Background(), commentsRef, 5, nil)
			if tc.want {
				if err != nil || len(got) != 3 || fmt.Sprint(srv.auth) != "[Bearer tok Bearer tok]" {
					t.Fatalf("%d comments, %v, authorization %v", len(got), err, srv.auth)
				}
				return
			}
			if err == nil || got != nil || len(srv.asked) != 1 {
				t.Fatalf("%d comments, %v, %d requests to the API", len(got), err, len(srv.asked))
			}
		})
	}
	if elsewhere.pages() != "[]" {
		t.Fatalf("another host was sent %s, authorization %v", elsewhere.pages(), elsewhere.auth)
	}

	// The caller's own redirect policy still applies on the same host.
	srv := count(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/1/issues/7/comments", http.StatusFound)
	})
	stop := srv.Client()
	stop.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("no redirects") }
	if _, err := github.NewHTTPAPIAt(srv.URL, "tok", stop).LatestComments(context.Background(), commentsRef, 5, nil); err == nil || !strings.Contains(err.Error(), "no redirects") {
		t.Fatalf("error %v, want the client's own refusal", err)
	}
}

// ReadComments reads only an issue or pull request on GitHub, under the
// grants every other read by reference has.
func TestReadCommentsTakesTheReadReferences(t *testing.T) {
	for _, tc := range []struct {
		name, source, reference string
		asked                   string
		pages                   string
	}{
		{"the run's own subject", "github://acme/repo/pull/7", "", "", "[1]"},
		{"its own repository by name", "github://acme/repo/pull/3", "acme/repo#7", "", "[1]"},
		{"another surface", "slack://T01/C01", "https://github.com/acme/repo/pull/7", "acme/repo", "[1]"},
		{"a repository", "slack://T01/C01", "acme/repo", "", "[]"},
		{"no reference from another surface", "slack://T01/C01", "", "", "[]"},
		{"another host", "slack://T01/C01", "https://evil.test/acme/repo/issues/7", "", "[]"},
		{"an API URL", "slack://T01/C01", "https://api.github.com/repos/acme/repo/issues/7/comments", "", "[]"},
		{"an owner not served", "slack://T01/C01", "partner/repo#7", "", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, srv, _ := commentsServer(t, 3)
			var asked []string
			c, err := github.New(github.Config{API: api, Owners: []string{"acme"}, MayRead: func(_ context.Context, owner, repo string) bool {
				asked = append(asked, owner+"/"+repo)
				return true
			}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.ReadComments(context.Background(), address.MustParse(tc.source), tc.reference, 5, nil)
			if strings.Join(asked, ",") != tc.asked || srv.pages() != tc.pages {
				t.Fatalf("MayRead %v, want %q; pages %s, want %s", asked, tc.asked, srv.pages(), tc.pages)
			}
			if (tc.pages == "[1]") != (err == nil && len(got) == 3) {
				t.Fatalf("%d comments, %v", len(got), err)
			}
		})
	}
}

// basicAPI is an API with none of the optional capabilities.
type basicAPI struct{ github.API }

func TestReadCommentsNeedsTheCapabilityAndReadsTheFake(t *testing.T) {
	source := address.MustParse("github://acme/repo/issues/7")
	c, err := github.New(github.Config{API: basicAPI{github.NewFake()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadComments(context.Background(), source, "", 5, nil); !errors.Is(err, connector.ErrUnsupported) {
		t.Fatalf("error %v, want ErrUnsupported", err)
	}

	fake := github.NewFake()
	var seeded github.Context
	for i := 1; i <= 64; i++ {
		seeded.Comments = append(seeded.Comments, github.ContextComment{ID: int64(i)})
	}
	if err := fake.SeedContext(commentsRef, seeded); err != nil {
		t.Fatal(err)
	}
	c, err = github.New(github.Config{API: fake})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		keep        func(github.ContextComment) bool
		first, last int64
	}{
		{nil, 61, 64},
		{func(c github.ContextComment) bool { return c.ID < 60 }, 31, 64},
	} {
		got, err := c.ReadComments(context.Background(), source, "", 3, tc.keep)
		if err != nil || len(got) == 0 || got[0].ID != tc.first || got[len(got)-1].ID != tc.last {
			t.Fatalf("comments %v, %v; want %d..%d", got, err, tc.first, tc.last)
		}
	}
}
