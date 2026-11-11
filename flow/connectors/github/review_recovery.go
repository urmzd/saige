package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/flow/connectors/internal/render"
)

func reviewMarker(view render.View, pinned string) string {
	sum := sha256.Sum256([]byte(view.Tenant + "\x00" + view.RunID + "\x00" + pinned))
	return "<!-- humagen-review:" + hex.EncodeToString(sum[:]) + " -->"
}

func markFindings(review Review) Review {
	review.Comments = slices.Clone(review.Comments)
	for i := range review.Comments {
		if id := review.Comments[i].FindingID; id != "" {
			review.Comments[i].Body += "\n\n" + findingMarker(id)
		}
	}
	return review
}

// Bind the remote payload to immutable final input, independent of comment
// listing order. Hashes keep private finding IDs out of GitHub's visible body.
func reviewPayloadMarker(view render.View, review Review) string {
	review.Comments = slices.Clone(review.Comments)
	slices.SortFunc(review.Comments, func(a, b ReviewComment) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return strings.Compare(string(left), string(right))
	})
	raw, _ := json.Marshal(struct {
		Tenant, Run, Channel, Text string
		Review                     Review
	}{view.Tenant, view.RunID, view.Channel, view.Text, review})
	sum := sha256.Sum256(raw)
	return "<!-- humagen-review-payload:" + hex.EncodeToString(sum[:]) + " -->"
}

// Recover only a previously submitted, exact app-owned payload. The current
// head guards creation, not observation of an existing
// review. Its original remote diff hunks supply evidence for finding mapping.
func (s reviewSurface) recoverReview(ctx context.Context, ref Ref, row ReviewRecord, marker string, view render.View) (string, error) {
	event := map[string]string{"COMMENTED": "COMMENT", "APPROVED": "APPROVE", "CHANGES_REQUESTED": "REQUEST_CHANGES"}[row.State]
	if row.ID <= 0 || event == "" {
		return "", fmt.Errorf("github: invalid submitted review disposition")
	}
	const prefix = "\n\n<!-- humagen-review-payload:"
	cut := strings.LastIndex(row.Body, prefix)
	if cut < 0 || !strings.HasSuffix(row.Body[:cut], "\n\n"+marker) {
		return "", fmt.Errorf("github: existing review has no immutable payload proof")
	}
	payload := row.Body[cut+2:]
	remote := Review{CommitID: row.CommitID, Event: event, Body: strings.TrimSuffix(row.Body[:cut], "\n\n"+marker)}
	var files []ContextFile
	fileIndex := map[string]int{}
	if api, ok := s.c.cfg.API.(ReviewCommentsAPI); ok {
		for page := 1; page <= 2; page++ {
			comments, err := api.ListReviewComments(ctx, ref, row.ID, page)
			if err != nil {
				return "", err
			}
			for _, c := range comments {
				if len(remote.Comments) >= 100 || c.ID <= 0 || c.Path == "" || c.DiffHunk == "" || !strings.EqualFold(c.User.Login, s.c.cfg.Login) || c.OriginalCommitID != row.CommitID && c.CommitID != row.CommitID {
					return "", fmt.Errorf("github: existing review comment proof is invalid")
				}
				line, start := c.OriginalLine, c.OriginalStartLine
				if line == 0 {
					line = c.Line
				}
				if start == 0 {
					start = c.StartLine
				}
				if start == line {
					start = 0
				}
				startSide := c.StartSide
				if start == 0 {
					startSide = ""
				}
				remote.Comments = append(remote.Comments, ReviewComment{Path: c.Path, Body: c.Body, Line: line, Side: c.Side, StartLine: start, StartSide: startSide})
				if i, exists := fileIndex[c.Path]; exists {
					files[i].Patch += "\n" + c.DiffHunk
				} else {
					fileIndex[c.Path] = len(files)
					files = append(files, ContextFile{Filename: c.Path, Patch: c.DiffHunk})
				}
			}
			if len(comments) < 100 {
				break
			}
			if page == 2 {
				return "", fmt.Errorf("github: existing review comments exceed recovery bound")
			}
		}
	}
	if reviewPayloadMarker(view, remote) != payload {
		return "", fmt.Errorf("github: existing review payload differs from immutable delivery intent")
	}
	// Only a verified payload may reach the host's historical parser. It cannot
	// create a review or bypass the host's current evidence visibility checks.
	var planned Review
	var ok bool
	var err error
	if s.c.cfg.RecoverReview != nil {
		planned, ok, err = s.c.cfg.RecoverReview(view, files, event)
	} else {
		planned, ok, err = s.c.cfg.Review(view, files)
	}
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("github: existing review no longer has a reproducible finding mapping")
	}
	planned, err = prepareReview(planned, files, true)
	if err != nil {
		return "", err
	}
	planned = markFindings(planned)
	if reviewPayloadMarker(view, planned) != payload {
		return "", fmt.Errorf("github: existing review finding mapping differs from submitted payload")
	}
	planned.Body = row.Body
	return reviewReceipt(row.ID, planned, view)
}
