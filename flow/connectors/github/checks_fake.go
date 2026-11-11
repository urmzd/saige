package github

import (
	"context"
	"fmt"
	"sort"
)

// FakeCheck is one check maintained by the in-memory GitHub API.
type FakeCheck struct {
	ID    int64
	Ref   Ref
	Check Check
	Edits int
}

// EnsureCheck creates or recovers a check by repository, head and external ID.
func (f *Fake) EnsureCheck(_ context.Context, ref Ref, check Check) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	if !validSHA(check.HeadSHA) || check.ExternalID == "" {
		return 0, fmt.Errorf("github: check needs SHA and run ID")
	}
	if f.checks == nil {
		f.checks = make(map[int64]FakeCheck)
	}
	for id, old := range f.checks {
		if old.Ref.Owner == ref.Owner && old.Ref.Repo == ref.Repo && old.Check.HeadSHA == check.HeadSHA && old.Check.ExternalID == check.ExternalID && old.Check.Name == check.Name {
			old.Check = check
			old.Edits++
			f.checks[id] = old
			return id, nil
		}
	}
	f.nextID++
	f.checks[f.nextID] = FakeCheck{ID: f.nextID, Ref: ref, Check: check}
	return f.nextID, nil
}

// UpdateCheck replaces progress and preserves the original head.
func (f *Fake) UpdateCheck(_ context.Context, ref Ref, id int64, check Check) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	old, ok := f.checks[id]
	if !ok || old.Ref.Owner != ref.Owner || old.Ref.Repo != ref.Repo {
		return &NotFoundError{What: "check", ID: id}
	}
	check.HeadSHA = old.Check.HeadSHA
	old.Check = check
	old.Edits++
	f.checks[id] = old
	return nil
}

// Checks returns a stable snapshot in creation order.
func (f *Fake) Checks() []FakeCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCheck, 0, len(f.checks))
	for _, check := range f.checks {
		out = append(out, check)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

var _ CheckAPI = (*Fake)(nil)
var _ CheckAPI = (*HTTPAPI)(nil)
