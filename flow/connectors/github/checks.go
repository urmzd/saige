package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
)

// CheckAPI owns one check per agent run on the pinned commit.
type CheckAPI interface {
	EnsureCheck(context.Context, Ref, Check) (int64, error)
	UpdateCheck(context.Context, Ref, int64, Check) error
}

// Check is the GitHub check-run payload. ExternalID is the stable flow run ID.
type Check struct {
	Name       string      `json:"name"`
	HeadSHA    string      `json:"head_sha,omitempty"`
	ExternalID string      `json:"external_id"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion,omitempty"`
	Output     CheckOutput `json:"output"`
}

// CheckOutput is the human-facing progress or review result.
type CheckOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// EnsureCheck recovers an existing check by external ID, including after an
// uncertain create response. GitHub has no atomic idempotency-key operation;
// deployments must serialize delivery for each run, as the sink engine does.
func (a *HTTPAPI) EnsureCheck(ctx context.Context, ref Ref, check Check) (int64, error) {
	if err := validReadRef(ref, 1); err != nil {
		return 0, err
	}
	if !validSHA(check.HeadSHA) || check.ExternalID == "" {
		return 0, fmt.Errorf("github: check needs head SHA and run ID")
	}
	base := fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo)
	for page := 1; ; page++ {
		var list struct {
			Checks []struct {
				ID         int64  `json:"id"`
				ExternalID string `json:"external_id"`
				Name       string `json:"name"`
				HeadSHA    string `json:"head_sha"`
			} `json:"check_runs"`
		}
		path := fmt.Sprintf("%s/commits/%s/check-runs?filter=all&check_name=%s&per_page=100&page=%d", base, check.HeadSHA, url.QueryEscape(check.Name), page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return 0, fmt.Errorf("github: find check: %w", err)
		}
		for _, item := range list.Checks {
			if item.ExternalID == check.ExternalID && item.Name == check.Name && item.HeadSHA == check.HeadSHA && item.ID > 0 {
				if err := a.UpdateCheck(ctx, ref, item.ID, check); err != nil {
					return 0, err
				}
				return item.ID, nil
			}
		}
		if len(list.Checks) < 100 {
			break
		}
	}
	var result struct {
		ID int64 `json:"id"`
	}
	if err := a.client.Do(ctx, http.MethodPost, base+"/check-runs", check, &result); err != nil {
		return 0, fmt.Errorf("github: create check: %w", err)
	}
	if result.ID <= 0 {
		return 0, fmt.Errorf("github: create check returned no ID")
	}
	return result.ID, nil
}

// UpdateCheck updates progress without moving the check to another commit.
func (a *HTTPAPI) UpdateCheck(ctx context.Context, ref Ref, id int64, check Check) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("github: invalid check ID")
	}
	check.HeadSHA = ""
	path := fmt.Sprintf("/repos/%s/%s/check-runs/%d", ref.Owner, ref.Repo, id)
	if err := a.client.Do(ctx, http.MethodPatch, path, check, nil); err != nil {
		return fmt.Errorf("github: update check: %w", err)
	}
	return nil
}

func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' {
			continue
		}
		return false
	}
	return true
}

func (c *Connector) checkBody(v render.View) string {
	check := Check{Name: "fiber/" + v.Agent, ExternalID: v.RunID, Status: "in_progress", Output: CheckOutput{Title: sink.StatusLabel(v.Status)}}
	if check.Output.Title == "" {
		check.Output.Title = "working"
	}
	switch v.Status {
	case render.StatusAccepted:
		check.Status = "queued"
	case render.StatusFailed:
		check.Status = "completed"
		check.Conclusion = "failure"
	case render.StatusStopped:
		// Someone ended the run on purpose. That is GitHub's "cancelled":
		// finished, and neither a pass nor a failure.
		check.Status = "completed"
		check.Conclusion = "cancelled"
	case render.StatusDone:
		check.Status = "completed"
		check.Conclusion = "neutral"
		if c.cfg.Conclusion != nil {
			check.Conclusion = c.cfg.Conclusion(v)
		}

	}
	// Review completion is neutral unless an error was found: completing a model
	// turn does not prove tests passed or every changed line was inspected.
	progress := v
	progress.Thinking = ""
	check.Output.Summary = sink.Clamp(c.body(progress), MaxBodyChars-1024)
	if check.Output.Summary == "" {
		check.Output.Summary = "Waiting for agent output."
	}
	encoded, _ := json.Marshal(check)
	return string(encoded)
}

type checkSurface struct {
	c   *Connector
	api CheckAPI
}

func (s checkSurface) Create(ctx context.Context, target address.Address, body string) (string, error) {
	ref, err := s.c.ref(target)
	if err != nil {
		return "", err
	}
	var check Check
	if err := json.Unmarshal([]byte(body), &check); err != nil {
		return "", err
	}
	check.HeadSHA = target.Params["head_sha"]
	id, err := s.api.EnsureCheck(ctx, ref, check)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}
func (s checkSurface) Update(ctx context.Context, target address.Address, handle, body string) error {
	ref, err := s.c.ref(target)
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(handle, 10, 64)
	if err != nil {
		return err
	}
	var check Check
	if err := json.Unmarshal([]byte(body), &check); err != nil {
		return err
	}
	return s.api.UpdateCheck(ctx, ref, id, check)
}
