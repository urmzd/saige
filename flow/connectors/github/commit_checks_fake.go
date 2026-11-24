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
// is kept, two of the same name included, as the HTTP API returns them. Own is
// set from the name, as the HTTP API sets it.
func (f *Fake) SeedCommitCheck(owner, repo, sha string, check CommitCheck) error {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return err
	}
	if check.Kind != CheckKindRun && check.Kind != CheckKindStatus || check.Name == "" || check.Status == "" {
		return fmt.Errorf("github: invalid commit check seed")
	}
	check.Own = ownCheck(check.Kind, check.Name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitChecks == nil {
		f.commitChecks = make(map[string][]CommitCheck)
	}
	key := commitChecksKey(owner, repo, sha)
	f.commitChecks[key] = append(f.commitChecks[key], check)
	return nil
}

// CommitChecks returns the checks on sha: the connector's own checks made with
// EnsureCheck and the seeded check runs, then the seeded statuses, as the HTTP
// API reads them.
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
			runs = append(runs, CommitCheck{Kind: CheckKindRun, Name: c.Check.Name, Status: c.Check.Status, Conclusion: c.Check.Conclusion, Own: ownCheck(CheckKindRun, c.Check.Name)})
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
