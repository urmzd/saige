package github

import (
	"context"
	"fmt"
	"strings"
)

func commitChecksKey(owner, repo, sha string) string {
	return strings.ToLower(owner + "/" + repo + "@" + sha)
}

// SeedCommitCheck reports a check run or commit status on a commit. Seeding a
// check with the same kind and name again replaces it, as a newer run does.
func (f *Fake) SeedCommitCheck(owner, repo, sha string, check CommitCheck) error {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return err
	}
	if check.Kind != CheckKindRun && check.Kind != CheckKindStatus || check.Name == "" || check.Status == "" {
		return fmt.Errorf("github: invalid commit check seed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitChecks == nil {
		f.commitChecks = make(map[string][]CommitCheck)
	}
	key := commitChecksKey(owner, repo, sha)
	for i, have := range f.commitChecks[key] {
		if have.Kind == check.Kind && have.Name == check.Name {
			f.commitChecks[key][i] = check
			return nil
		}
	}
	f.commitChecks[key] = append(f.commitChecks[key], check)
	return nil
}

// CommitChecks returns the seeded checks on sha: check runs first, then
// statuses, as the HTTP API reads them.
func (f *Fake) CommitChecks(_ context.Context, owner, repo, sha string) ([]CommitCheck, error) {
	if err := validCommitRef(owner, repo, sha); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	var out []CommitCheck
	for _, kind := range []string{CheckKindRun, CheckKindStatus} {
		n := 0
		for _, check := range f.commitChecks[commitChecksKey(owner, repo, sha)] {
			if check.Kind == kind {
				out = append(out, check)
				n++
			}
		}
		if n >= MaxCommitChecks {
			return nil, fmt.Errorf("github: commit has %d or more checks of kind %s", MaxCommitChecks, kind)
		}
	}
	return out, nil
}

var _ CommitChecksAPI = (*Fake)(nil)
