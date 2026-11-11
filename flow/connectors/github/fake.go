package github

import (
	"context"
	"sync"
)

// Fake is a complete in-memory GitHub, not a stub: comments have ids and can be
// edited, labels accumulate as a set the way GitHub's do, and review requests are
// recorded per pull request. It ships in the package so the mesh can be run
// end to end — mention in, edited comment out — with no token and no network.
type Fake struct {
	fixPulls      map[string]FakeFixPull
	defaults      map[string]RepositoryBase
	reactions     map[string][]NativeReaction
	reviewThreads map[string]ReviewThreadState
	submitted     []FakeReview
	workflows     map[string]map[int64]WorkflowRun
	writers       map[string]bool
	files         map[string]map[string]string
	commits       []FakeCommit
	mu            sync.Mutex
	contexts      map[string]Context
	checks        map[int64]FakeCheck
	comments      []FakeComment
	byID          map[int64]int
	labels        map[string][]string
	reviews       map[string][]string
	nextID        int64
	bodies        map[string]string
	// Err, when set, fails every call, which is how a test drives the retry path.
	Err error
}

// FakeComment is one comment in the fake repository.
type FakeComment struct {
	ID   int64
	Ref  Ref
	Body string
	// ReviewRoot is nonzero for a reply in a PR review thread.
	ReviewRoot int64
	GuideAgent string
	GuideLogin string
	// Edits counts body replacements, which is what "one comment, edited" is
	// asserted on.
	Edits int
	// Login and UserType say who posted it; empty is the app itself. BodyHTML
	// is the rendered body Comments reports.
	Login    string
	UserType string
	BodyHTML string
}

// NewFake returns an empty fake GitHub.
func NewFake() *Fake {
	return &Fake{
		byID:    make(map[int64]int),
		labels:  make(map[string][]string),
		reviews: make(map[string][]string),
		nextID:  1000,
	}
}

// CreateComment implements API.
func (f *Fake) CreateComment(_ context.Context, ref Ref, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return 0, f.Err
	}
	f.nextID++
	f.comments = append(f.comments, FakeComment{ID: f.nextID, Ref: ref, Body: body})
	f.byID[f.nextID] = len(f.comments) - 1
	return f.nextID, nil
}

// UpdateComment implements API.
func (f *Fake) UpdateComment(_ context.Context, _ Ref, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	i, ok := f.byID[id]
	if !ok {
		return &NotFoundError{What: "comment", ID: id}
	}
	f.comments[i].Body = body
	f.comments[i].Edits++
	return nil
}

// RequestReview implements API.
func (f *Fake) RequestReview(_ context.Context, ref Ref, users, teams []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.reviews[ref.String()] = append(f.reviews[ref.String()], append(append([]string{}, users...), teams...)...)
	return nil
}

// AddLabels implements API.
func (f *Fake) AddLabels(_ context.Context, ref Ref, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	key := ref.String()
	for _, label := range labels {
		found := false
		for _, have := range f.labels[key] {
			if have == label {
				found = true
				break
			}
		}
		if !found {
			f.labels[key] = append(f.labels[key], label)
		}
	}
	return nil
}

// Comments returns every comment, oldest first.
func (f *Fake) Comments() []FakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeComment(nil), f.comments...)
}

// Labels returns the labels on a ref.
func (f *Fake) Labels(ref Ref) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.labels[ref.String()]...)
}

// Reviewers returns the reviewers requested on a ref.
func (f *Fake) Reviewers(ref Ref) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reviews[ref.String()]...)
}

// NotFoundError is the fake's 404, so a test can tell "GitHub said no" from
// "the connector lost track of which comment it owned".
type NotFoundError struct {
	What string
	ID   int64
}

func (e *NotFoundError) Error() string {
	return "github: no such " + e.What
}
