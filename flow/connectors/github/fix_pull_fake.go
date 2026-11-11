package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

type FakeFixPull struct {
	Ref     Ref
	Request FixPullRequest
	Result  FixPullResult
	Body    string
}

func (f *Fake) SeedDefaultBranch(ref Ref, base RepositoryBase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.defaults == nil {
		f.defaults = map[string]RepositoryBase{}
	}
	f.defaults[strings.ToLower(ref.Owner+"/"+ref.Repo)] = base
}
func (f *Fake) ReadDefaultBranch(_ context.Context, ref Ref) (RepositoryBase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return RepositoryBase{}, f.Err
	}
	base, ok := f.defaults[strings.ToLower(ref.Owner+"/"+ref.Repo)]
	if !ok {
		return RepositoryBase{}, fmt.Errorf("github: no seeded default branch")
	}
	return base, nil
}
func (f *Fake) CreateFixPull(_ context.Context, ref Ref, run string, r FixPullRequest) (FixPullResult, error) {
	if err := validateFixPull(r); err != nil {
		return FixPullResult{}, err
	}
	if run == "" {
		return FixPullResult{}, fmt.Errorf("github: missing run identity")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return FixPullResult{}, f.Err
	}
	branch, marker := fixIdentity(ref, run, r)
	key := strings.ToLower(ref.Owner+"/"+ref.Repo) + ":" + branch
	if old, ok := f.fixPulls[key]; ok {
		out := old.Result
		out.Recovered = true
		return out, nil
	}
	if f.defaults[strings.ToLower(ref.Owner+"/"+ref.Repo)] != r.Base {
		return FixPullResult{}, fmt.Errorf("github: default branch changed")
	}
	if f.fixPulls == nil {
		f.fixPulls = map[string]FakeFixPull{}
	}
	f.nextID++
	hash := sha256.Sum256([]byte(marker))
	result := FixPullResult{Number: int(f.nextID), URL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", ref.Owner, ref.Repo, f.nextID), Branch: branch, SHA: hex.EncodeToString(hash[:20])}
	r.Changes.Files = slices.Clone(r.Changes.Files)
	f.fixPulls[key] = FakeFixPull{Ref: ref, Request: r, Result: result, Body: fixBody(ref, r, marker)}
	return result, nil
}
func (f *Fake) FixPulls() []FakeFixPull {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeFixPull, 0, len(f.fixPulls))
	for _, v := range f.fixPulls {
		v.Request.Changes.Files = slices.Clone(v.Request.Changes.Files)
		out = append(out, v)
	}
	return out
}
