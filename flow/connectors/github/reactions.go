package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
)

// NativeReaction is a reaction with a stable GitHub identity for polling.
type NativeReaction struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	User    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

// ReactionAPI reads and replaces reactions on issues and comments. Pagination
// is explicit and bounded; the host decides when to poll.
type ReactionAPI interface {
	ListReactions(context.Context, address.Address, int) ([]NativeReaction, error)
	AddReaction(context.Context, address.Address, string) (int64, error)
	RemoveReaction(context.Context, address.Address, int64) error
}

func reactionPath(target address.Address) (string, error) {
	if target.Connector != Name || len(target.Path) != 3 {
		return "", fmt.Errorf("github: invalid reaction target")
	}
	root := "/repos/" + target.Workspace + "/" + target.Path[0]
	for _, param := range []string{"comment", "review_comment"} {
		if raw, _ := target.Param(param); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return "", fmt.Errorf("github: invalid comment id")
			}
			if param == "review_comment" {
				return fmt.Sprintf("%s/pulls/comments/%d/reactions", root, id), nil
			}
			return fmt.Sprintf("%s/issues/comments/%d/reactions", root, id), nil
		}
	}
	number, err := strconv.Atoi(target.Path[2])
	if err != nil || number <= 0 {
		return "", fmt.Errorf("github: invalid issue number")
	}
	return fmt.Sprintf("%s/issues/%d/reactions", root, number), nil
}
func (a *HTTPAPI) ListReactions(ctx context.Context, target address.Address, page int) ([]NativeReaction, error) {
	if page < 1 || page > 100 {
		return nil, fmt.Errorf("github: reaction page must be 1 to 100")
	}
	path, err := reactionPath(target)
	if err != nil {
		return nil, err
	}
	var out []NativeReaction
	err = a.client.Do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &out)
	return out, err
}
func (a *HTTPAPI) AddReaction(ctx context.Context, target address.Address, content string) (int64, error) {
	path, err := reactionPath(target)
	if err != nil {
		return 0, err
	}
	var out NativeReaction
	if err = a.client.Do(ctx, http.MethodPost, path, map[string]string{"content": content}, &out); err != nil {
		return 0, err
	}
	if out.ID <= 0 {
		return 0, fmt.Errorf("github: reaction returned no id")
	}
	return out.ID, nil
}
func (a *HTTPAPI) RemoveReaction(ctx context.Context, target address.Address, id int64) error {
	path, err := reactionPath(target)
	if err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("github: invalid reaction id")
	}
	return a.client.Do(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", path, id), nil, nil)
}

type reactionSurface struct {
	api   ReactionAPI
	login string
}

func (s reactionSurface) Create(ctx context.Context, target address.Address, content string) (string, error) {
	if err := s.set(ctx, target, content); err != nil {
		return "", err
	}
	return "reaction", nil
}
func (s reactionSurface) Update(ctx context.Context, target address.Address, _ string, content string) error {
	return s.set(ctx, target, content)
}
func (s reactionSurface) set(ctx context.Context, target address.Address, content string) error {
	exists := false
	for page := 1; page <= 100; page++ {
		reactions, err := s.api.ListReactions(ctx, target, page)
		if err != nil {
			return err
		}
		for _, r := range reactions {
			if !strings.EqualFold(r.User.Login, s.login) {
				continue
			}
			if r.Content == content {
				exists = true
				continue
			}
			switch r.Content {
			case "eyes", "rocket", "confused", "-1":
				if err := s.api.RemoveReaction(ctx, target, r.ID); err != nil {
					return err
				}
			}
		}
		if len(reactions) < 100 {
			break
		}
		if page == 100 {
			return fmt.Errorf("github: lifecycle reactions exceed 100 pages")
		}
	}
	if exists {
		return nil
	}
	_, err := s.api.AddReaction(ctx, target, content)
	return err
}
func reactionBody(v render.View) string {
	switch v.Status {
	case render.StatusAccepted, render.StatusRunning:
		return "eyes"
	case render.StatusDone:
		return "rocket"
	case render.StatusFailed:
		return "-1"
	case render.StatusParked, render.StatusStopped:
		return "confused"
	default:
		return ""
	}
}
