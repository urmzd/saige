package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/event"
)

const hookSHA = "316c56f81540c3eaca02184173df322a48aebdd9"

// hookProbe records what the read hooks were asked and what reached the API.
type hookProbe struct {
	mu       sync.Mutex
	mayRead  []string
	refused  []string
	requests []string
	// early is set when a hook was asked after a request had been made.
	early bool
}

func (p *hookProbe) snapshot() (mayRead, refused, requests []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.mayRead), slices.Clone(p.refused), slices.Clone(p.requests)
}

// probed is a connector over the HTTP API of a local server that answers
// every request 404, with both hooks recording. grant and refuse are what
// MayRead and ReadRefused answer. edit changes the Config before New.
func probed(t *testing.T, grant, refuse bool, edit func(*github.Config)) (*github.Connector, *hookProbe) {
	t.Helper()
	p := &hookProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cfg := github.Config{
		API:              github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()),
		Owners:           []string{"acme"},
		FixPullRequests:  true,
		AuthorizeFixPull: func(context.Context, github.Ref) error { return nil },
		MayRead: func(_ context.Context, owner, repo string) bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.mayRead = append(p.mayRead, owner+"/"+repo)
			p.early = p.early || len(p.requests) > 0
			return grant
		},
		ReadRefused: func(_ context.Context, owner, repo string) bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.refused = append(p.refused, owner+"/"+repo)
			p.early = p.early || len(p.requests) > 0
			return refuse
		},
	}
	if edit != nil {
		edit(&cfg)
	}
	c, err := github.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

type entryKind string

const (
	// reads takes a reference: the hooks are asked for a repository other
	// than the run's own.
	reads entryKind = "read"
	// writes changes GitHub: the hooks are never asked.
	writes entryKind = "write"
	// bound reads an address the host supplied: the hooks are never asked.
	bound entryKind = "bound read"
	// ingress receives deliveries: the hooks are never asked.
	ingress entryKind = "ingress"
	// inProcess makes no request.
	inProcess entryKind = "local"
)

// entryPoints is every way into the connector: each action Actions returns
// and each exported method. A new one fails
// TestEveryEntryPointChoosesWhetherItAsksTheReadHooks until it is listed.
// args are an action's arguments naming github://acme/docs/pull/7.
var entryPoints = map[string]struct {
	kind entryKind
	args string
}{
	"action github_read_context":    {reads, `{"address":"github://acme/docs/pull/7"}`},
	"action github_read_file":       {reads, `{"address":"github://acme/docs/pull/7","path":"README.md","ref":"` + hookSHA + `"}`},
	"action github_workflow_status": {reads, `{"address":"github://acme/docs/pull/7","ref":"` + hookSHA + `"}`},
	"action github_fix_base":        {reads, `{"address":"github://acme/docs/pull/7"}`},
	"method ReadComments":           {reads, ""},

	"action github_comment":        {writes, `{"address":"github://acme/docs/pull/7","body":"note"}`},
	"action github_request_review": {writes, `{"address":"github://acme/docs/pull/7","reviewers":["ada"]}`},
	"action github_add_labels":     {writes, `{"address":"github://acme/docs/pull/7","labels":["bug"]}`},
	"action github_create_review":  {writes, `{"address":"github://acme/docs/pull/7","commit_id":"` + hookSHA + `","event":"COMMENT","body":"note"}`},
	"action github_create_fix_pr":  {writes, `{"address":"github://acme/docs/pull/7","base":{"name":"main","sha":"` + hookSHA + `"},"title":"Fix","body":"Fix","changes":{"message":"fix","files":[{"path":"a.txt","content":"a"}]}}`},
	"method Deliver":               {writes, ""},
	"method Flush":                 {writes, ""},

	"method Messages":     {bound, ""},
	"method PollFeedback": {bound, ""},

	"method ServeHTTP": {ingress, ""},
	"method Ingest":    {ingress, ""},

	"method Name":      {inProcess, ""},
	"method Ingesting": {inProcess, ""},
	"method Actions":   {inProcess, ""},
	"method Reference": {inProcess, ""},
}

// enter calls one entry point for a run raised on source. The methods that
// take no reference are called on the address a host would bind.
func enter(t *testing.T, c *github.Connector, name string, source address.Address) error {
	t.Helper()
	ctx := context.Background()
	docs := address.MustParse("github://acme/docs/pull/7")
	kind, rest, _ := strings.Cut(name, " ")
	if kind == "action" {
		for _, a := range c.Actions() {
			if a.Name == rest {
				_, err := a.Invoke(ctx, source, json.RawMessage(entryPoints[name].args))
				return err
			}
		}
		t.Fatalf("no action %s", rest)
	}
	switch rest {
	case "ReadComments":
		_, err := c.ReadComments(ctx, source, docs.String(), 5, nil)
		return err
	case "Messages":
		_, err := c.Messages(ctx, "", docs)
		return err
	case "PollFeedback":
		_, err := c.PollFeedback(ctx, docs, 1)
		return err
	case "Deliver":
		return c.Deliver(ctx, docs, event.Envelope{})
	case "Flush":
		return c.Flush(ctx)
	case "ServeHTTP":
		c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
	case "Ingest":
		done, cancel := context.WithCancel(ctx)
		cancel()
		return c.Ingest(done, func(context.Context, event.Envelope) error { return nil })
	case "Name":
		_ = c.Name()
	case "Ingesting":
		_ = c.Ingesting()
	case "Actions":
		_ = c.Actions()
	case "Reference":
		_, err := c.Reference(docs)
		return err
	default:
		t.Fatalf("entry point %s is listed but enter does not call it", name)
	}
	return nil
}

func TestEveryEntryPointChoosesWhetherItAsksTheReadHooks(t *testing.T) {
	c, _ := probed(t, true, false, nil)
	var have []string
	for _, a := range c.Actions() {
		have = append(have, "action "+a.Name)
	}
	typ := reflect.TypeOf(c)
	for i := range typ.NumMethod() {
		have = append(have, "method "+typ.Method(i).Name)
	}
	var listed []string
	for name := range entryPoints {
		listed = append(listed, name)
	}
	sort.Strings(have)
	sort.Strings(listed)
	if !slices.Equal(have, listed) {
		t.Fatalf("entry points and the table differ; list every action and exported method with its kind\nconnector: %v\ntable:     %v", have, listed)
	}

	slack := address.MustParse("slack://T01/C01")
	own := address.MustParse("github://acme/docs/pull/7")
	const docs = "acme/docs"
	for _, name := range listed {
		entry := entryPoints[name]
		t.Run(name, func(t *testing.T) {
			if entry.kind != reads {
				// Not a read by reference: the hooks are never asked, from
				// another surface or from the repository's own run, and a
				// grant they would give lets nothing through.
				for _, source := range []address.Address{slack, own} {
					c, p := probed(t, true, false, nil)
					err := enter(t, c, name, source)
					mayRead, refused, requests := p.snapshot()
					if len(mayRead) != 0 || len(refused) != 0 {
						t.Fatalf("from %s the hooks were asked: MayRead %v, ReadRefused %v", source, mayRead, refused)
					}
					if entry.kind == writes && strings.HasPrefix(name, "action ") && source.Connector != github.Name && (err == nil || len(requests) != 0) {
						t.Fatalf("a write from %s: error %v, requests %v", source, err, requests)
					}
				}
				return
			}

			// Granted: both hooks are asked once, before any request, about
			// the repository that is then requested.
			c, p := probed(t, true, false, nil)
			_ = enter(t, c, name, slack)
			mayRead, refused, requests := p.snapshot()
			if !slices.Equal(mayRead, []string{docs}) || !slices.Equal(refused, []string{docs}) || p.early {
				t.Fatalf("granted: MayRead %v, ReadRefused %v, asked after a request: %t", mayRead, refused, p.early)
			}
			if len(requests) == 0 {
				t.Fatal("granted: nothing was requested")
			}
			for _, r := range requests {
				if !strings.Contains(r, " /repos/acme/docs") {
					t.Fatalf("granted: requested %s, not the repository the hooks were asked about", r)
				}
			}

			// Not granted: asked once, nothing requested, a *ReadError.
			c, p = probed(t, false, false, nil)
			err := enter(t, c, name, slack)
			mayRead, refused, requests = p.snapshot()
			var refusal *github.ReadError
			if !errors.As(err, &refusal) || *refusal != (github.ReadError{Owner: "acme", Repo: "docs"}) || !errors.Is(err, connector.ErrUndeliverable) {
				t.Fatalf("not granted: error %v", err)
			}
			if !slices.Equal(mayRead, []string{docs}) || !slices.Equal(refused, []string{docs}) || len(requests) != 0 {
				t.Fatalf("not granted: MayRead %v, ReadRefused %v, requests %v", mayRead, refused, requests)
			}

			// Kept to its own runs: ReadRefused is asked once and decides,
			// whatever MayRead or ReadRepositories would grant.
			c, p = probed(t, true, true, func(cfg *github.Config) { cfg.ReadRepositories = []string{docs} })
			err = enter(t, c, name, slack)
			mayRead, refused, requests = p.snapshot()
			if !errors.As(err, &refusal) || *refusal != (github.ReadError{Owner: "acme", Repo: "docs", Kept: true}) || !errors.Is(err, connector.ErrUndeliverable) {
				t.Fatalf("kept: error %v", err)
			}
			if len(mayRead) != 0 || !slices.Equal(refused, []string{docs}) || len(requests) != 0 {
				t.Fatalf("kept: MayRead %v, ReadRefused %v, requests %v", mayRead, refused, requests)
			}

			// Named in ReadRepositories: MayRead is not needed.
			c, p = probed(t, false, false, func(cfg *github.Config) { cfg.ReadRepositories = []string{docs} })
			_ = enter(t, c, name, slack)
			mayRead, refused, requests = p.snapshot()
			if len(mayRead) != 0 || !slices.Equal(refused, []string{docs}) || len(requests) == 0 {
				t.Fatalf("listed: MayRead %v, ReadRefused %v, requests %v", mayRead, refused, requests)
			}

			// The repository's own run reads it without the hooks.
			c, p = probed(t, false, true, nil)
			_ = enter(t, c, name, own)
			mayRead, refused, requests = p.snapshot()
			if len(mayRead) != 0 || len(refused) != 0 || len(requests) == 0 {
				t.Fatalf("own run: MayRead %v, ReadRefused %v, requests %v", mayRead, refused, requests)
			}
		})
	}
}

// The hooks are asked about the repository the read then requests, however
// the reference is written.
func TestTheReadHooksAreAskedAboutWhatIsRead(t *testing.T) {
	for _, tc := range []struct {
		name, source, args string
		// asked is the owner/repo both hooks hear, "" when neither is asked.
		asked string
		// first is the first request, "" when nothing is requested.
		first string
	}{
		{"github address", "slack://T01/C01", `{"address":"github://acme/docs/issues/7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"pull request URL", "slack://T01/C01", `{"address":"https://github.com/acme/docs/pull/7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"comment URL", "slack://T01/C01", `{"address":"https://github.com/acme/docs/issues/7#issuecomment-1"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"short reference", "slack://T01/C01", `{"address":"acme/docs#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"repository URL with a number", "slack://T01/C01", `{"address":"https://github.com/acme/docs#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"surrounding space", "slack://T01/C01", `{"address":"  acme/docs#7 "}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"mixed case", "slack://T01/C01", `{"address":"Acme/Docs#7"}`, "Acme/Docs", "/repos/Acme/Docs/issues/7"},
		{"mixed case URL", "slack://T01/C01", `{"address":"https://github.com/ACME/docs/pull/7"}`, "ACME/docs", "/repos/ACME/docs/issues/7"},
		{"clone suffix, short", "slack://T01/C01", `{"address":"acme/docs.git#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"clone suffix, address", "slack://T01/C01", `{"address":"github://acme/docs.git/issues/7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"repository and number", "slack://T01/C01", `{"address":"acme/docs","number":7}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"reference and another number", "slack://T01/C01", `{"address":"acme/docs#3","number":7}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"another repository's run", "github://acme/web/pull/3", `{"address":"acme/docs#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},
		{"repository", "slack://T01/C01", `{"address":"acme/docs"}`, "acme/docs", "/repos/acme/docs"},
		{"repository URL", "slack://T01/C01", `{"address":"https://github.com/acme/docs.git"}`, "acme/docs", "/repos/acme/docs"},
		{"repository address", "slack://T01/C01", `{"address":"github://acme/docs"}`, "acme/docs", "/repos/acme/docs"},
		{"address key in another case", "slack://T01/C01", `{"Address":"acme/docs#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},

		{"a number in the run's own repository", "github://acme/web/pull/3", `{"number":7}`, "", "/repos/acme/web/issues/7"},
		{"the run's own repository by name", "github://acme/web/pull/3", `{"address":"ACME/Web#7"}`, "", "/repos/ACME/Web/issues/7"},
		{"a run bound to a repository, a number", "github://acme/web", `{"number":7}`, "", "/repos/acme/web/issues/7"},
		{"a run bound to a repository, its issue by name", "github://acme/web", `{"address":"acme/web#7"}`, "", "/repos/acme/web/issues/7"},
		{"a run bound to a repository, its name and a number", "github://acme/web", `{"address":"acme/web","number":7}`, "", "/repos/acme/web/issues/7"},
		{"a run bound to a repository, another repository", "github://acme/web", `{"address":"acme/docs#7"}`, "acme/docs", "/repos/acme/docs/issues/7"},

		{"a number with no repository", "slack://T01/C01", `{"address":"#7"}`, "", ""},
		{"a number with no repository, from a run", "github://acme/web/pull/3", `{"address":"#7"}`, "", ""},
		{"another host", "slack://T01/C01", `{"address":"https://evil.test/acme/docs/issues/7"}`, "", ""},
		{"a host that ends the same", "slack://T01/C01", `{"address":"https://notgithub.com/acme/docs/issues/7"}`, "", ""},
		{"a URL with a user", "slack://T01/C01", `{"address":"https://user@github.com/acme/docs/issues/7"}`, "", ""},
		{"a URL with a query", "slack://T01/C01", `{"address":"https://github.com/acme/docs/issues/7?next=1"}`, "", ""},
		{"plain http", "slack://T01/C01", `{"address":"http://github.com/acme/docs/issues/7"}`, "", ""},
		{"an owner not served", "slack://T01/C01", `{"address":"partner/docs#7"}`, "", ""},
		{"a clone suffix twice", "slack://T01/C01", `{"address":"acme/docs.git.git#7"}`, "", ""},
		{"a clone suffix twice, address", "slack://T01/C01", `{"address":"github://acme/docs.git.git/issues/7"}`, "", ""},
		{"a clone suffix twice, in another case", "slack://T01/C01", `{"address":"https://github.com/acme/docs.GIT.git"}`, "", ""},
		{"a clone suffix in another case", "slack://T01/C01", `{"address":"acme/docs.Git#7"}`, "", ""},
		{"a repository name past its length", "slack://T01/C01", `{"address":"acme/` + strings.Repeat("r", 101) + `#7"}`, "", ""},
		{"an owner past its length", "slack://T01/C01", `{"address":"` + strings.Repeat("a", 40) + `/docs#7"}`, "", ""},
		{"two address keys", "slack://T01/C01", `{"address":"acme/web#1","Address":"acme/docs#7"}`, "", ""},
		{"the address twice", "slack://T01/C01", `{"address":"acme/web#1","address":"acme/docs#7"}`, "", ""},
		{"an undeclared key", "slack://T01/C01", `{"address":"acme/web#1","repo":"acme/docs"}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := probed(t, true, false, nil)
			_, err := c.Actions()[0].Invoke(context.Background(), address.MustParse(tc.source), json.RawMessage(tc.args))
			mayRead, refused, requests := p.snapshot()
			var want []string
			if tc.asked != "" {
				want = []string{tc.asked}
			}
			if !slices.Equal(mayRead, want) || !slices.Equal(refused, want) || p.early {
				t.Fatalf("MayRead %v, ReadRefused %v, want each %v; asked after a request: %t", mayRead, refused, want, p.early)
			}
			if tc.first == "" {
				if err == nil || len(requests) != 0 {
					t.Fatalf("error %v, requests %v; want a refusal and no request", err, requests)
				}
				return
			}
			if len(requests) == 0 || requests[0] != "GET "+tc.first {
				t.Fatalf("requests %v, want first GET %s (error %v)", requests, tc.first, err)
			}
			repository := strings.Join(strings.Split(tc.first, "/")[:4], "/")
			for _, r := range requests {
				if got := strings.TrimPrefix(r, "GET "); got != repository && !strings.HasPrefix(got, repository+"/") {
					t.Fatalf("requested %s outside %s", r, repository)
				}
			}
		})
	}
}

// A Config without the hooks reads what it read before them: the run's own
// repository and the ReadRepositories, with arguments decoded as before.
func TestNilReadHooksGrantNothingMore(t *testing.T) {
	fake := github.NewFake()
	for _, repo := range []string{"docs", "web"} {
		if err := fake.SeedContext(github.Ref{Owner: "acme", Repo: repo, Number: 7}, github.Context{Subject: github.ContextSubject{Title: repo + " issue"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, source, args string
		grants             []string
		want               string
	}{
		{"its own repository", "github://acme/docs/issues/9", `{"address":"acme/docs#7"}`, nil, "docs issue"},
		{"a listed repository", "slack://T01/C01", `{"address":"acme/docs#7"}`, []string{"acme/docs"}, "docs issue"},
		{"no grant", "slack://T01/C01", `{"address":"acme/docs#7"}`, nil, ""},
		{"another repository's run", "github://acme/web/issues/7", `{"address":"acme/docs#7"}`, nil, ""},
		{"a grant for another repository", "slack://T01/C01", `{"address":"acme/docs#7"}`, []string{"acme/web"}, ""},
		{"a run bound to its repository, a number", "github://acme/web", `{"number":7}`, nil, "web issue"},
		{"a run bound to its repository, its issue by name", "github://acme/web", `{"address":"acme/web#7"}`, nil, "web issue"},
		{"a run bound to its repository, its name and a number", "github://acme/web", `{"address":"acme/web","number":7}`, nil, "web issue"},
		{"a run bound to a repository, another repository", "github://acme/web", `{"address":"acme/docs#7"}`, nil, ""},
		// The decoder keeps the last key that matches a field, as it did.
		{"two address keys, the last listed", "slack://T01/C01", `{"address":"acme/web#7","Address":"acme/docs#7"}`, []string{"acme/docs"}, "docs issue"},
		{"two address keys, the first listed", "slack://T01/C01", `{"address":"acme/web#7","Address":"acme/docs#7"}`, []string{"acme/web"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := github.New(github.Config{API: fake, ReadRepositories: tc.grants})
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.Actions()[0].Invoke(context.Background(), address.MustParse(tc.source), json.RawMessage(tc.args))
			if tc.want != "" {
				if err != nil || !strings.Contains(result.Summary, tc.want) {
					t.Fatalf("summary %q error %v, want %q", result.Summary, err, tc.want)
				}
				return
			}
			var refusal *github.ReadError
			if !errors.As(err, &refusal) || refusal.Kept || !errors.Is(err, connector.ErrUndeliverable) {
				t.Fatalf("summary %q error %v, want a ReadError", result.Summary, err)
			}
			if want := fmt.Sprintf("%s: repository %s/%s is not granted for cross-source reads", connector.ErrUndeliverable, refusal.Owner, refusal.Repo); err.Error() != want {
				t.Fatalf("error %q, want %q", err, want)
			}
		})
	}
}

// The ReadRepositories list is matched without regard to case, and the
// repository is requested as the reference wrote it.
func TestAListedRepositoryIsReadInAnyCaseWithoutTheHooks(t *testing.T) {
	c, p := probed(t, false, false, func(cfg *github.Config) {
		cfg.MayRead, cfg.ReadRefused, cfg.ReadRepositories = nil, nil, []string{"acme/docs"}
	})
	_, _ = c.Actions()[0].Invoke(context.Background(), address.MustParse("slack://T01/C01"), json.RawMessage(`{"address":"Acme/Docs#7"}`))
	if _, _, requests := p.snapshot(); !slices.Equal(requests, []string{"GET /repos/Acme/Docs/issues/7"}) {
		t.Fatalf("requests %v", requests)
	}
}

// A grant from MayRead never reaches a write, on any action.
func TestAReadHookGrantNeverAuthorizesAWrite(t *testing.T) {
	everything := func(context.Context, string, string) bool { return true }
	fake := github.NewFake()
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: everything, FixPullRequests: true, AuthorizeFixPull: func(context.Context, github.Ref) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for name, entry := range entryPoints {
		action, ok := strings.CutPrefix(name, "action ")
		if !ok || entry.kind != writes {
			continue
		}
		if err := enter(t, c, name, address.MustParse("slack://T01/C01")); err == nil {
			t.Errorf("%s wrote to a repository MayRead grants, from another surface", action)
		}
	}
	if len(fake.Comments()) != 0 {
		t.Fatalf("comments were posted: %+v", fake.Comments())
	}
}

// moved answers /repos/acme/old/... with a redirect to the same path under
// /repos/acme/kept, on the same host, as the API does for a repository that
// was renamed or transferred, and serves acme/kept.
func moved(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/old"); ok {
			target := "/repos/acme/kept" + rest
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, status)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/kept")
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := `[]`
		switch {
		case rest == "":
			body = `{"full_name":"acme/kept","description":"kept content","default_branch":"main","html_url":"https://github.com/acme/kept"}`
		case rest == "/git/ref/heads/main":
			body = `{"object":{"sha":"` + hookSHA + `"}}`
		case rest == "/issues/7":
			body = `{"number":7,"title":"kept content","body":"kept content","state":"open","html_url":"https://github.com/acme/kept/issues/7"}`
		case rest == "/issues/7/comments":
			body = `[{"id":1,"body":"kept content","user":{"login":"ana"}}]`
		case strings.HasPrefix(rest, "/contents/"):
			body = `{"type":"file","encoding":"base64","content":"a2VwdCBjb250ZW50","sha":"x","html_url":"https://github.com/acme/kept/blob/x/README.md","size":12}`
		case strings.HasPrefix(rest, "/actions/runs"):
			body = `{"total_count":1,"workflow_runs":[{"id":1,"name":"kept content","head_sha":"` + hookSHA + `","run_attempt":1,"status":"completed","conclusion":"success"}]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A read of a repository other than the run's own is answered by the
// repository the grants were decided for, or it fails: a redirect to
// another location is not followed, with the hooks or with the
// ReadRepositories list alone.
func TestACrossSourceReadIsAnsweredWhereItWasNamed(t *testing.T) {
	reads := map[string]string{
		"github_read_context":             `{"address":"acme/old#7"}`,
		"github_read_context, repository": `{"address":"acme/old"}`,
		"github_read_context, numbered":   `{"address":"acme/old","number":7}`,
		"github_read_file":                `{"address":"acme/old#7","path":"README.md","ref":"` + hookSHA + `"}`,
		"github_workflow_status":          `{"address":"acme/old#7","ref":"` + hookSHA + `"}`,
		"github_fix_base":                 `{"address":"acme/old#7"}`,
		"ReadComments":                    "",
	}
	configs := map[string]func(*github.Config){
		"hooks": func(cfg *github.Config) {
			cfg.MayRead = func(context.Context, string, string) bool { return true }
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				return strings.EqualFold(owner+"/"+repo, "acme/kept")
			}
		},
		"list": func(cfg *github.Config) { cfg.ReadRepositories = []string{"acme/old"} },
		"target granted": func(cfg *github.Config) {
			cfg.MayRead = func(context.Context, string, string) bool { return true }
		},
	}
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for grants, edit := range configs {
			srv := moved(t, status)
			cfg := github.Config{
				API: github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()), Owners: []string{"acme"}, FixPullRequests: true,
				AuthorizeFixPull: func(context.Context, github.Ref) error { return nil },
			}
			edit(&cfg)
			c, err := github.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			invoke := func(name, args string, source address.Address) (string, error) {
				if name == "ReadComments" {
					comments, err := c.ReadComments(context.Background(), source, "acme/old#7", 5, nil)
					data, _ := json.Marshal(comments)
					return string(data), err
				}
				name, _, _ = strings.Cut(name, ",")
				for _, a := range c.Actions() {
					if a.Name == name {
						r, err := a.Invoke(context.Background(), source, json.RawMessage(args))
						return r.Summary + string(r.Data), err
					}
				}
				t.Fatalf("no action %s", name)
				return "", nil
			}
			for name, args := range reads {
				got, err := invoke(name, args, address.MustParse("slack://T01/C01"))
				if !errors.Is(err, github.ErrReadRedirected) || strings.Contains(got, "kept content") || strings.Contains(got, hookSHA) {
					t.Errorf("%d, %s, %s: result %q, error %v; want ErrReadRedirected", status, grants, name, got, err)
				}
				if err != nil && strings.Contains(err.Error(), "kept") {
					t.Errorf("%d, %s, %s: the error names where the repository answered from: %v", status, grants, name, err)
				}
			}
			// A run on the repository itself follows a redirect to another
			// repository only when that repository is granted as a
			// cross-source read; the comments read never follows one.
			own := address.MustParse("github://acme/old/issues/7")
			got, err := invoke("github_read_context", `{}`, own)
			if grants == "target granted" {
				if err != nil || !strings.Contains(got, "kept content") {
					t.Errorf("%d, %s: the run's own read: %q, %v", status, grants, got, err)
				}
			} else if !errors.Is(err, github.ErrReadRedirected) || strings.Contains(got, "kept content") {
				t.Errorf("%d, %s: the run's own read: %q, %v; want ErrReadRedirected", status, grants, got, err)
			}
			if _, err := invoke("ReadComments", "", own); !errors.Is(err, github.ErrReadRedirected) {
				t.Errorf("%d, %s: the run's own comments read: %v", status, grants, err)
			}
		}
	}
}

// A context marked for a read where it was named stops a redirect on any
// API call; an unmarked one keeps the client's own policy.
func TestReadWhereNamedMarksAContext(t *testing.T) {
	ctx := context.Background()
	if github.IsReadWhereNamed(ctx) || !github.IsReadWhereNamed(github.ReadWhereNamed(ctx)) {
		t.Fatal("the mark is not read back")
	}
	srv := moved(t, http.StatusMovedPermanently)
	api := github.NewHTTPAPIAt(srv.URL, "tok", srv.Client())
	old := github.Ref{Owner: "acme", Repo: "old", Number: 7}
	if got, err := api.ReadContext(ctx, old, 1); err != nil || got.Subject.Title != "kept content" {
		t.Fatalf("unmarked: %+v, %v", got.Subject, err)
	}
	if _, err := api.ReadContext(github.ReadWhereNamed(ctx), old, 1); !errors.Is(err, github.ErrReadRedirected) {
		t.Fatalf("marked: %v", err)
	}
	stop := srv.Client()
	stop.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("no redirects") }
	if _, err := github.NewHTTPAPIAt(srv.URL, "tok", stop).ReadContext(ctx, old, 1); err == nil || !strings.Contains(err.Error(), "no redirects") {
		t.Fatalf("the client's own policy: %v", err)
	}
}

// transferredIssue answers /repos/acme/web/issues/7 with a redirect to
// location plus the rest of the path, as the API does for an issue moved to
// another repository, and serves acme/other's and acme/web's issue 7.
func transferredIssue(t *testing.T, location string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/web/issues/7"); ok {
			target := location + rest
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.ToLower(r.URL.Path) {
		case "/repos/acme/other/issues/7", "/repos/acme/web/moved/issues/7":
			_, _ = w.Write([]byte(`{"number":7,"title":"moved content","body":"moved content","state":"open"}`))
		case "/repos/acme/other/issues/7/comments", "/repos/acme/web/moved/issues/7/comments":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

// A read of the run's own repository that GitHub redirects to another
// repository asks the cross-source rules about that repository before it
// requests it, and follows the redirect only when they grant it.
func TestAnOwnReadRedirectedToAnotherRepositoryAsksTheReadHooks(t *testing.T) {
	own := address.MustParse("github://acme/web/pull/3")
	reads := []string{`{"number":7}`, `{"address":"acme/web#7"}`, `{"address":"acme/web","number":7}`}
	for _, tc := range []struct {
		name     string
		location string
		cfg      func(*github.Config, *[]string)
		// asked is what ReadRefused hears; follows says the read returns
		// the redirect's target.
		asked   []string
		follows bool
	}{
		{"kept", "/repos/acme/other/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.MayRead = func(context.Context, string, string) bool { return true }
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return owner+"/"+repo == "acme/other"
			}
		}, []string{"acme/other"}, false},
		{"not granted", "/repos/acme/other/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return false
			}
		}, []string{"acme/other"}, false},
		{"granted by MayRead", "/repos/acme/other/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.MayRead = func(_ context.Context, owner, repo string) bool { return owner+"/"+repo == "acme/other" }
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return false
			}
		}, []string{"acme/other"}, true},
		{"granted by ReadRepositories", "/repos/acme/other/issues/7", func(cfg *github.Config, _ *[]string) {
			cfg.ReadRepositories = []string{"acme/other"}
		}, nil, true},
		{"an owner not served", "/repos/partner/other/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.MayRead = func(context.Context, string, string) bool { return true }
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return false
			}
		}, nil, false},
		{"a location that names no repository", "/repositories/42/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.MayRead = func(context.Context, string, string) bool { return true }
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return false
			}
		}, nil, false},
		{"within the same repository", "/repos/ACME/Web/moved/issues/7", func(cfg *github.Config, asked *[]string) {
			cfg.ReadRefused = func(_ context.Context, owner, repo string) bool {
				*asked = append(*asked, owner+"/"+repo)
				return true
			}
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, args := range reads {
				srv, requests := transferredIssue(t, tc.location)
				var asked []string
				cfg := github.Config{API: github.NewHTTPAPIAt(srv.URL, "tok", srv.Client()), Owners: []string{"acme"}}
				tc.cfg(&cfg, &asked)
				c, err := github.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				r, err := c.Actions()[0].Invoke(context.Background(), own, json.RawMessage(args))
				if !slices.Equal(asked, tc.asked) {
					t.Errorf("%s: ReadRefused heard %v, want %v", args, asked, tc.asked)
				}
				if tc.follows {
					if err != nil || !strings.Contains(r.Summary, "moved content") {
						t.Errorf("%s: %q, %v; want the redirect followed", args, r.Summary, err)
					}
					continue
				}
				if !errors.Is(err, github.ErrReadRedirected) || strings.Contains(r.Summary, "moved content") {
					t.Errorf("%s: %q, %v; want ErrReadRedirected", args, r.Summary, err)
				}
				if got := requests(); len(got) != 1 {
					t.Errorf("%s: requests %v; want only the run's own", args, got)
				}
			}
		})
	}
}

// FollowReadRedirect follows every redirect for an unmarked context and
// none for one marked ReadWhereNamed.
func TestFollowReadRedirectWithoutTheConnectorsMark(t *testing.T) {
	ctx := context.Background()
	if err := github.FollowReadRedirect(ctx, "acme", "other"); err != nil {
		t.Errorf("unmarked: %v", err)
	}
	if err := github.FollowReadRedirect(github.ReadWhereNamed(ctx), "acme", "old"); !errors.Is(err, github.ErrReadRedirected) {
		t.Errorf("marked: %v", err)
	}
}
