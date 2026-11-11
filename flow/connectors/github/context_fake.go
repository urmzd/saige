package github

import (
	"context"
	"encoding/json"
	"fmt"
)

// SeedContext installs repository evidence in the in-memory API. The value is
// copied so callers cannot mutate the fake behind its lock.
func (f *Fake) SeedContext(ref Ref, value Context) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.contexts == nil {
		f.contexts = make(map[string]Context)
	}
	f.contexts[ref.String()] = copyContext(value)
	return nil
}

// ReadContext implements ContextAPI with the same page boundaries as HTTPAPI.
func (f *Fake) ReadContext(_ context.Context, ref Ref, page int) (Context, error) {
	if err := validReadRef(ref, page); err != nil {
		return Context{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return Context{}, f.Err
	}
	stored, ok := f.contexts[ref.String()]
	if !ok {
		return Context{}, fmt.Errorf("github: no context for %s", ref)
	}
	out := copyContext(stored)
	for _, c := range f.comments {
		if c.Ref == ref {
			if c.ReviewRoot != 0 {
				out.ReviewComments = append(out.ReviewComments, ContextComment{ID: c.ID, Body: c.Body, InReplyToID: c.ReviewRoot})
			} else {
				out.Comments = append(out.Comments, ContextComment{ID: c.ID, Body: c.Body})
			}
		}
	}
	out.Page = page
	out.NextPage = 0
	slice := func(n int) (int, int) {
		if page-1 > n/contextPageSize {
			return n, n
		}
		start := (page - 1) * contextPageSize
		end := min(start+contextPageSize, n)
		if end-start == contextPageSize {
			out.NextPage = page + 1
		}
		return start, end
	}
	lo, hi := slice(len(out.Comments))
	out.Comments = out.Comments[lo:hi]
	lo, hi = slice(len(out.Reviews))
	out.Reviews = out.Reviews[lo:hi]
	lo, hi = slice(len(out.ReviewComments))
	out.ReviewComments = out.ReviewComments[lo:hi]
	lo, hi = slice(len(out.Files))
	out.Files = out.Files[lo:hi]
	return out, nil
}

func copyContext(c Context) Context {
	b, _ := json.Marshal(c)
	var out Context
	_ = json.Unmarshal(b, &out)
	return out
}

var _ ContextAPI = (*Fake)(nil)
var _ ContextAPI = (*HTTPAPI)(nil)
