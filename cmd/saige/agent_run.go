package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/tui"
	"github.com/urmzd/saige/agent/types"
)

const (
	traceProvider = "provider"
	traceModel    = "model"
	traceName     = "name"
	traceTools    = "tools"
)

type agentRunOptions struct {
	sessionID, sessionsDir, identity, definition string
	trace                                        bool
	diagnostics                                  io.Writer
}

func (o agentRunOptions) event(kind string, details any) {
	if o.trace {
		_ = json.NewEncoder(o.diagnostics).Encode(struct {
			Event   string `json:"event"`
			Details any    `json:"details"`
		}{kind, details})
	}
}

var sessionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// A session lock spans restore, execution and atomic replacement, so competing
// CLI processes cannot silently overwrite a conversation's next turn.
func openRunSession(o agentRunOptions) (string, func(), error) {
	if !sessionName.MatchString(o.sessionID) {
		return "", nil, fmt.Errorf("invalid session ID %q: use 1-128 letters, digits, hyphens or underscores, starting with a letter or digit", o.sessionID)
	}
	dir := o.sessionsDir
	if dir == "" {
		config, err := os.UserConfigDir()
		if err != nil {
			return "", nil, err
		}
		dir = filepath.Join(config, "saige", "sessions")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, o.sessionID+".json")
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // validated session name under the explicitly selected session directory
	if err != nil {
		return "", nil, fmt.Errorf("lock session %s (another process may be using it): %w", o.sessionID, err)
	}
	if err := lock.Close(); err != nil {
		_ = os.Remove(path + ".lock")
		return "", nil, err
	}
	return path, func() { _ = os.Remove(path + ".lock") }, nil
}

func saveRunSession(path string, session *agentsdk.Session) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".session-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := json.NewEncoder(f).Encode(session); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func runAgentTurn(ctx context.Context, a *agentsdk.Agent, question string, out tui.Output, allow bool, o agentRunOptions) error {
	info := a.Info()
	o.event("agent_loaded", map[string]any{"definition": o.definition, "digest": o.identity, traceProvider: info.Provider, traceTools: info.Tools, "sub_agents": info.SubAgents})
	var path string
	var previous *agentsdk.Session
	if o.sessionID != "" {
		var unlock func()
		var err error
		path, unlock, err = openRunSession(o)
		if err != nil {
			return err
		}
		defer unlock()
		previous, err = agentsdk.LoadSessionFromFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if previous.ID != o.sessionID || previous.Metadata["cli_identity"] != o.identity {
				return errors.New("session belongs to a different agent definition or digest; use a new --session")
			}
			if err := a.LoadSession(previous); err != nil {
				return err
			}
		}
		messages, err := a.Tree().FlattenBranch(a.Tree().Active())
		if err != nil {
			return err
		}
		o.event("session_loaded", map[string]any{"id": o.sessionID, "restored": previous != nil, "messages": len(messages)})
	}
	err := runAskTraced(ctx, a, question, out, allow, o)
	if path != "" {
		session, saveErr := a.SaveSession()
		if saveErr == nil {
			session.ID = o.sessionID
			session.Metadata["cli_identity"] = o.identity
			if previous != nil {
				session.CreatedAt = previous.CreatedAt
			}
			session.UpdatedAt = time.Now()
			saveErr = saveRunSession(path, session)
		}
		if saveErr != nil {
			return errors.Join(err, fmt.Errorf("save session: %w", saveErr))
		}
		o.event("session_saved", map[string]any{"id": o.sessionID})
	}
	return err
}

func runAskTraced(ctx context.Context, a *agentsdk.Agent, question string, out tui.Output, allow bool, o agentRunOptions) error {
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text(question))})
	deltas := stream.Deltas()
	if o.trace {
		forwarded := make(chan types.Delta)
		go func() {
			defer close(forwarded)
			for d := range deltas {
				switch v := d.(type) {
				case types.RouteDelta:
					o.event(traceProvider, map[string]any{traceProvider: v.Provider, traceModel: v.Model, "profile": v.Profile, "reason": v.Reason})
				case types.ToolExecStartDelta:
					o.event("tool_started", map[string]any{"id": v.ToolCallID, traceName: v.Name})
				case types.ToolExecEndDelta:
					o.event("tool_finished", map[string]any{"id": v.ToolCallID, traceName: v.Name, "failed": v.Error != "", "citations": len(v.Citations)})
				case types.CitationDelta:
					o.event("citation", map[string]any{"tool_call_id": v.ToolCallID})
				}
				forwarded <- d
			}
		}()
		result := tui.StreamDeltasResolving(out, tui.AgentHeader{}, forwarded, func(d types.MarkerDelta) {
			if allow {
				stream.ResolveMarker(d.ToolCallID, true, nil)
			} else {
				stream.ResolveMarkerWithMessage(d.ToolCallID, false, nil, deniedByFlag)
			}
		})
		return result.Err
	}
	result := tui.StreamDeltasResolving(out, tui.AgentHeader{}, deltas, func(d types.MarkerDelta) {
		if allow {
			stream.ResolveMarker(d.ToolCallID, true, nil)
		} else {
			stream.ResolveMarkerWithMessage(d.ToolCallID, false, nil, deniedByFlag)
		}
	})
	return result.Err
}
