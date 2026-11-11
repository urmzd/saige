package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// FakeCommit is an immutable record of an atomic in-memory commit.
type FakeCommit struct {
	Ref            Ref
	Parent, Marker string
	Changes        ChangeRequest
	Result         CommitResult
}

// SetWritePermission changes a collaborator's effective in-memory permission.
func (f *Fake) SetWritePermission(ref Ref, login string, allowed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writers == nil {
		f.writers = make(map[string]bool)
	}
	f.writers[strings.ToLower(ref.Owner+"/"+ref.Repo+":"+login)] = allowed
}

// CanWrite implements the effective permission lookup.
func (f *Fake) CanWrite(_ context.Context, ref Ref, login string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return false, f.Err
	}
	return f.writers[strings.ToLower(ref.Owner+"/"+ref.Repo+":"+login)], nil
}

// SeedFiles installs the complete file snapshot for the current seeded PR head.
func (f *Fake) SeedFiles(ref Ref, files map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	head := f.contexts[ref.String()].Subject.Head.SHA
	if !validSHA(head) {
		return fmt.Errorf("github: seed PR context before files")
	}
	if f.files == nil {
		f.files = make(map[string]map[string]string)
	}
	clone := make(map[string]string, len(files))
	for name, content := range files {
		clone[name] = content
	}
	f.files[ref.String()+":"+head] = clone
	return nil
}

// CommitChanges applies the entire batch under a single lock and recovers an
// identical retried operation. A changed head refuses different edits.
func (f *Fake) CommitChanges(_ context.Context, ref Ref, expected, run string, args ChangeRequest) (CommitResult, error) {
	if err := validateChanges(args); err != nil {
		return CommitResult{}, err
	}
	if !validSHA(expected) || run == "" {
		return CommitResult{}, fmt.Errorf("github: missing pinned head or request")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return CommitResult{}, f.Err
	}
	current, ok := f.contexts[ref.String()]
	if !ok || current.Subject.State != "open" {
		return CommitResult{}, fmt.Errorf("github: no open PR")
	}
	marker := mutationMarker(run, expected, args)
	if current.Subject.Head.SHA != expected {
		for _, commit := range f.commits {
			if commit.Ref == ref && commit.Result.SHA == current.Subject.Head.SHA && commit.Parent == expected && commit.Marker == marker {
				out := commit.Result
				out.Recovered = true
				return out, nil
			}
		}
		return CommitResult{}, fmt.Errorf("github: PR head changed")
	}
	files := map[string]string{}
	for name, content := range f.files[ref.String()+":"+expected] {
		files[name] = content
	}
	for _, file := range args.Files {
		if file.Delete {
			if _, ok := files[file.Path]; !ok {
				return CommitResult{}, fmt.Errorf("github: deletion names missing file %s", file.Path)
			}
			delete(files, file.Path)
		} else {
			files[file.Path] = file.Content
		}
	}
	digest := sha256.Sum256([]byte(marker))
	sha := hex.EncodeToString(digest[:20])
	result := CommitResult{SHA: sha, URL: fmt.Sprintf("https://github.com/%s/%s/commit/%s", ref.Owner, ref.Repo, sha)}
	current.Subject.Head.SHA = sha
	f.contexts[ref.String()] = current
	if f.files == nil {
		f.files = make(map[string]map[string]string)
	}
	f.files[ref.String()+":"+sha] = files
	args.Files = append([]FileEdit(nil), args.Files...)
	f.commits = append(f.commits, FakeCommit{Ref: ref, Parent: expected, Marker: marker, Changes: args, Result: result})
	return result, nil
}

// Commits returns detached records of completed in-memory commits.
func (f *Fake) Commits() []FakeCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]FakeCommit(nil), f.commits...)
	for i := range out {
		out[i].Changes.Files = append([]FileEdit(nil), out[i].Changes.Files...)
	}
	return out
}

var _ MutationAPI = (*Fake)(nil)
