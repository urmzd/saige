package durablecodec

import (
	"encoding/gob"
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// This file reads the records releases before typed parts wrote (v0.3x).
// Those stored messages as gob interface values, which gob names by Go
// type: "github.com/urmzd/saige/agent/types.TextContent" and so on. The
// types below are frozen copies of the structs those names meant,
// registered under the old names, so a journal from an in-flight run
// decodes and is upgraded to parts on replay. Keep them indefinitely and
// never change their field names or types: gob matches fields by name.
// Nested types that did not change (citations, grants, options) are the
// current ones.

const legacyPkg = "github.com/urmzd/saige/agent/types."

func init() {
	gob.RegisterName(legacyPkg+"SystemMessage", legacySystemMessage{})
	gob.RegisterName(legacyPkg+"UserMessage", legacyUserMessage{})
	gob.RegisterName(legacyPkg+"AssistantMessage", legacyAssistantMessage{})
	gob.RegisterName(legacyPkg+"TextContent", legacyText{})
	gob.RegisterName(legacyPkg+"ToolUseContent", legacyToolUse{})
	gob.RegisterName(legacyPkg+"ThinkingContent", legacyThinking{})
	gob.RegisterName(legacyPkg+"ToolResultContent", legacyToolResult{})
	gob.RegisterName(legacyPkg+"FileContent", legacyFile{})
	gob.RegisterName(legacyPkg+"ServerToolContent", legacyServerTool{})
	gob.RegisterName(legacyPkg+"ConfigContent", legacyConfig{})
	gob.RegisterName(legacyPkg+"FeedbackContent", legacyFeedback{})
	gob.RegisterName(legacyPkg+"HandoffContent", legacyHandoff{})
	gob.RegisterName(legacyPkg+"SteerContent", legacySteer{})
	gob.RegisterName(legacyPkg+"TruncationContent", legacyTruncation{})
	gob.RegisterName(legacyPkg+"RouteContent", legacyRoute{})
	gob.RegisterName(legacyPkg+"ApprovalContent", legacyApproval{})
	gob.RegisterName(legacyPkg+"GuardrailContent", legacyGuardrail{})
	gob.RegisterName(legacyPkg+"CompactionContent", legacyCompaction{})
}

// legacyMessage is a message in the form an earlier release wrote.
type legacyMessage interface {
	message() (types.Message, error)
}

// legacyContent is a content block in the form an earlier release wrote.
// parts returns the parts it means: one, or two for a server tool block,
// which held a call and its result.
type legacyContent interface {
	parts() []types.Part
}

type (
	legacySystemMessage    struct{ Content []legacyContent }
	legacyUserMessage      struct{ Content []legacyContent }
	legacyAssistantMessage struct{ Content []legacyContent }
)

func (m legacySystemMessage) message() (types.Message, error) {
	parts, err := legacyRoleParts[types.SystemPart](types.RoleSystem, m.Content)
	return types.SystemMessage{Parts: parts}, err
}

func (m legacyUserMessage) message() (types.Message, error) {
	parts, err := legacyRoleParts[types.UserPart](types.RoleUser, m.Content)
	return types.UserMessage{Parts: parts}, err
}

func (m legacyAssistantMessage) message() (types.Message, error) {
	parts, err := legacyRoleParts[types.AssistantPart](types.RoleAssistant, m.Content)
	return types.AssistantMessage{Parts: parts}, err
}

func legacyRoleParts[T types.Part](role types.Role, content []legacyContent) ([]T, error) {
	var out []T
	for _, c := range content {
		if c == nil {
			continue
		}
		for _, p := range c.parts() {
			v, ok := p.(T)
			if !ok {
				return nil, fmt.Errorf("durablecodec: %s message holds %s content", role, p.Kind())
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// decodeLegacyMessages reads a run input an earlier release wrote: a bare
// []types.Message.
func decodeLegacyMessages(raw []byte) ([]types.Message, error) {
	var in []legacyMessage
	if err := gobDecode(raw, &in); err != nil {
		return nil, err
	}
	var out []types.Message
	for i, m := range in {
		if m == nil {
			return nil, fmt.Errorf("durablecodec: legacy message %d is nil", i)
		}
		msg, err := m.message()
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

// legacyResult upgrades a step an earlier release recorded.
func (r stepRecord) legacyResult(out types.StepResult) (types.StepResult, error) {
	out.V = 1
	if r.Message != nil {
		msg, err := r.Message.message()
		if err != nil {
			return out, err
		}
		m := msg.(types.AssistantMessage)
		out.Message = &m
	}
	for _, b := range r.ToolBlocks {
		out.ToolParts = append(out.ToolParts, b.part())
	}
	if h := r.Hook; h != nil {
		hr := h.common()
		if h.Message != nil {
			msg, err := h.Message.message()
			if err != nil {
				return out, err
			}
			m := msg.(types.UserMessage)
			hr.Message = &m
		}
		out.Hook = hr
	}
	return out, nil
}

// ── Content blocks, as v0.3x defined them ────────────────────────────

type legacyText struct {
	Text string
}

type legacyToolUse struct {
	ID             string
	Name           string
	Arguments      map[string]any
	ArgumentsError string
}

type legacyThinking struct {
	Thinking  string
	Signature string
}

type legacyBlock struct {
	Kind      string
	Text      string
	MediaType types.MediaType
	URI       string
	Filename  string
	Data      []byte
	JSON      json.RawMessage
}

type legacyToolResult struct {
	ToolCallID  string
	Text        string
	IsError     bool
	Blocks      []legacyBlock
	Citations   []types.Citation
	ToolVersion string
}

type legacyFile struct {
	URI       string
	MediaType types.MediaType
	Data      []byte
	Filename  string
}

type legacyServerTool struct {
	ID     string
	Kind   types.ServerToolKind
	Name   string
	Input  map[string]any
	Text   string
	Result json.RawMessage
	Files  []legacyFile
}

type legacyConfig struct {
	Model      string
	MaxIter    int
	Compact    *types.CompactConfig
	CompactNow bool
	ToolChoice *types.ToolChoice
	Dials      *types.Dials
	Reason     string
}

type legacyFeedback struct {
	TargetNodeID string
	Rating       types.Rating
	Comment      string
}

type legacyHandoff struct {
	To      string
	From    string
	Reason  string
	Message string
	Context string
}

type legacySteer struct {
	ID string
}

type legacyTruncation struct {
	Reason string
}

type legacyRoute struct {
	Profile         string
	Provider        string
	Model           string
	Experiment      string
	Variant         string
	Reason          string
	Preset          string
	ConfigHash      string
	CatalogRevision string
	Options         *types.RequestOptions
	Dials           *types.DialReport
}

type legacyApproval struct {
	Event      types.ApprovalEvent
	Tool       string
	ToolCallID string
	Grant      *types.Grant
	GrantID    string
	Approver   string
	Reason     string
	Grants     []types.Grant
	Approvals  map[string]int
	Denials    map[string]int
}

type legacyGuardrail struct {
	Guardrail string
	Phase     string
	Action    string
	Reason    string
	Canceled  bool
}

type legacyCompaction struct {
	Strategy     string
	Steps        []string
	Trigger      types.CompactionTrigger
	TokensBefore int
	TokensAfter  int
	FromBranch   types.BranchID
	Kept         []types.NodeID
	Selected     []types.NodeID
	Cleared      []types.NodeID
	Summarized   []types.NodeID
	Dropped      []types.NodeID
	SummaryNode  types.NodeID
}

// ── Upgrades ─────────────────────────────────────────────────────────

func (c legacyText) parts() []types.Part { return []types.Part{types.TextPart{Text: c.Text}} }

func (c legacyToolUse) parts() []types.Part {
	return []types.Part{types.ToolCallPart{ID: c.ID, Name: c.Name, Arguments: c.Arguments, ArgumentsError: c.ArgumentsError}}
}

func (c legacyThinking) parts() []types.Part {
	return []types.Part{types.ThinkingPart{Text: c.Thinking, Signature: c.Signature}}
}

// source keeps the attachment's bytes, which a journal held, with its
// digest, and its URI.
func legacySource(mt types.MediaType, uri, filename string, data []byte) types.Source {
	src := types.Source{MediaType: mt, URI: uri, Filename: filename}
	if len(data) > 0 {
		b := types.Bytes(mt, data)
		src.Inline, src.Digest, src.Size = b.Inline, b.Digest, b.Size
	}
	return src
}

func (c legacyFile) parts() []types.Part {
	return []types.Part{types.Media(legacySource(c.MediaType, c.URI, c.Filename, c.Data))}
}

// part maps a tool output block. An "image" block is an image whatever its
// media type; a "file" block is classified by its media type, and kept as
// a file when that kind cannot be tool output.
func (b legacyBlock) part() types.ToolOutputPart {
	switch b.Kind {
	case "text":
		return types.Text(b.Text)
	case "json":
		return types.JSONPart{JSON: b.JSON}
	case "image":
		return types.Image(legacySource(b.MediaType, b.URI, b.Filename, b.Data))
	}
	src := legacySource(b.MediaType, b.URI, b.Filename, b.Data)
	if p, ok := types.Media(src).(types.ToolOutputPart); ok {
		return p
	}
	return types.File(src)
}

// parts maps a tool result: its text, then its blocks. The text is the
// blocks' projection, so it is left out when a text or JSON block already
// carries it.
func (c legacyToolResult) parts() []types.Part {
	r := types.ToolResultPart{CallID: c.ToolCallID, IsError: c.IsError, Citations: c.Citations, ToolVersion: c.ToolVersion}
	hasText := false
	for _, b := range c.Blocks {
		if b.Kind == "text" || b.Kind == "json" {
			hasText = true
		}
	}
	if len(c.Blocks) == 0 || (!hasText && c.Text != "") {
		r.Parts = append(r.Parts, types.Text(c.Text))
	}
	for _, b := range c.Blocks {
		r.Parts = append(r.Parts, b.part())
	}
	return []types.Part{r}
}

func (c legacyServerTool) parts() []types.Part {
	out := []types.Part{types.ServerToolCallPart{ID: c.ID, ToolKind: c.Kind, Name: c.Name, Input: c.Input}}
	if c.Text == "" && len(c.Result) == 0 && len(c.Files) == 0 {
		return out
	}
	r := types.ServerToolResultPart{CallID: c.ID, ToolKind: c.Kind, Text: c.Text, Result: c.Result}
	for _, f := range c.Files {
		r.Outputs = append(r.Outputs, f.parts()[0])
	}
	return append(out, r)
}

func (c legacyConfig) parts() []types.Part {
	p := types.ConfigPart{MaxIter: c.MaxIter, Compact: c.Compact, CompactNow: c.CompactNow, ToolChoice: c.ToolChoice,
		Dials: c.Dials, Reason: c.Reason}
	if c.Model != "" {
		p.Target = types.ModelTarget(types.ModelID(c.Model))
	}
	return []types.Part{p}
}

func (c legacyFeedback) parts() []types.Part {
	return []types.Part{types.FeedbackPart{TargetNodeID: c.TargetNodeID, Rating: c.Rating, Comment: c.Comment}}
}

func (c legacyHandoff) parts() []types.Part {
	return []types.Part{types.HandoffPart{To: c.To, From: c.From, Reason: c.Reason, Message: c.Message, Context: c.Context}}
}

func (c legacySteer) parts() []types.Part { return []types.Part{types.SteerPart{ID: c.ID}} }

func (c legacyTruncation) parts() []types.Part {
	return []types.Part{types.TruncationPart{Reason: c.Reason}}
}

func (c legacyRoute) parts() []types.Part {
	return []types.Part{types.RoutePart{Profile: c.Profile, Provider: c.Provider, Model: c.Model, Experiment: c.Experiment,
		Variant: c.Variant, Reason: c.Reason, Preset: c.Preset, ConfigHash: c.ConfigHash, CatalogRevision: c.CatalogRevision,
		Options: c.Options, Dials: c.Dials}}
}

func (c legacyApproval) parts() []types.Part {
	return []types.Part{types.ApprovalPart{Event: c.Event, Tool: c.Tool, ToolCallID: c.ToolCallID, Grant: c.Grant, GrantID: c.GrantID,
		Approver: c.Approver, Reason: c.Reason, Grants: c.Grants, Approvals: c.Approvals, Denials: c.Denials}}
}

func (c legacyGuardrail) parts() []types.Part {
	return []types.Part{types.GuardrailPart{Guardrail: c.Guardrail, Phase: c.Phase, Action: c.Action, Reason: c.Reason, Canceled: c.Canceled}}
}

func (c legacyCompaction) parts() []types.Part {
	return []types.Part{types.CompactionPart{Strategy: c.Strategy, Steps: c.Steps, Trigger: c.Trigger, TokensBefore: c.TokensBefore,
		TokensAfter: c.TokensAfter, FromBranch: c.FromBranch, Kept: c.Kept, Selected: c.Selected, Cleared: c.Cleared,
		Summarized: c.Summarized, Dropped: c.Dropped, SummaryNode: c.SummaryNode}}
}
