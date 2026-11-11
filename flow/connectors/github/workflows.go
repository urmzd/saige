package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// WorkflowAPI reads actual CI results and reruns an existing immutable run.
// Rerunning by run ID avoids dispatching a mutable branch at a different SHA.
type WorkflowAPI interface {
	WorkflowRun(context.Context, Ref, int64) (WorkflowRun, error)
	WorkflowRuns(context.Context, Ref, string, int) (WorkflowPage, error)
	RerunWorkflow(context.Context, Ref, int64, string, int) (WorkflowRun, error)
}

// WorkflowRun records GitHub's observed attempt, status and conclusion.
// Requested marks a rerun request, never a claim that execution passed.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Attempt    int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
	Requested  bool   `json:"requested,omitempty"`
}

// WorkflowPage is a page of workflow runs for exactly one commit.
type WorkflowPage struct {
	Runs     []WorkflowRun `json:"workflow_runs"`
	NextPage int           `json:"next_page,omitempty"`
}

// WorkflowRun reads a run from the bound repository.
func (a *HTTPAPI) WorkflowRun(ctx context.Context, ref Ref, id int64) (WorkflowRun, error) {
	if err := validReadRef(ref, 1); err != nil {
		return WorkflowRun{}, err
	}
	if id <= 0 {
		return WorkflowRun{}, fmt.Errorf("github: workflow needs a positive run ID")
	}
	var run WorkflowRun
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", ref.Owner, ref.Repo, id)
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &run); err != nil {
		return WorkflowRun{}, fmt.Errorf("github: read workflow run: %w", err)
	}
	if run.ID != id || run.Attempt <= 0 || !validSHA(run.HeadSHA) {
		return WorkflowRun{}, fmt.Errorf("github: incomplete workflow run response")
	}
	return run, nil
}

// WorkflowRuns reads one page filtered to the exact commit.
func (a *HTTPAPI) WorkflowRuns(ctx context.Context, ref Ref, sha string, page int) (WorkflowPage, error) {
	if err := validReadRef(ref, page); err != nil {
		return WorkflowPage{}, err
	}
	if !validSHA(sha) {
		return WorkflowPage{}, fmt.Errorf("github: workflows require a full commit SHA")
	}
	var out WorkflowPage
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=30&page=%d", ref.Owner, ref.Repo, sha, page)
	if err := a.client.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return WorkflowPage{}, fmt.Errorf("github: list workflow runs: %w", err)
	}
	for _, run := range out.Runs {
		if run.HeadSHA != sha {
			return WorkflowPage{}, fmt.Errorf("github: workflow response did not match requested commit")
		}
	}
	if len(out.Runs) == 30 {
		out.NextPage = page + 1
	}
	return out, nil
}

// RerunWorkflow requests the next attempt only if the observed attempt still
// matches the one authorized by the user. Newer or running attempts are returned
// as observations so a retried action does not start another completed run.
func (a *HTTPAPI) RerunWorkflow(ctx context.Context, ref Ref, id int64, sha string, attempt int) (WorkflowRun, error) {
	current, err := a.WorkflowRun(ctx, ref, id)
	if err != nil {
		return WorkflowRun{}, err
	}
	if current.HeadSHA != sha || attempt <= 0 || current.Attempt < attempt {
		return WorkflowRun{}, fmt.Errorf("github: workflow does not match authorized commit and attempt")
	}
	if current.Attempt > attempt || current.Status != "completed" {
		return current, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/rerun", ref.Owner, ref.Repo, id)
	if err := a.client.Do(ctx, http.MethodPost, path, struct{}{}, nil); err != nil {
		return WorkflowRun{}, fmt.Errorf("github: rerun workflow: %w", err)
	}
	current.Requested = true
	current.Status = "requested"
	current.Conclusion = ""
	return current, nil
}

func (c *Connector) workflowStatusAction() connector.Action {
	return connector.Action{Name: "github_workflow_status", Description: "Read real GitHub Actions workflow attempts and results. Supply run_id to poll a run, or omit it to list runs for a commit. A requested or in-progress workflow has not passed. Follow next_page for more results.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string","description":"Optional issue/PR reference in a granted repository."},"run_id":{"type":"integer","minimum":1},"ref":{"type":"string","description":"Full commit SHA; defaults to this run's pinned head."},"page":{"type":"integer","minimum":1}},"additionalProperties":false}`), Invoke: c.workflowStatus}
}

func (c *Connector) workflowStatus(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	var args struct {
		Address string `json:"address"`
		RunID   int64  `json:"run_id"`
		Ref     string `json:"ref"`
		Page    int    `json:"page"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return connector.Result{}, err
	}
	if args.RunID < 0 || args.Page < 0 {
		return connector.Result{}, fmt.Errorf("github: run ID and page must be positive")
	}
	target, err := c.readTarget(target, args.Address)
	if err != nil {
		return connector.Result{}, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return connector.Result{}, err
	}
	if args.Ref == "" {
		args.Ref = target.Params["head_sha"]
	}
	api, ok := c.cfg.API.(WorkflowAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("%w: GitHub workflow reads", connector.ErrUnsupported)
	}
	var result any
	if args.RunID != 0 {
		run, err := api.WorkflowRun(ctx, ref, args.RunID)
		if err != nil {
			return connector.Result{}, err
		}
		if args.Ref != "" && run.HeadSHA != args.Ref {
			return connector.Result{}, fmt.Errorf("github: workflow belongs to a different commit")
		}
		result = run
	} else {
		if args.Page == 0 {
			args.Page = 1
		}
		runs, err := api.WorkflowRuns(ctx, ref, args.Ref, args.Page)
		if err != nil {
			return connector.Result{}, err
		}
		result = runs
	}
	data, _ := json.Marshal(result)
	return connector.Result{Summary: string(data), Data: data, Address: refAddress(ref, target.Path[1])}, nil
}

var _ WorkflowAPI = (*HTTPAPI)(nil)
