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
	"sync/atomic"

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

// ErrNoApproval refuses an approving review: the model comments or requests
// changes. Only the host's review sink posts an approval, and only when
// Config.MayApprove says yes.
var ErrNoApproval = errors.New("github: agents never approve pull requests")

// approvalKey carries the one approval the review sink was granted. The type is
// unexported, so nothing outside this package can put a grant on a context.
type approvalKey struct{}

type approvalGrant struct {
	ref    Ref
	commit string
	used   atomic.Bool
}

// withApproval grants one approving CreateReview for exactly this pull request
// and commit. Only the review sink calls it, after Config.MayApprove said yes.
func withApproval(ctx context.Context, ref Ref, commit string) context.Context {
	return context.WithValue(ctx, approvalKey{}, &approvalGrant{ref: ref, commit: commit})
}

// approvalGranted reports whether ctx holds the sink's grant for this review,
// and spends it: a grant posts one approval.
func approvalGranted(ctx context.Context, ref Ref, review Review) bool {
	grant, ok := ctx.Value(approvalKey{}).(*approvalGrant)
	return ok && grant.commit != "" && grant.commit == review.CommitID && grant.ref == ref && grant.used.CompareAndSwap(false, true)
}

// ApprovalRequest is what Config.MayApprove decides: the pull request as the
// review sink read it, and the review it would post as an approval. Draft is
// always false today, since the sink posts a draft's review as a comment
// without asking; it is here so a host can state the rule itself.
type ApprovalRequest struct {
	Owner, Repo string
	Number      int
	// Commit is the reviewed head, the commit an approval lands on.
	Commit string
	// Author is the login that opened the pull request.
	Author string
	Draft  bool
	// RunID is the run whose review this is. The connector's check for it
	// carries it as its external ID.
	RunID string
	// App is the slug of the connector's own app, from Config.Login without
	// "[bot]", in lower case. Empty when Config.Login is empty.
	App    string
	Review Review
}

// OwnCheck reports whether check is the connector's own check for the run
// being reviewed: a check run named "fiber/...", published by App, whose
// external ID is RunID. That check is still in progress while MayApprove runs.
// Another app's check, or the check of another run, is never own, and neither
// is a commit status. With App or RunID empty, nothing is own.
func (r ApprovalRequest) OwnCheck(check CommitCheck) bool {
	return r.App != "" && r.RunID != "" && check.Kind == CheckKindRun &&
		strings.HasPrefix(check.Name, ownCheckPrefix) &&
		strings.EqualFold(check.App, r.App) && check.ExternalID == r.RunID
}

// appSlug is the app slug a login names: "humagen[bot]" and "humagen" are both
// the app "humagen".
func appSlug(login string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

// PrepareReview checks every inline location against actual changed-file
// patches. Unavailable or invalid anchors degrade to the review body; they do
// not disappear. An approving review is refused with ErrNoApproval.
func PrepareReview(review Review, files []ContextFile) (Review, error) {
	return prepareReview(review, files, false)
}

// prepareReview with allowApproval accepts an APPROVE event. The review sink
// passes it after Config.MayApprove said yes, and recovery passes it to read
// back an approval already posted; neither path is reachable from a tool.
func prepareReview(review Review, files []ContextFile, allowApproval bool) (Review, error) {
	if review.CommitID == "" {
		return Review{}, fmt.Errorf("github: review needs the inspected commit")
	}
	switch review.Event {
	case "COMMENT", "REQUEST_CHANGES":
	case "APPROVE":
		if !allowApproval {
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

// reviewEventAllowed refuses an APPROVE that does not carry the review sink's
// grant for this pull request and commit, and any event GitHub would read as a
// draft.
func reviewEventAllowed(ctx context.Context, ref Ref, review Review) error {
	switch review.Event {
	case "COMMENT", "REQUEST_CHANGES":
		return nil
	case "APPROVE":
		if approvalGranted(ctx, ref, review) {
			return nil
		}
		return ErrNoApproval
	default:
		return fmt.Errorf("github: review event is required")
	}
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
	if err := reviewEventAllowed(ctx, ref, review); err != nil {
		return ReviewResult{}, err
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
