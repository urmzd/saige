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

// The two kinds of check GitHub reports on a commit.
const (
	CheckKindRun    = "check_run"
	CheckKindStatus = "status"
)

// MaxCommitChecks bounds one CommitChecks read for each kind. A commit with
// that many or more fails the read rather than returning a part of its checks.
const MaxCommitChecks = 1000

// CommitCheck is one check run or one commit status on a commit.
//
// A check run keeps GitHub's status (queued, in_progress, completed and the
// rest) and its conclusion, and names the app that published it (App, its
// slug) and its ExternalID. A commit status is the latest one for its context:
// pending is Status "pending" with no conclusion, and success, failure and
// error are Status "completed" with that state as the conclusion. A status has
// no App or ExternalID.
type CommitCheck struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	App        string `json:"app,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
}

// ownCheckPrefix starts the name of the check the connector publishes for a run.
const ownCheckPrefix = "fiber/"

// AllPassed reports whether a commit's checks allow an approval: at least one
// check that own does not skip, every such check Passed, and fewer than
// MaxCommitChecks in the list. An empty list, or one where own skips every
// check, has not passed: checks that have not registered yet are not a pass.
// Pass ApprovalRequest.OwnCheck as own to skip only the check of the run being
// reviewed; nil skips nothing.
func AllPassed(checks []CommitCheck, own func(CommitCheck) bool) bool {
	if len(checks) >= MaxCommitChecks {
		return false
	}
	counted := 0
	for _, check := range checks {
		if own != nil && own(check) {
			continue
		}
		if !check.Passed() {
			return false
		}
		counted++
	}
	return counted > 0
}

// Passed reports whether the check completed as success, neutral or skipped,
// the conclusions GitHub's required status checks accept. Anything pending,
// failed, cancelled, timed out or waiting for action has not passed.
func (c CommitCheck) Passed() bool {
	if c.Status != checkCompleted {
		return false
	}
	switch c.Conclusion {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

const (
	commitChecksPage = 100
	checkCompleted   = "completed"
	checkPending     = "pending"
)

// CommitChecks reads the latest check run of every check and the latest commit
// status of every context on sha. It follows pages until one comes back short,
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
			out = append(out, CommitCheck{Kind: CheckKindRun, Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, URL: run.URL, App: run.App.Slug, ExternalID: run.ExternalID})
		}
		runs += len(list.Checks)
		if len(list.Checks) < commitChecksPage {
			break
		}
		if runs >= MaxCommitChecks {
			return nil, fmt.Errorf("github: commit has %d or more check runs", MaxCommitChecks)
		}
	}
	statuses := 0
	for page := 1; ; page++ {
		var combined struct {
			SHA      string `json:"sha"`
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
				URL     string `json:"target_url"`
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
			out = append(out, statusCheck(status.Context, status.State, status.URL))
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
