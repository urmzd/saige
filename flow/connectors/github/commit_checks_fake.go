package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

func commitChecksKey(owner, repo, sha string) string {
	return strings.ToLower(owner + "/" + repo + "@" + sha)
}

// SeedCommitCheck reports a check run or commit status on a commit. Every seed
// is kept, two of the same name included, as the HTTP API returns them. A
// status keeps no App, AppID or ExternalID, as GitHub reports none, and a
// check run keeps no Creator. A negative AppID is read as 0.
func (f *Fake) SeedCommitCheck(owner, repo, sha string, check CommitCheck) error {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return err
	}
	if check.Kind != CheckKindRun && check.Kind != CheckKindStatus || check.Name == "" || check.Status == "" {
		return fmt.Errorf("github: invalid commit check seed")
	}
	if check.Kind == CheckKindStatus {
		check.App, check.AppID, check.ExternalID = "", 0, ""
	} else {
		check.Creator = ""
	}
	check.AppID = knownID(check.AppID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitChecks == nil {
		f.commitChecks = make(map[string][]CommitCheck)
	}
	key := commitChecksKey(owner, repo, sha)
	f.commitChecks[key] = append(f.commitChecks[key], check)
	return nil
}

// FakeApp and FakeAppID are the app slug and ID of the checks the fake makes
// with EnsureCheck: the app whose login the fake reports for its own reviews,
// "humagen[bot]".
const (
	FakeApp         = "humagen"
	FakeAppID int64 = 1001
)

// CommitChecks returns the checks on sha: the connector's own checks made with
// EnsureCheck (App FakeApp, AppID FakeAppID, with their external ID) and the seeded check runs,
// then the seeded statuses, as the HTTP API reads them.
func (f *Fake) CommitChecks(_ context.Context, owner, repo, sha string) ([]CommitCheck, error) {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var runs []CommitCheck
	ids := make([]int64, 0, len(f.checks))
	for id := range f.checks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := f.checks[id]
		if strings.EqualFold(c.Ref.Owner, owner) && strings.EqualFold(c.Ref.Repo, repo) && strings.EqualFold(c.Check.HeadSHA, sha) {
			runs = append(runs, CommitCheck{Kind: CheckKindRun, Name: c.Check.Name, Status: c.Check.Status, Conclusion: c.Check.Conclusion, App: FakeApp, AppID: FakeAppID, ExternalID: c.Check.ExternalID})
		}
	}
	var statuses []CommitCheck
	for _, check := range f.commitChecks[commitChecksKey(owner, repo, sha)] {
		if check.Kind == CheckKindRun {
			runs = append(runs, check)
		} else {
			statuses = append(statuses, check)
		}
	}
	if len(runs) >= MaxCommitChecks || len(statuses) >= MaxCommitChecks {
		return nil, fmt.Errorf("github: commit has %d or more checks of one kind", MaxCommitChecks)
	}
	return append(runs, statuses...), nil
}

var _ CommitChecksAPI = (*Fake)(nil)
