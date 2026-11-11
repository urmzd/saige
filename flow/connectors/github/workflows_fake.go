package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func workflowKey(ref Ref) string { return strings.ToLower(ref.Owner + "/" + ref.Repo) }

// SeedWorkflow installs an observed CI attempt, including a later completion.
func (f *Fake) SeedWorkflow(ref Ref, run WorkflowRun) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	if run.ID <= 0 || run.Attempt <= 0 || !validSHA(run.HeadSHA) {
		return fmt.Errorf("github: invalid workflow seed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workflows == nil {
		f.workflows = make(map[string]map[int64]WorkflowRun)
	}
	key := workflowKey(ref)
	if f.workflows[key] == nil {
		f.workflows[key] = make(map[int64]WorkflowRun)
	}
	f.workflows[key][run.ID] = run
	return nil
}

// WorkflowRun returns an observed run without inventing a completion.
func (f *Fake) WorkflowRun(_ context.Context, ref Ref, id int64) (WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return WorkflowRun{}, f.Err
	}
	run, ok := f.workflows[workflowKey(ref)][id]
	if !ok {
		return WorkflowRun{}, &NotFoundError{What: "workflow run", ID: id}
	}
	return run, nil
}

// WorkflowRuns pages through the requested commit's observed runs.
func (f *Fake) WorkflowRuns(_ context.Context, ref Ref, sha string, page int) (WorkflowPage, error) {
	if err := validReadRef(ref, page); err != nil {
		return WorkflowPage{}, err
	}
	if !validSHA(sha) {
		return WorkflowPage{}, fmt.Errorf("github: workflows require a full commit SHA")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return WorkflowPage{}, f.Err
	}
	all := []WorkflowRun{}
	for _, run := range f.workflows[workflowKey(ref)] {
		if run.HeadSHA == sha {
			all = append(all, run)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if page-1 > len(all)/30 {
		return WorkflowPage{Runs: []WorkflowRun{}}, nil
	}
	start := (page - 1) * 30
	end := min(start+30, len(all))
	out := WorkflowPage{Runs: all[start:end]}
	if end-start == 30 {
		out.NextPage = page + 1
	}
	return out, nil
}

// RerunWorkflow moves a completed attempt to queued once. Tests or demos may
// subsequently seed the actual result, just as GitHub updates a running job.
func (f *Fake) RerunWorkflow(_ context.Context, ref Ref, id int64, sha string, attempt int) (WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return WorkflowRun{}, f.Err
	}
	runs := f.workflows[workflowKey(ref)]
	current, ok := runs[id]
	if !ok {
		return WorkflowRun{}, &NotFoundError{What: "workflow run", ID: id}
	}
	if current.HeadSHA != sha || attempt <= 0 || current.Attempt < attempt {
		return WorkflowRun{}, fmt.Errorf("github: workflow does not match authorized attempt")
	}
	if current.Attempt > attempt || current.Status != "completed" {
		return current, nil
	}
	current.Attempt++
	current.Status = "queued"
	current.Conclusion = ""
	current.Requested = false
	runs[id] = current
	response := current
	response.Attempt = attempt
	response.Status = "requested"
	response.Requested = true
	return response, nil
}

var _ WorkflowAPI = (*Fake)(nil)
