package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/tools"
)

// Prompt runs one turn. It streams the run's text, thinking and tool calls
// as session updates, asks the client about every approval marker, and
// answers with the stop reason. A session/cancel ends it with cancelled.
func (s *acpServer) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	sess := s.sessions.Get(string(req.SessionId))
	if sess == nil {
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown session " + string(req.SessionId)})
	}
	msg, err := acpUserMessage(req.Prompt)
	if err != nil {
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	stream, err := sess.Start(s.ctx, msg)
	if err != nil {
		return acp.PromptResponse{}, toACPError(err)
	}
	t := &acpTurn{srv: s, ctx: ctx, sess: sess, sid: req.SessionId, root: s.workspaceOf(sess),
		started: map[string]bool{}, names: map[string]string{}, args: map[string]map[string]any{}, before: map[string]*string{}}

	// session/cancel cancels ctx; the run stops and drains.
	stop := context.AfterFunc(ctx, stream.Cancel)
	defer stop()
	for d := range stream.Deltas() {
		t.handle(d)
	}
	runErr := stream.Wait()
	t.permissions.Wait()
	s.save(sess)

	switch {
	case ctx.Err() != nil || errors.Is(runErr, types.ErrStreamCanceled):
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	case errors.Is(runErr, types.ErrMaxIterations):
		return acp.PromptResponse{StopReason: acp.StopReasonMaxTurnRequests}, nil
	case runErr != nil:
		return acp.PromptResponse{}, acp.NewInternalError(map[string]any{"error": runErr.Error()})
	case t.maxTokens:
		return acp.PromptResponse{StopReason: acp.StopReasonMaxTokens}, nil
	case t.refused && !t.sawText:
		return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// workspaceOf is the directory a session's relative tool paths are under.
func (s *acpServer) workspaceOf(sess *agenthost.Session[*acpSession]) string {
	return firstNonEmpty(s.opts.workspace, sess.Host.cwd)
}

// acpUserMessage maps ACP content blocks to the parts of one user message.
// An embedded text resource becomes text headed by its URI; a blob becomes
// a media part by its MIME type; a resource link is named in text, so the
// agent can read it with its own tools, except an http(s) link to media,
// which becomes a media part by URL.
func acpUserMessage(blocks []acp.ContentBlock) (types.UserMessage, error) {
	var parts []types.UserPart
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, types.Text(b.Text.Text))
		case b.Image != nil:
			if b.Image.Data == "" && b.Image.Uri != nil {
				parts = append(parts, types.Image(types.URL(*b.Image.Uri, types.MediaType(b.Image.MimeType))))
				continue
			}
			data, err := base64.StdEncoding.DecodeString(b.Image.Data)
			if err != nil {
				return types.UserMessage{}, fmt.Errorf("image: %w", err)
			}
			parts = append(parts, types.Image(types.Bytes(types.MediaType(b.Image.MimeType), data)))
		case b.Audio != nil:
			data, err := base64.StdEncoding.DecodeString(b.Audio.Data)
			if err != nil {
				return types.UserMessage{}, fmt.Errorf("audio: %w", err)
			}
			parts = append(parts, types.Audio(types.Bytes(types.MediaType(b.Audio.MimeType), data)))
		case b.Resource != nil:
			switch r := b.Resource.Resource; {
			case r.TextResourceContents != nil:
				parts = append(parts, types.Text(fmt.Sprintf("Contents of %s:\n\n%s", r.TextResourceContents.Uri, r.TextResourceContents.Text)))
			case r.BlobResourceContents != nil:
				data, err := base64.StdEncoding.DecodeString(r.BlobResourceContents.Blob)
				if err != nil {
					return types.UserMessage{}, fmt.Errorf("resource %s: %w", r.BlobResourceContents.Uri, err)
				}
				mt := types.MediaType("application/octet-stream")
				if r.BlobResourceContents.MimeType != nil {
					mt = types.MediaType(*r.BlobResourceContents.MimeType)
				}
				src := types.Bytes(mt, data)
				src.Filename = path.Base(r.BlobResourceContents.Uri)
				src.URI = r.BlobResourceContents.Uri
				parts = append(parts, types.Media(src))
			}
		case b.ResourceLink != nil:
			l := b.ResourceLink
			mt := ""
			if l.MimeType != nil {
				mt = *l.MimeType
			}
			if (strings.HasPrefix(l.Uri, "https://") || strings.HasPrefix(l.Uri, "http://")) && mt != "" &&
				types.MediaType(mt).Modality() != types.ModalityFile && !strings.HasPrefix(mt, "text/") {
				parts = append(parts, types.Media(types.URL(l.Uri, types.MediaType(mt))))
				continue
			}
			text := "Referenced resource " + l.Name + ": " + l.Uri
			if mt != "" {
				text += " (" + mt + ")"
			}
			parts = append(parts, types.Text(text))
		}
	}
	if len(parts) == 0 {
		return types.UserMessage{}, errors.New("the prompt has no content")
	}
	return types.UserMsg(parts...), nil
}

// acpTurn maps one run's deltas to session updates.
type acpTurn struct {
	srv *acpServer
	// ctx is the prompt's: a cancel ends the permission requests it opened.
	ctx  context.Context
	sess *agenthost.Session[*acpSession]
	sid  acp.SessionId
	root string

	// started marks tool calls already announced; names and args remember
	// each call; before holds a file's content before a write, for diffs.
	started map[string]bool
	names   map[string]string
	args    map[string]map[string]any
	before  map[string]*string

	sawText, refused, maxTokens bool
	permissions                 sync.WaitGroup
}

func (t *acpTurn) update(u acp.SessionUpdate) {
	if err := t.srv.client.SessionUpdate(t.srv.ctx, acp.SessionNotification{SessionId: t.sid, Update: u}); err != nil {
		slog.Debug("saige acp: session update", "error", err)
	}
}

func (t *acpTurn) handle(d types.Delta) {
	path, inner := types.FlattenDelta(d)
	nested := len(path) > 0
	callID := func(id string) string {
		if !nested {
			return id
		}
		return strings.Join(append(append([]string(nil), path...), id), "/")
	}
	switch v := inner.(type) {
	case types.PartStart:
		if !nested && v.Kind == types.KindToolCall && v.ID != "" {
			t.startCall(v.ID, v.Name, nil)
		}
	case types.PartDelta:
		if nested {
			return
		}
		switch {
		case v.Text != "":
			t.sawText = true
			t.update(acp.UpdateAgentMessageText(v.Text))
		case v.Thinking != "":
			t.update(acp.UpdateAgentThoughtText(v.Thinking))
		case v.Refusal != "":
			t.refused = true
			t.update(acp.UpdateAgentMessageText(v.Refusal))
		}
	case types.PartEnd:
		if nested {
			return
		}
		if call, ok := v.Part.(types.ToolCallPart); ok && call.ID != "" {
			t.startCall(call.ID, call.Name, call.Arguments)
		}
	case types.TruncatedDelta:
		if !nested && v.Reason == types.FinishReasonMaxTokens {
			t.maxTokens = true
		}
	case types.ToolExecStartDelta:
		id := callID(v.ToolCallID)
		if !t.started[id] {
			t.startCall(id, v.Name, nil)
		}
		t.captureBefore(id)
		t.update(acp.UpdateToolCall(acp.ToolCallId(id), acp.WithUpdateStatus(acp.ToolCallStatusInProgress)))
	case types.ToolExecEndDelta:
		t.endCall(callID(v.ToolCallID), v)
	case types.MarkerDelta:
		// A sub-agent's marker already names its path.
		t.permissions.Add(1)
		go func() {
			defer t.permissions.Done()
			t.askPermission(v)
		}()
	}
}

// startCall announces a tool call, or adds the arguments to one already
// announced.
func (t *acpTurn) startCall(id, name string, args map[string]any) {
	if name != "" {
		t.names[id] = name
	}
	name = t.names[id]
	if args != nil {
		t.args[id] = args
	}
	title := acpToolTitle(name, t.args[id])
	locs := t.locations(t.args[id])
	if !t.started[id] {
		t.started[id] = true
		opts := []acp.ToolCallStartOpt{acp.WithStartKind(t.kindOf(name)), acp.WithStartStatus(acp.ToolCallStatusPending)}
		if args != nil {
			opts = append(opts, acp.WithStartRawInput(args))
		}
		if len(locs) > 0 {
			opts = append(opts, acp.WithStartLocations(locs))
		}
		t.update(acp.StartToolCall(acp.ToolCallId(id), title, opts...))
		return
	}
	if args == nil {
		return
	}
	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateTitle(title), acp.WithUpdateRawInput(args)}
	if len(locs) > 0 {
		opts = append(opts, acp.WithUpdateLocations(locs))
	}
	t.update(acp.UpdateToolCall(acp.ToolCallId(id), opts...))
}

// endCall reports a call's result: its text and media, and for a file
// write or edit the diff.
func (t *acpTurn) endCall(id string, v types.ToolExecEndDelta) {
	status := acp.ToolCallStatusCompleted
	if v.Error != "" {
		status = acp.ToolCallStatusFailed
	}
	var content []acp.ToolCallContent
	if diff, ok := t.diff(id); ok && status == acp.ToolCallStatusCompleted {
		content = append(content, diff)
	}
	text := v.Result
	if v.Error != "" && text == "" {
		text = v.Error
	}
	if text != "" {
		content = append(content, acp.ToolContent(acp.TextBlock(text)))
	}
	for _, p := range v.Parts {
		switch m := p.(type) {
		case types.ImagePart:
			if len(m.Source.Inline) > 0 {
				content = append(content, acp.ToolContent(acp.ImageBlock(base64.StdEncoding.EncodeToString(m.Source.Inline), string(m.Source.MediaType))))
			}
		case types.AudioPart:
			if len(m.Source.Inline) > 0 {
				content = append(content, acp.ToolContent(acp.AudioBlock(base64.StdEncoding.EncodeToString(m.Source.Inline), string(m.Source.MediaType))))
			}
		}
	}
	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status)}
	if len(content) > 0 {
		opts = append(opts, acp.WithUpdateContent(content))
	}
	if v.Result != "" || v.Error != "" {
		opts = append(opts, acp.WithUpdateRawOutput(map[string]any{"result": v.Result, "error": v.Error}))
	}
	t.update(acp.UpdateToolCall(acp.ToolCallId(id), opts...))
}

// askPermission turns an approval marker into session/request_permission.
// "Always allow" is offered when the definition's grant cap reaches the
// tool scope and the tool is not destructive (grants never cover those);
// it grants the tool for the rest of the session. A request the client
// does not answer in time is denied.
func (t *acpTurn) askPermission(m types.MarkerDelta) {
	ag := t.sess.Agent()
	always := ag.AllowsGrant(types.GrantTool) && !strings.Contains(m.ToolCallID, "/") && !t.destructive(ag, m.ToolName)
	options := []acp.PermissionOption{{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: "allow_once"}}
	if always {
		options = append(options, acp.PermissionOption{Kind: acp.PermissionOptionKindAllowAlways, Name: "Always allow " + m.ToolName + " in this session", OptionId: "allow_always"})
	}
	options = append(options, acp.PermissionOption{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: "reject_once"})

	title := acpToolTitle(m.ToolName, m.Arguments)
	if msg := preferredMessage(m.Markers); msg != "" {
		title = msg
	}
	ctx, cancel := context.WithTimeout(t.ctx, t.srv.opts.approvalTimeout)
	defer cancel()
	resp, err := t.srv.client.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: t.sid,
		Options:   options,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(m.ToolCallID),
			Title:      acp.Ptr(title),
			Kind:       acp.Ptr(t.kindOf(m.ToolName)),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			RawInput:   m.Arguments,
			Locations:  t.locations(m.Arguments),
		},
	})
	res := agentsdk.Resolution{Approver: "acp-client"}
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Message = "approval timed out with no decision; the call was denied"
	case err != nil:
		res.Message = "the approval request failed: " + err.Error()
	case resp.Outcome.Cancelled != nil || resp.Outcome.Selected == nil:
		res.Message = "the turn was cancelled before " + m.ToolName + " was approved"
	default:
		switch resp.Outcome.Selected.OptionId {
		case "allow_once":
			res.Approved = true
		case "allow_always":
			res.Approved = true
			if always {
				res.Grant = &types.GrantRequest{Scope: types.GrantTool}
			}
		default:
			res.Message = "the user rejected " + m.ToolName
		}
	}
	if err := t.sess.Decide(m.ToolCallID, res); err != nil && !errors.Is(err, agentsdk.ErrUnknownMarker) {
		slog.Warn("saige acp: deliver decision", "tool", m.ToolName, "error", err)
		if res.Approved {
			_ = t.sess.Decide(m.ToolCallID, agentsdk.Resolution{Message: "the decision could not be applied: " + err.Error()})
		}
	}
}

// preferredMessage is the message of the marker that describes the
// approval.
func preferredMessage(markers []types.Marker) string {
	for _, m := range markers {
		if mutating, _ := m.Meta["mutating"].(bool); m.Kind == "human_approval" || mutating {
			return m.Message
		}
	}
	return ""
}

func (t *acpTurn) destructive(ag agenthost.Agent, name string) bool {
	if ag.Tools == nil {
		return true
	}
	tool, ok := ag.Tools.Get(name)
	if !ok {
		return true
	}
	return tool.Definition().Capability == types.ToolCapabilityDestructive
}

// kindOf maps a tool to an ACP tool kind: harness tools by name, others by
// declared capability.
func (t *acpTurn) kindOf(name string) acp.ToolKind {
	switch name {
	case tools.ReadFileName, tools.ListDirName:
		return acp.ToolKindRead
	case tools.GlobName, tools.GrepName:
		return acp.ToolKindSearch
	case tools.WriteFileName, tools.EditFileName:
		return acp.ToolKindEdit
	case tools.ExecuteCodeName:
		return acp.ToolKindExecute
	case tools.FetchURLName:
		return acp.ToolKindFetch
	}
	if strings.HasPrefix(name, "delegate_to_") || strings.HasPrefix(name, "spawn_") {
		return acp.ToolKindThink
	}
	if reg := t.sess.Agent().Tools; reg != nil {
		if tool, ok := reg.Get(name); ok {
			switch tool.Definition().Capability {
			case types.ToolCapabilityRead:
				return acp.ToolKindRead
			case types.ToolCapabilityWrite:
				return acp.ToolKindEdit
			case types.ToolCapabilityDestructive:
				return acp.ToolKindDelete
			}
		}
	}
	return acp.ToolKindOther
}

// acpToolTitle names a call by its tool and main argument.
func acpToolTitle(name string, args map[string]any) string {
	for _, key := range []string{"path", "pattern", "url", "query", "command", "code", "task"} {
		if v, ok := args[key].(string); ok && v != "" {
			v, _, _ = strings.Cut(v, "\n")
			return name + ": " + oneLine(v, 80)
		}
	}
	return name
}

// locations returns the file a call touches, as an absolute path.
func (t *acpTurn) locations(args map[string]any) []acp.ToolCallLocation {
	p, ok := args["path"].(string)
	if !ok || p == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.root, p)
	}
	return []acp.ToolCallLocation{{Path: p}}
}

// captureBefore reads the file a write is about to replace, so the result
// can carry a diff. It reads only inside the workspace.
func (t *acpTurn) captureBefore(id string) {
	if t.names[id] != tools.WriteFileName {
		return
	}
	p, _ := t.args[id]["path"].(string)
	if p == "" || t.root == "" {
		return
	}
	root, err := os.OpenRoot(t.root)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	rel := p
	if filepath.IsAbs(p) {
		if rel, err = filepath.Rel(t.root, p); err != nil {
			return
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		t.before[id] = nil
		return
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err == nil {
		s := string(raw)
		t.before[id] = &s
	}
}

// diff returns the diff of a completed write or edit.
func (t *acpTurn) diff(id string) (acp.ToolCallContent, bool) {
	args := t.args[id]
	locs := t.locations(args)
	if len(locs) == 0 {
		return acp.ToolCallContent{}, false
	}
	switch t.names[id] {
	case tools.WriteFileName:
		content, ok := args["content"].(string)
		if !ok {
			return acp.ToolCallContent{}, false
		}
		if old := t.before[id]; old != nil {
			return acp.ToolDiffContent(locs[0].Path, content, *old), true
		}
		return acp.ToolDiffContent(locs[0].Path, content), true
	case tools.EditFileName:
		oldText, _ := args["old_string"].(string)
		newText, ok := args["new_string"].(string)
		if !ok {
			return acp.ToolCallContent{}, false
		}
		return acp.ToolDiffContent(locs[0].Path, newText, oldText), true
	}
	return acp.ToolCallContent{}, false
}
