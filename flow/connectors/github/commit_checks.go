package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/urmzd/saige/flow/address"
)

// CommitChecksAPI reads every check reported on one commit, so a host can
// require them all to pass before it approves. The model has no tool for it.
type CommitChecksAPI interface {
	CommitChecks(ctx context.Context, owner, repo, sha string) ([]CommitCheck, error)
}

// The kinds of check GitHub reports on a commit. A check suite is an app's
// set of check runs on the commit; it is read so that a suite queued before
// any of its runs registered still counts as pending.
const (
	CheckKindRun    = "check_run"
	CheckKindStatus = "status"
	CheckKindSuite  = "check_suite"
)

// MaxCommitChecks bounds one CommitChecks read for each kind (check runs,
// check suites, commit statuses). A commit with that many or more of a kind
// fails the read rather than returning a part of its checks.
const MaxCommitChecks = 1000

// CommitCheck is one check run or one commit status on a commit.
//
// A check run keeps GitHub's status (queued, in_progress, completed and the
// rest) and its conclusion, and names the app that published it: AppID, its
// numeric ID, and App, its slug. It also keeps its ExternalID. A commit status
// is the latest one for its context: pending is Status "pending" with no
// conclusion, and success, failure and error are Status "completed" with that
// state as the conclusion. A status has no App, AppID or ExternalID; Creator is
// the login that set it, "<slug>[bot]" when an app did. A check suite keeps
// GitHub's status and conclusion and its app; its Name is the app's slug, and
// Runs is how many check runs it has.
type CommitCheck struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	App        string `json:"app,omitempty"`
	// AppID is the numeric ID of the app that published a check run. It is 0
	// when GitHub's response carries none.
	AppID      int64  `json:"app_id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	// Creator is the login that set a commit status. Empty for a check run.
	Creator string `json:"creator,omitempty"`
	// Runs is the number of check runs of a check suite.
	Runs int `json:"runs,omitempty"`
}

// ownCheckPrefix starts the name of the check the connector publishes for a run.
const ownCheckPrefix = "fiber/"

// AllPassed reports whether a commit's checks allow an approval. Checks own
// marks are left out. Of the rest, at least one Passed, none Blocks, and the
// list is shorter than MaxCommitChecks. A check run that concluded neutral or
// skipped neither passes nor blocks, and a check suite never passes. An empty list, or one where own marks
// every check, has not passed: checks that have not registered yet are not a
// pass. Pass ApprovalRequest.OwnCheck as own to leave out every check of the
// connector's own app; nil leaves out nothing. ApprovalRequest.ChecksAllow is
// the one call a host needs: it also requires that the request names the app.
func AllPassed(checks []CommitCheck, own func(CommitCheck) bool) bool {
	if len(checks) >= MaxCommitChecks {
		return false
	}
	passed := 0
	for _, check := range checks {
		if own != nil && own(check) {
			continue
		}
		if check.Blocks() {
			return false
		}
		if check.Passed() {
			passed++
		}
	}
	return passed > 0
}

// Passed reports whether the check completed with the conclusion success. A
// check suite never passes on its own: its runs do.
func (c CommitCheck) Passed() bool {
	return c.Kind != CheckKindSuite && c.Status == checkCompleted && c.Conclusion == conclusionSuccess
}

// Blocks reports whether the check stands in the way of an approval: it has
// not completed, or it completed with any conclusion but success, except that
// a check run or check suite that concluded neutral or skipped does not
// block. A commit status has no neutral state, so a status blocks unless it
// passed. A check suite that has not completed blocks, with or without runs.
func (c CommitCheck) Blocks() bool {
	if c.Status != checkCompleted {
		return true
	}
	switch c.Conclusion {
	case conclusionSuccess:
		return false
	case "neutral", "skipped":
		return c.Kind == CheckKindStatus
	}
	return true
}

const (
	commitChecksPage  = 100
	checkCompleted    = "completed"
	conclusionSuccess = "success"
	checkPending      = "pending"
)

// CommitChecks reads the latest check run of every check, every check suite,
// and the latest commit status of every context on sha, in that order. It follows pages until one comes back short,
// up to MaxCommitChecks of each kind, and never trusts a total count to stop
// early. A response for another commit fails the read.
func (a *HTTPAPI) CommitChecks(ctx context.Context, owner, repo, sha string) ([]CommitCheck, error) {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return nil, err
	}
	base := fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repo, sha)
	var out []CommitCheck
	runs := 0
	for page := 1; ; page++ {
		var list struct {
			Checks []struct {
				Name       string `json:"name"`
				ExternalID string `json:"external_id"`
				HeadSHA    string `json:"head_sha"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				URL        string `json:"html_url"`
				App        struct {
					ID   int64  `json:"id"`
					Slug string `json:"slug"`
				} `json:"app"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf("%s/check-runs?filter=latest&per_page=%d&page=%d", base, commitChecksPage, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, fmt.Errorf("github: list check runs: %w", err)
		}
		for _, run := range list.Checks {
			if run.HeadSHA != sha {
				return nil, fmt.Errorf("github: check run response did not match requested commit")
			}
			out = append(out, CommitCheck{Kind: CheckKindRun, Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, URL: run.URL, App: run.App.Slug, AppID: knownID(run.App.ID), ExternalID: run.ExternalID})
		}
		runs += len(list.Checks)
		if len(list.Checks) < commitChecksPage {
			break
		}
		if runs >= MaxCommitChecks {
			return nil, fmt.Errorf("github: commit has %d or more check runs", MaxCommitChecks)
		}
	}
	suites, err := a.checkSuites(ctx, base, sha)
	if err != nil {
		return nil, err
	}
	out = append(out, suites...)
	statuses := 0
	for page := 1; ; page++ {
		var combined struct {
			SHA      string `json:"sha"`
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
				URL     string `json:"target_url"`
				Creator struct {
					Login string `json:"login"`
				} `json:"creator"`
			} `json:"statuses"`
		}
		path := fmt.Sprintf("%s/status?per_page=%d&page=%d", base, commitChecksPage, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &combined); err != nil {
			return nil, fmt.Errorf("github: read commit status: %w", err)
		}
		if combined.SHA != sha {
			return nil, fmt.Errorf("github: commit status response did not match requested commit")
		}
		for _, status := range combined.Statuses {
			check := statusCheck(status.Context, status.State, status.URL)
			check.Creator = status.Creator.Login
			out = append(out, check)
		}
		statuses += len(combined.Statuses)
		if len(combined.Statuses) < commitChecksPage {
			break
		}
		if statuses >= MaxCommitChecks {
			return nil, fmt.Errorf("github: commit has %d or more statuses", MaxCommitChecks)
		}
	}
	return out, nil
}

// checkSuites reads every check suite on sha, up to MaxCommitChecks.
func (a *HTTPAPI) checkSuites(ctx context.Context, base, sha string) ([]CommitCheck, error) {
	var out []CommitCheck
	for page := 1; ; page++ {
		var list struct {
			Suites []struct {
				HeadSHA    string `json:"head_sha"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Runs       int    `json:"latest_check_runs_count"`
				App        struct {
					ID   int64  `json:"id"`
					Slug string `json:"slug"`
				} `json:"app"`
			} `json:"check_suites"`
		}
		path := fmt.Sprintf("%s/check-suites?per_page=%d&page=%d", base, commitChecksPage, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, fmt.Errorf("github: list check suites: %w", err)
		}
		for _, suite := range list.Suites {
			if suite.HeadSHA != sha {
				return nil, fmt.Errorf("github: check suite response did not match requested commit")
			}
			out = append(out, CommitCheck{Kind: CheckKindSuite, Name: suite.App.Slug, Status: suite.Status, Conclusion: suite.Conclusion, App: suite.App.Slug, AppID: knownID(suite.App.ID), Runs: max(suite.Runs, 0)})
		}
		if len(list.Suites) < commitChecksPage {
			return out, nil
		}
		if len(out) >= MaxCommitChecks {
			return nil, fmt.Errorf("github: commit has %d or more check suites", MaxCommitChecks)
		}
	}
}

func statusCheck(name, state, url string) CommitCheck {
	check := CommitCheck{Kind: CheckKindStatus, Name: name, Status: checkCompleted, Conclusion: state, URL: url}
	if state == checkPending {
		check.Status, check.Conclusion = checkPending, ""
	}
	return check
}

func validCommitRef(owner, repo, sha string) error {
	if address.ValidWorkspace(owner) != nil || !validRepository(repo) {
		return fmt.Errorf("github: checks need a valid repository")
	}
	if !validSHA(sha) {
		return fmt.Errorf("github: checks require a full commit SHA")
	}
	return nil
}

var _ CommitChecksAPI = (*HTTPAPI)(nil)
