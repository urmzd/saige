package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
)

// ReviewAPI submits a native pull request review, including inline suggestions.
// Hosts validate locations against the reviewed commit with PrepareReview.
type ReviewAPI interface {
	CreateReview(context.Context, Ref, Review) (ReviewResult, error)
}

// Review is GitHub's native review request. CommitID pins every comment to the
// revision actually inspected; an omitted event must never create a draft.
type Review struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments,omitempty"`
}

// ReviewComment anchors a finding to the new (RIGHT) or old (LEFT) diff side.
// Change, when present, is a replacement including an intentional empty string.
type ReviewComment struct {
	FindingID string  `json:"-"`
	Path      string  `json:"path"`
	Body      string  `json:"body"`
	Line      int     `json:"line"`
	Side      string  `json:"side"`
	StartLine int     `json:"start_line,omitempty"`
	StartSide string  `json:"start_side,omitempty"`
	Change    *string `json:"-"`
}

type ReviewResult struct {
	ID  int64  `json:"id"`
	URL string `json:"html_url"`
}

var diffHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ErrNoApproval refuses an approving review: agents comment or request
// changes, and a person approves and merges.
var ErrNoApproval = errors.New("github: agents never approve pull requests")

// PrepareReview checks every inline location against actual changed-file
// patches. Unavailable or invalid anchors degrade to the review body; they do
// not disappear. An approving review is refused with ErrNoApproval.
func PrepareReview(review Review, files []ContextFile) (Review, error) {
	return prepareReview(review, files, false)
}

// prepareReview also reads back an approval submitted before agents stopped
// approving, so recovery can match an existing review; it never creates one.
func prepareReview(review Review, files []ContextFile, recovering bool) (Review, error) {
	if review.CommitID == "" {
		return Review{}, fmt.Errorf("github: review needs the inspected commit")
	}
	switch review.Event {
	case "COMMENT", "REQUEST_CHANGES":
	case "APPROVE":
		if !recovering {
			return Review{}, ErrNoApproval
		}
	default:
		return Review{}, fmt.Errorf("github: invalid review event %q", review.Event)
	}
	if len(review.Comments) > 100 {
		return Review{}, fmt.Errorf("github: review exceeds 100 findings")
	}
	patches := map[string]map[string]map[int]bool{}
	for _, f := range files {
		patches[f.Filename] = diffLines(f.Patch)
	}
	out := review
	out.Comments = nil
	for _, comment := range review.Comments {
		if comment.Side == "" {
			comment.Side = "RIGHT"
		}
		start := comment.StartLine
		if start == 0 {
			start = comment.Line
		}
		side := comment.StartSide
		if side == "" {
			side = comment.Side
		}
		valid := comment.Line > 0 && start > 0 && start <= comment.Line && side == comment.Side && comment.Line-start <= 1000
		lines := patches[comment.Path][comment.Side]
		for line := start; valid && line <= comment.Line; line++ {
			valid = lines[line]
		}
		if comment.Change != nil && (comment.Side != "RIGHT" || strings.Contains(*comment.Change, "```")) {
			valid = false
		}
		if !valid {
			out.Body += fmt.Sprintf("\n\n**%s:%d** (location unverified)\n\n%s", comment.Path, comment.Line, comment.Body)
			if comment.Change != nil {
				out.Body += "\n\nProposed replacement (not applied):\n\n" + *comment.Change
			}
			continue
		}
		if comment.Change != nil {
			comment.Body += "\n\n```suggestion\n" + *comment.Change + "\n```"
			comment.Change = nil
		}
		if start == comment.Line {
			comment.StartLine = 0
			comment.StartSide = ""
		} else {
			comment.StartLine = start
			comment.StartSide = side
		}
		out.Comments = append(out.Comments, comment)
	}
	return out, nil
}

func diffLines(patch string) map[string]map[int]bool {
	lines := map[string]map[int]bool{"LEFT": {}, "RIGHT": {}}
	old, next := 0, 0
	active := false
	for _, row := range strings.Split(patch, "\n") {
		if match := diffHeader.FindStringSubmatch(row); match != nil {
			old, _ = strconv.Atoi(match[1])
			next, _ = strconv.Atoi(match[2])
			active = true
			continue
		}
		if !active || row == "" {
			continue
		}
		switch row[0] {
		case ' ':
			if old > 0 && next > 0 {
				lines["LEFT"][old] = true
				lines["RIGHT"][next] = true
			}
			old++
			next++
		case '-':
			if old > 0 {
				lines["LEFT"][old] = true
			}
			old++
		case '+':
			if next > 0 {
				lines["RIGHT"][next] = true
			}
			next++
		case '\\':
		default:
			active = false
		}
	}
	return lines
}

func (a *HTTPAPI) CreateReview(ctx context.Context, ref Ref, review Review) (ReviewResult, error) {
	if ref.Number <= 0 || review.CommitID == "" {
		return ReviewResult{}, fmt.Errorf("github: review needs a pull request and commit")
	}
	if review.Event == "APPROVE" {
		return ReviewResult{}, ErrNoApproval
	}
	if review.Event != "COMMENT" && review.Event != "REQUEST_CHANGES" {
		return ReviewResult{}, fmt.Errorf("github: review event is required")
	}
	var out ReviewResult
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", ref.Owner, ref.Repo, ref.Number)
	if err := a.client.Do(ctx, http.MethodPost, endpoint, review, &out); err != nil {
		return out, fmt.Errorf("github: review %s: %w", ref, err)
	}
	if out.ID == 0 {
		return out, fmt.Errorf("github: review returned no id")
	}
	return out, nil
}

func (c *Connector) reviewAction() connector.Action {
	return connector.Action{Name: "github_create_review", Description: "Submit a pull request review on the inspected commit. Inline findings outside its diff become body text. It comments or requests changes; agents never approve.", Schema: json.RawMessage(`{"type":"object","properties":{"address":{"type":"string"},"commit_id":{"type":"string"},"event":{"type":"string","enum":["COMMENT","REQUEST_CHANGES"]},"body":{"type":"string"},"comments":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"body":{"type":"string"},"line":{"type":"integer"},"side":{"type":"string","enum":["LEFT","RIGHT"]},"start_line":{"type":"integer"},"start_side":{"type":"string"},"change":{"type":"string"}},"required":["path","body","line"],"additionalProperties":false}}},"required":["commit_id","event","body"],"additionalProperties":false}`), Invoke: c.createReview}
}
func (c *Connector) createReview(ctx context.Context, target address.Address, raw json.RawMessage) (connector.Result, error) {
	if target.Connector == Name {
		if _, err := c.ref(target); err != nil {
			return connector.Result{}, err
		}
	}
	api, ok := c.cfg.API.(ReviewAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("github: API cannot create reviews")
	}
	reader, ok := c.cfg.API.(ContextAPI)
	if !ok {
		return connector.Result{}, fmt.Errorf("github: API cannot validate review locations")
	}
	var input struct {
		Address  string `json:"address"`
		CommitID string `json:"commit_id"`
		Event    string `json:"event"`
		Body     string `json:"body"`
		Comments []struct {
			Path      string  `json:"path"`
			Body      string  `json:"body"`
			Line      int     `json:"line"`
			Side      string  `json:"side"`
			StartLine int     `json:"start_line"`
			StartSide string  `json:"start_side"`
			Change    *string `json:"change"`
		} `json:"comments"`
	}
	if err := decodeArguments(raw, &input); err != nil {
		return connector.Result{}, err
	}
	target, err := c.actionTarget(target, input.Address)
	if err != nil {
		return connector.Result{}, err
	}
	ref, err := c.ref(target)
	if err != nil {
		return connector.Result{}, err
	}
	if len(target.Path) != 3 || target.Path[1] != "pull" {
		return connector.Result{}, fmt.Errorf("github: reviews require a pull request")
	}
	review := Review{CommitID: input.CommitID, Event: input.Event, Body: input.Body}
	for _, v := range input.Comments {
		review.Comments = append(review.Comments, ReviewComment{Path: v.Path, Body: v.Body, Line: v.Line, Side: v.Side, StartLine: v.StartLine, StartSide: v.StartSide, Change: v.Change})
	}
	var files []ContextFile
	for page := 1; page <= 10; page++ {
		snapshot, err := reader.ReadContext(ctx, ref, page)
		if err != nil {
			return connector.Result{}, err
		}
		if snapshot.Subject.Head.SHA != review.CommitID {
			return connector.Result{}, fmt.Errorf("github: pull request head changed; inspect the new commit before reviewing")
		}
		files = append(files, snapshot.Files...)
		if snapshot.NextPage == 0 {
			break
		}
	}
	review, err = PrepareReview(review, files)
	if err != nil {
		return connector.Result{}, err
	}
	result, err := api.CreateReview(ctx, ref, review)
	if err != nil {
		return connector.Result{}, err
	}
	return connector.Result{Summary: "Submitted pull request review", Address: refAddress(ref, "pull").WithParam("review", strconv.FormatInt(result.ID, 10))}, nil
}

var _ ReviewAPI = (*HTTPAPI)(nil)
