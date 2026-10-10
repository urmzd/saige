package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
)

// acpSessionIDRE matches the IDs saige acp gives sessions, so a stored
// session's ID never names a path outside the sessions directory.
var acpSessionIDRE = regexp.MustCompile(`^sess_[0-9a-f]{24}$`)

// acpSaved is a stored ACP session: what it rebinds from, and its
// conversation.
type acpSaved struct {
	SessionID string          `json:"session_id"`
	Agent     string          `json:"agent"`
	Model     string          `json:"model,omitempty"`
	Cwd       string          `json:"cwd"`
	Title     string          `json:"title,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
	Tree      json.RawMessage `json:"tree"`
}

func (s *acpServer) savedPath(id string) string {
	return filepath.Join(s.opts.sessionsDir, id+".json")
}

// save stores the session after a turn, when a sessions directory is set.
func (s *acpServer) save(sess *agenthost.Session[*acpSession]) {
	if s.opts.sessionsDir == "" {
		return
	}
	t := sess.Agent().Agent.Tree()
	raw, err := json.Marshal(t)
	if err == nil {
		rec := acpSaved{SessionID: sess.ID, Agent: sess.Host.agent, Model: sess.Host.model, Cwd: sess.Host.cwd,
			Title: acpTitle(t), UpdatedAt: time.Now().UTC(), Tree: raw}
		err = writeFileAtomic(s.savedPath(sess.ID), rec)
	}
	if err != nil {
		slog.Warn("saige acp: save session", "session", sess.ID, "error", err)
	}
}

// acpTitle is the start of the conversation's first user message.
func acpTitle(t *tree.Tree) string {
	msgs, err := t.FlattenBranch(t.Active())
	if err != nil {
		return ""
	}
	for _, m := range msgs {
		if um, ok := m.(types.UserMessage); ok {
			return oneLine(types.TextOf(um), 80)
		}
	}
	return ""
}

func writeFileAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// restore rebinds a stored session under its own ID, continuing its
// conversation. A live session with that ID is replaced.
func (s *acpServer) restore(ctx context.Context, id, cwd string, servers []acp.McpServer) (*agenthost.Session[*acpSession], error) {
	if s.opts.sessionsDir == "" {
		return nil, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
	}
	if !acpSessionIDRE.MatchString(id) {
		return nil, acp.NewInvalidParams(map[string]any{"error": "unknown session " + id})
	}
	raw, err := os.ReadFile(s.savedPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, acp.NewInvalidParams(map[string]any{"error": "unknown session " + id})
	}
	if err != nil {
		return nil, toACPError(err)
	}
	var rec acpSaved
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, toACPError(err)
	}
	t := new(tree.Tree)
	if err := t.UnmarshalJSON(rec.Tree); err != nil {
		return nil, toACPError(err)
	}
	if cwd == "" {
		cwd = rec.Cwd
	}
	st, err := newACPSessionState(cwd, servers, rec.Agent)
	if err != nil {
		return nil, err
	}
	if rec.Model != "" {
		st.model = rec.Model
	}
	s.sessions.Remove(id)
	sess, err := s.sessions.Create(id, st, func() (agenthost.Agent, error) {
		return s.opts.bind(ctx, acpBindRequest{agent: st.agent, model: st.model, cwd: st.cwd, servers: st.servers, tree: t})
	})
	if err != nil {
		return nil, toACPError(err)
	}
	return sess, nil
}

// LoadSession restores a stored session and replays its conversation as
// user and agent message chunks before answering.
func (s *acpServer) LoadSession(ctx context.Context, req acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	sess, err := s.restore(ctx, string(req.SessionId), req.Cwd, req.McpServers)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	t := sess.Agent().Agent.Tree()
	msgs, err := t.FlattenBranch(t.Active())
	if err != nil {
		return acp.LoadSessionResponse{}, toACPError(err)
	}
	for _, m := range msgs {
		var u *acp.SessionUpdate
		switch v := m.(type) {
		case types.UserMessage:
			if text := types.TextOf(v); text != "" {
				u = acp.Ptr(acp.UpdateUserMessageText(text))
			}
		case types.AssistantMessage:
			if text := types.TextOf(v); text != "" {
				u = acp.Ptr(acp.UpdateAgentMessageText(text))
			}
		}
		if u != nil {
			_ = s.client.SessionUpdate(ctx, acp.SessionNotification{SessionId: req.SessionId, Update: *u})
		}
	}
	return acp.LoadSessionResponse{ConfigOptions: s.configOptions(sess.Host)}, nil
}

// ResumeSession continues a live session, or restores a stored one without
// replaying it.
func (s *acpServer) ResumeSession(ctx context.Context, req acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	if sess := s.sessions.Get(string(req.SessionId)); sess != nil {
		return acp.ResumeSessionResponse{ConfigOptions: s.configOptions(sess.Host)}, nil
	}
	sess, err := s.restore(ctx, string(req.SessionId), req.Cwd, req.McpServers)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	return acp.ResumeSessionResponse{ConfigOptions: s.configOptions(sess.Host)}, nil
}

// ListSessions lists stored sessions, most recent first, optionally only
// those of one working directory.
func (s *acpServer) ListSessions(_ context.Context, req acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if s.opts.sessionsDir == "" {
		return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
	}
	entries, err := os.ReadDir(s.opts.sessionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return acp.ListSessionsResponse{}, toACPError(err)
	}
	var recs []acpSaved
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !acpSessionIDRE.MatchString(id) {
			continue
		}
		raw, err := os.ReadFile(s.savedPath(id))
		if err != nil {
			continue
		}
		var rec acpSaved
		if json.Unmarshal(raw, &rec) != nil || rec.SessionID != id {
			continue
		}
		if req.Cwd != nil && *req.Cwd != rec.Cwd {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].UpdatedAt.After(recs[j].UpdatedAt) })
	out := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
	for _, r := range recs {
		info := acp.SessionInfo{SessionId: acp.SessionId(r.SessionID), Cwd: r.Cwd, UpdatedAt: acp.Ptr(r.UpdatedAt.Format(time.RFC3339))}
		if r.Title != "" {
			info.Title = acp.Ptr(r.Title)
		}
		out.Sessions = append(out.Sessions, info)
	}
	return out, nil
}
