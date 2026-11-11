package slack

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
)

// ReactionAPI supports replacing lifecycle reactions without posting status
// messages. APIs without it can keep using ordinary message rendering.
type ReactionAPI interface {
	AddReaction(context.Context, Reaction) error
	RemoveReaction(context.Context, Reaction) error
}

// RemoveReaction removes the agent's reaction from a message; one already gone is not an error.
func (a *HTTPAPI) RemoveReaction(ctx context.Context, r Reaction) error {
	err := a.call(ctx, "/reactions.remove", map[string]string{paramChannel: r.Channel, "timestamp": r.TS, "name": r.Name}, &response{})
	if errorCode(err) == "no_reaction" {
		return nil
	}
	return err
}

// RemoveReaction removes a recorded reaction, as HTTPAPI.RemoveReaction does.
func (f *Fake) RemoveReaction(_ context.Context, r Reaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	i, ok := f.byTS[r.TS]
	if !ok {
		return &Error{Method: "/reactions.remove", Code: codeMessageNotFound}
	}
	f.messages[i].Reactions = slices.DeleteFunc(f.messages[i].Reactions, func(name string) bool { return name == r.Name })
	return nil
}

type reactionSurface struct{ api ReactionAPI }

func (s reactionSurface) Create(ctx context.Context, target address.Address, name string) (string, error) {
	if err := s.set(ctx, target, name); err != nil {
		return "", err
	}
	return "reaction", nil
}
func (s reactionSurface) Update(ctx context.Context, target address.Address, _ string, name string) error {
	return s.set(ctx, target, name)
}
func (s reactionSurface) set(ctx context.Context, target address.Address, name string) error {
	ts, _ := target.Param("thread")
	if ts == "" {
		return nil
	}
	reaction := Reaction{Workspace: target.Workspace, Channel: target.Path[0], TS: ts}
	for _, previous := range []string{"eyes", "white_check_mark", "x", "raising_hand", emojiRaisedHand} {
		if previous == name {
			continue
		}
		reaction.Name = previous
		if err := s.api.RemoveReaction(ctx, reaction); err != nil {
			return err
		}
	}
	reaction.Name = name
	err := s.api.AddReaction(ctx, reaction)
	if errorCode(err) == codeAlreadyReacted {
		return nil
	}
	return err
}
func reactionBody(v render.View) string {
	switch v.Status {
	case render.StatusAccepted, render.StatusRunning:
		return "eyes"
	case render.StatusParked:
		return "raising_hand"
	case render.StatusDone:
		return "white_check_mark"
	case render.StatusFailed:
		return "x"
	case render.StatusStopped:
		return emojiRaisedHand
	default:
		return ""
	}
}
func replyBody(v render.View) string {
	text := strings.TrimSpace(v.Text)
	if v.Status == render.StatusFailed {
		return "Could not finish: " + nativeText(v.Error)
	}
	if v.Status == render.StatusStopped && text == "" {
		return "Stopped."
	}
	if v.Status == render.StatusParked && v.Question != nil {
		return nativeText(v.Question.Prompt)
	}
	return nativeText(text)
}

// nativeText preserves explicit Slack mentions, channel references and HTTPS
// permalinks, while escaping literal markup and refusing broadcast tokens.
func nativeText(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		start := strings.IndexByte(text, '<')
		if start < 0 {
			out.WriteString(escape(text))
			break
		}
		out.WriteString(escape(text[:start]))
		text = text[start:]
		end := strings.IndexByte(text, '>')
		if end < 0 {
			out.WriteString(escape(text))
			break
		}
		token := text[1:end]
		bare, _, _ := strings.Cut(token, "|")
		valid := false
		if len(bare) > 2 && (strings.HasPrefix(bare, "@U") || strings.HasPrefix(bare, "@W") || strings.HasPrefix(bare, "#C") || strings.HasPrefix(bare, "#G")) {
			valid = true
			for _, r := range bare[2:] {
				if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
					valid = false
					break
				}
			}
		}
		if strings.HasPrefix(bare, "https://") && !strings.ContainsAny(token, "\n\r<") {
			valid = true
		}
		if valid {
			out.WriteString("<" + strings.ReplaceAll(token, "&", "&amp;") + ">")
		} else {
			out.WriteString(escape(text[:end+1]))
		}
		text = text[end+1:]
	}
	return out.String()
}

func reactionsAvailable(api API) (ReactionAPI, error) {
	r, ok := api.(ReactionAPI)
	if !ok {
		return nil, fmt.Errorf("slack: status reactions require ReactionAPI")
	}
	return r, nil
}
