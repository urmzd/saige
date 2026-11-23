package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/github"
)

// installedOnly grants what an installation covering acme/docs would.
func installedOnly(_ context.Context, owner, repo string) bool {
	return strings.EqualFold(owner+"/"+repo, "acme/docs")
}

func action(t *testing.T, c *github.Connector, name string) func(source, args string) (string, error) {
	t.Helper()
	for _, a := range c.Actions() {
		if a.Name == name {
			return func(source, args string) (string, error) {
				r, err := a.Invoke(context.Background(), address.MustParse(source), json.RawMessage(args))
				return r.Summary, err
			}
		}
	}
	t.Fatalf("no action %s", name)
	return nil
}

func TestCrossSourceReadsReachARepositoryTheInstallationCovers(t *testing.T) {
	fake := github.NewFake()
	for _, ref := range []github.Ref{{Owner: "acme", Repo: "docs", Number: 7}, {Owner: "acme", Repo: "secret", Number: 7}} {
		var evidence github.Context
		evidence.Subject.Title = ref.Repo + " issue"
		evidence.Subject.Head.SHA = sourceSHAForGrant
		if err := fake.SeedContext(ref, evidence); err != nil {
			t.Fatal(err)
		}
		if err := fake.SeedFiles(ref, map[string]string{"README.md": "hello\n"}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: installedOnly})
	if err != nil {
		t.Fatal(err)
	}
	read := action(t, c, "github_read_context")
	for _, tc := range []struct {
		name, source, reference string
		want                    bool
	}{
		{"Slack, an installed repository", "slack://T01/C01", "https://github.com/acme/docs/issues/7", true},
		{"Slack, a github:// address", "slack://T01/C01", "github://acme/docs/issues/7", true},
		{"another repository's run", "github://acme/web/pull/3", "github://acme/docs/issues/7", true},
		{"Slack, a repository it does not cover", "slack://T01/C01", "github://acme/secret/issues/7", false},
		{"a lookalike", "slack://T01/C01", "github://acme/docs-other/issues/7", false},
		{"another host", "slack://T01/C01", "https://evil.test/acme/docs/issues/7", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := read(tc.source, `{"address":"`+tc.reference+`"}`)
			if (err == nil) != tc.want {
				t.Fatalf("summary %q error %v", summary, err)
			}
			if tc.want && !strings.Contains(summary, "docs issue") {
				t.Fatalf("read the wrong subject: %s", summary)
			}
		})
	}
	file := action(t, c, "github_read_file")
	if summary, err := file("slack://T01/C01", `{"address":"github://acme/docs/issues/7","path":"README.md","ref":"`+sourceSHAForGrant+`"}`); err != nil || !strings.Contains(summary, "hello") {
		t.Fatalf("file of an installed repository: %q, %v", summary, err)
	}
	if _, err := file("slack://T01/C01", `{"address":"github://acme/secret/issues/7","path":"README.md","ref":"`+sourceSHAForGrant+`"}`); err == nil {
		t.Fatal("read a file of a repository the installation does not cover")
	}
}

func TestAnInstallationGrantNeverAuthorizesWrites(t *testing.T) {
	c, err := github.New(github.Config{API: github.NewFake(), Owners: []string{"acme"}, MayRead: installedOnly})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"github_comment", "github_add_labels", "github_request_review"} {
		if _, err := action(t, c, name)("slack://T01/C01", `{"body":"write","labels":["x"],"reviewers":["a"],"address":"github://acme/docs/issues/7"}`); err == nil {
			t.Fatalf("%s wrote to an installed repository from another surface", name)
		}
	}
}

// The owners a connector serves are checked before any grant, so a
// repository of another owner stays refused to the read tools whatever
// MayRead answers.
func TestAnInstalledRepositoryOutsideTheOwnersIsRefusedToTheReadTools(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "partner", Repo: "tools", Number: 7}
	if err := fake.SeedContext(ref, github.Context{Subject: github.ContextSubject{Title: "tools issue"}}); err != nil {
		t.Fatal(err)
	}
	everything := func(context.Context, string, string) bool { return true }
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: everything})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := action(t, c, "github_read_context")("slack://T01/C01", `{"address":"github://partner/tools/issues/7"}`)
	if err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("summary %q error %v, want the owners refusal", summary, err)
	}
}

const sourceSHAForGrant = "316c56f81540c3eaca02184173df322a48aebdd9"

// A repository kept to its own runs is read by no other: not from Slack,
// not by another repository's run, and not through a grant by name, while a
// run on its own issue still reads it.
func TestARepositoryKeptToItsOwnRunsRefusesCrossSourceReads(t *testing.T) {
	fake := github.NewFake()
	ref := github.Ref{Owner: "acme", Repo: "docs", Number: 7}
	var evidence github.Context
	evidence.Subject.Title = "docs issue"
	evidence.Subject.Head.SHA = sourceSHAForGrant
	if err := fake.SeedContext(ref, evidence); err != nil {
		t.Fatal(err)
	}
	if err := fake.SeedFiles(ref, map[string]string{"README.md": "hello\n"}); err != nil {
		t.Fatal(err)
	}
	refused := func(_ context.Context, owner, repo string) bool {
		return strings.EqualFold(owner+"/"+repo, "acme/docs")
	}
	for _, cfg := range []github.Config{
		{API: fake, Owners: []string{"acme"}, MayRead: installedOnly, ReadRefused: refused},
		{API: fake, Owners: []string{"acme"}, ReadRepositories: []string{"acme/docs"}, ReadRefused: refused},
	} {
		c, err := github.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		read := action(t, c, "github_read_context")
		for _, source := range []string{"slack://T01/C01", "github://acme/web/pull/3"} {
			if summary, err := read(source, `{"address":"github://acme/docs/issues/7"}`); err == nil || !strings.Contains(err.Error(), "not readable from other surfaces") {
				t.Fatalf("%s read a repository kept to its own runs: %q, %v", source, summary, err)
			}
		}
		if _, err := action(t, c, "github_read_file")("slack://T01/C01", `{"address":"github://acme/docs/issues/7","path":"README.md","ref":"`+sourceSHAForGrant+`"}`); err == nil {
			t.Fatal("read a file of a repository kept to its own runs")
		}
		if summary, err := read("github://acme/docs/issues/7", `{"address":"github://acme/docs/issues/7"}`); err != nil || !strings.Contains(summary, "docs issue") {
			t.Fatalf("its own run could not read it: %q, %v", summary, err)
		}
	}
}

// A repository named as owner/repo, github://owner/repo or its github.com
// URL reads under the same grants as its issues: the installation, the
// repository's own runs and its "readable from other surfaces" setting.
func TestARepositoryReadsUnderTheSameGrantsAsItsIssues(t *testing.T) {
	fake := github.NewFake()
	for _, repo := range []string{"docs", "secret"} {
		items := make([]github.RepositoryItem, 0, 14)
		for i := 1; i <= 14; i++ {
			items = append(items, github.RepositoryItem{Number: i, Title: repo + " item"})
		}
		if err := fake.SeedRepository("acme", repo, github.RepositoryContext{Description: repo + " repository", DefaultBranch: "main", PullRequests: items, Issues: items, README: strings.Repeat("line\n", 2000)}); err != nil {
			t.Fatal(err)
		}
	}
	var evidence github.Context
	evidence.Subject.Title = "docs issue"
	if err := fake.SeedContext(github.Ref{Owner: "acme", Repo: "docs", Number: 7}, evidence); err != nil {
		t.Fatal(err)
	}
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: installedOnly})
	if err != nil {
		t.Fatal(err)
	}
	read := action(t, c, "github_read_context")
	for _, tc := range []struct {
		name, source, reference string
		want                    bool
	}{
		{"owner/repo from Slack", "slack://T01/C01", "acme/docs", true},
		{"github:// repository", "slack://T01/C01", "github://acme/docs", true},
		{"github.com repository URL", "slack://T01/C01", "https://github.com/acme/docs", true},
		{"another repository's run", "github://acme/web/pull/3", "acme/docs", true},
		{"its own run", "github://acme/secret/pull/3", "acme/secret", true},
		{"not covered", "slack://T01/C01", "acme/secret", false},
		{"lookalike", "slack://T01/C01", "acme/docs-other", false},
		{"owner not served", "slack://T01/C01", "partner/docs", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := read(tc.source, `{"address":"`+tc.reference+`"}`)
			if (err == nil) != tc.want {
				t.Fatalf("summary %q error %v", summary, err)
			}
			if !tc.want {
				return
			}
			var got github.RepositoryContext
			if err := json.Unmarshal([]byte(summary), &got); err != nil {
				t.Fatal(err)
			}
			if got.DefaultBranch != "main" || len(got.PullRequests) > 10 || len(got.Issues) > 10 || len(got.README) > 4000 || !got.READMETruncated {
				t.Fatalf("unbounded or wrong repository read: %d PRs, %d issues, %d README bytes", len(got.PullRequests), len(got.Issues), len(got.README))
			}
		})
	}
	if summary, err := read("slack://T01/C01", `{"address":"acme/docs","number":7}`); err != nil || !strings.Contains(summary, "docs issue") {
		t.Fatalf("an issue by repository and number: %q, %v", summary, err)
	}
	if _, err := read("slack://T01/C01", `{"address":"acme/secret","number":7}`); err == nil {
		t.Fatal("read an issue of a repository the installation does not cover")
	}
	// github_read_file needs an issue or pull request; a repository is refused with the form to use.
	if _, err := action(t, c, "github_read_file")("slack://T01/C01", `{"address":"acme/docs","path":"README.md","ref":"`+sourceSHAForGrant+`"}`); err == nil || !strings.Contains(err.Error(), "/pull/12") {
		t.Fatalf("github_read_file took a repository: %v", err)
	}

	refused := func(_ context.Context, owner, repo string) bool {
		return strings.EqualFold(owner+"/"+repo, "acme/docs")
	}
	kept, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: installedOnly, ReadRefused: refused})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"acme/docs", "github://acme/docs", "https://github.com/acme/docs"} {
		if summary, err := action(t, kept, "github_read_context")("slack://T01/C01", `{"address":"`+ref+`"}`); err == nil || !strings.Contains(err.Error(), "not readable from other surfaces") {
			t.Fatalf("%s read a repository kept to its own runs: %q, %v", ref, summary, err)
		}
	}
	if _, err := action(t, kept, "github_read_context")("github://acme/docs/issues/7", `{"address":"acme/docs"}`); err != nil {
		t.Fatalf("its own run could not read it: %v", err)
	}
}

// The decoder matches argument keys without regard to case and keeps the
// last of several, so with a hook set a key in another case is the address,
// and two keys for it, or a key the action does not declare, are refused
// before anything is read.
func TestTheGrantsReadTheAddressTheWayTheActionDecodesIt(t *testing.T) {
	fake := github.NewFake()
	for _, repo := range []string{"docs", "web"} {
		if err := fake.SeedRepository("acme", repo, github.RepositoryContext{Description: repo + " repository", DefaultBranch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(_ context.Context, owner, repo string) bool {
		return strings.EqualFold(owner+"/"+repo, "acme/docs")
	}
	everything := func(context.Context, string, string) bool { return true }
	c, err := github.New(github.Config{API: fake, Owners: []string{"acme"}, MayRead: everything, ReadRefused: refused})
	if err != nil {
		t.Fatal(err)
	}
	read := action(t, c, "github_read_context")
	for _, tc := range []struct{ name, args, want string }{
		{"another case", `{"Address":"acme/docs"}`, "not readable from other surfaces"},
		{"two cases", `{"address":"acme/web","Address":"acme/docs"}`, "more than once"},
		{"twice", `{"address":"acme/web","address":"acme/docs"}`, "more than once"},
		{"undeclared", `{"address":"acme/web","repo":"acme/docs"}`, "takes no argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := read("slack://T01/C01", tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("summary %q error %v, want %q", summary, err, tc.want)
			}
		})
	}
	// A key in another case still reads a repository the grants allow.
	if summary, err := read("slack://T01/C01", `{"ADDRESS":"acme/web"}`); err != nil || !strings.Contains(summary, "web repository") {
		t.Fatalf("an allowed read in another case: %q, %v", summary, err)
	}
}
