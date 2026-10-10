package tree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// This file reads the version 1 node message format, which releases before
// typed parts wrote: {"content":[{"type":<tag>,"data":<content>}, ...]}.
// Trees, Postgres rows and WAL files written by those releases hold it, and
// they are migrated lazily, on read. Keep it indefinitely and never change
// the stored shapes below. Nothing writes this format any more.

// The version 1 type tags.
const (
	v1Text       = "text"
	v1ToolUse    = "tool_use"
	v1ToolResult = "tool_result"
	v1Thinking   = "thinking"
	v1File       = "file"
	v1ServerTool = "server_tool"
	v1Config     = "config"
	v1Handoff    = "handoff"
	v1Feedback   = "feedback"
	v1Steer      = "steer"
	v1Truncation = "truncation"
	v1Route      = "route"
	v1Approval   = "approval"
	v1Guardrail  = "guardrail"
	v1Compaction = "compaction"
	// v1Part tags a part in the shared part codec form. Development builds
	// between the two formats wrote it for parts with no version 1 tag.
	v1Part = "part"
)

// v1Envelope is one tagged content block.
type v1Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// v1Message is a version 1 node message.
type v1Message struct {
	Content []v1Envelope `json:"content"`
}

// The version 1 content shapes. Field names and tags are the stored format.
type (
	v1TextContent struct {
		Text string
	}
	v1ToolUseContent struct {
		ID             string
		Name           string
		Arguments      map[string]any
		ArgumentsError string
	}
	v1ThinkingContent struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	v1Block struct {
		Kind      string          `json:"kind"`
		Text      string          `json:"text,omitempty"`
		MediaType types.MediaType `json:"media_type,omitempty"`
		URI       string          `json:"uri,omitempty"`
		Filename  string          `json:"filename,omitempty"`
		JSON      json.RawMessage `json:"json,omitempty"`
	}
	v1ToolResultContent struct {
		ToolCallID  string
		Text        string
		IsError     bool
		Blocks      []v1Block        `json:"blocks,omitempty"`
		Citations   []types.Citation `json:"citations,omitempty"`
		ToolVersion string           `json:"tool_version,omitempty"`
	}
	v1FileContent struct {
		URI       string          `json:"uri"`
		MediaType types.MediaType `json:"media_type,omitempty"`
		Filename  string          `json:"filename,omitempty"`
	}
	v1ServerToolContent struct {
		ID     string               `json:"id"`
		Kind   types.ServerToolKind `json:"kind"`
		Name   string               `json:"name,omitempty"`
		Input  map[string]any       `json:"input,omitempty"`
		Text   string               `json:"text,omitempty"`
		Result json.RawMessage      `json:"result,omitempty"`
		Files  []v1FileContent      `json:"files,omitempty"`
	}
)

// errUnknownContent reports a stored type tag this release does not read.
var errUnknownContent = errors.New("unknown content type")

// unmarshalV1Message reads a version 1 node message.
func unmarshalV1Message(role types.Role, data json.RawMessage) (types.Message, error) {
	var env v1Message
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	switch role {
	case types.RoleSystem:
		parts, err := v1Parts[types.SystemPart](role, env.Content)
		return types.SystemMessage{Parts: parts}, err
	case types.RoleUser:
		parts, err := v1Parts[types.UserPart](role, env.Content)
		return types.UserMessage{Parts: parts}, err
	case types.RoleAssistant:
		parts, err := v1Parts[types.AssistantPart](role, env.Content)
		return types.AssistantMessage{Parts: parts}, err
	default:
		return nil, fmt.Errorf("unknown role: %s", role)
	}
}

func v1Parts[T types.Part](role types.Role, content []v1Envelope) ([]T, error) {
	var out []T
	for _, ce := range content {
		ps, err := v1Content(ce)
		if errors.Is(err, errUnknownContent) {
			return nil, fmt.Errorf("unknown %s content type: %s", role, ce.Type)
		}
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			v, ok := p.(T)
			if !ok {
				return nil, fmt.Errorf("unknown %s content type: %s", role, ce.Type)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func decodeNumbers(data json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// v1Content maps one version 1 block to the parts it means. A server tool
// block holds a call and its result, so it maps to two parts.
//
//nolint:gocyclo // one case per stored kind
func v1Content(ce v1Envelope) ([]types.Part, error) {
	one := func(p types.Part, err error) ([]types.Part, error) {
		if err != nil {
			return nil, err
		}
		return []types.Part{p}, nil
	}
	switch ce.Type {
	case v1Part:
		return one(types.UnmarshalPart(ce.Data))
	case v1Text:
		var c v1TextContent
		err := json.Unmarshal(ce.Data, &c)
		return one(types.TextPart{Text: c.Text}, err)
	case v1ToolUse:
		var c v1ToolUseContent
		err := decodeNumbers(ce.Data, &c)
		return one(types.ToolCallPart(c), err)
	case v1Thinking:
		var c v1ThinkingContent
		err := json.Unmarshal(ce.Data, &c)
		return one(types.ThinkingPart{Text: c.Thinking, Signature: c.Signature}, err)
	case v1ToolResult:
		var c v1ToolResultContent
		if err := json.Unmarshal(ce.Data, &c); err != nil {
			return nil, err
		}
		return one(upgradeV1ToolResult(c), nil)
	case v1File:
		var c v1FileContent
		err := json.Unmarshal(ce.Data, &c)
		return one(types.Media(c.source()), err)
	case v1ServerTool:
		var c v1ServerToolContent
		if err := decodeNumbers(ce.Data, &c); err != nil {
			return nil, err
		}
		return upgradeV1ServerTool(c), nil
	case v1Config:
		return v1Metadata[types.ConfigPart](ce.Data)
	case v1Handoff:
		return v1Metadata[types.HandoffPart](ce.Data)
	case v1Feedback:
		return v1Metadata[types.FeedbackPart](ce.Data)
	case v1Steer:
		return v1Metadata[types.SteerPart](ce.Data)
	case v1Truncation:
		return v1Metadata[types.TruncationPart](ce.Data)
	case v1Route:
		return v1Metadata[types.RoutePart](ce.Data)
	case v1Approval:
		return v1Metadata[types.ApprovalPart](ce.Data)
	case v1Guardrail:
		return v1Metadata[types.GuardrailPart](ce.Data)
	case v1Compaction:
		return v1Metadata[types.CompactionPart](ce.Data)
	default:
		return nil, errUnknownContent
	}
}

// v1Metadata reads a metadata block. The metadata parts kept their version
// 1 JSON form, and their decoders accept the older field names (a config's
// "Model", a route's options).
func v1Metadata[T types.Part](data json.RawMessage) ([]types.Part, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return []types.Part{v}, nil
}

// source is the attachment's source. Its bytes were never stored, so a
// file with no URI is elided.
func (f v1FileContent) source() types.Source {
	return types.Source{URI: f.URI, MediaType: f.MediaType, Filename: f.Filename}
}

// upgradeV1ServerTool maps a server tool block to its call and, when the block
// recorded an outcome, its result.
func upgradeV1ServerTool(c v1ServerToolContent) []types.Part {
	out := []types.Part{types.ServerToolCallPart{ID: c.ID, ToolKind: c.Kind, Name: c.Name, Input: c.Input}}
	if c.Text == "" && len(c.Result) == 0 && len(c.Files) == 0 {
		return out
	}
	r := types.ServerToolResultPart{CallID: c.ID, ToolKind: c.Kind, Text: c.Text, Result: c.Result}
	for _, f := range c.Files {
		r.Outputs = append(r.Outputs, types.Media(f.source()))
	}
	return append(out, r)
}

// upgradeV1ToolResult maps a tool result: its text, then its blocks as parts.
// The text is the blocks' projection, so it is left out when a text or JSON
// block already carries it. A media block keeps its URI; its bytes were
// never stored, so one with no URI is elided and adapters reject it.
func upgradeV1ToolResult(c v1ToolResultContent) types.ToolResultPart {
	r := types.ToolResultPart{CallID: c.ToolCallID, IsError: c.IsError, Citations: c.Citations, ToolVersion: c.ToolVersion}
	if len(c.Blocks) == 0 {
		r.Parts = []types.ToolOutputPart{types.Text(c.Text)}
		return r
	}
	hasText := false
	for _, b := range c.Blocks {
		if b.Kind == "text" || b.Kind == "json" {
			hasText = true
		}
	}
	if !hasText && c.Text != "" {
		r.Parts = append(r.Parts, types.Text(c.Text))
	}
	for _, b := range c.Blocks {
		switch b.Kind {
		case "text":
			r.Parts = append(r.Parts, types.Text(b.Text))
		case "json":
			r.Parts = append(r.Parts, types.JSONPart{JSON: b.JSON})
		default:
			r.Parts = append(r.Parts, v1BlockMedia(b.Kind, types.Source{URI: b.URI, MediaType: b.MediaType, Filename: b.Filename}))
		}
	}
	return r
}

// v1BlockMedia is the tool output part of a media block. An "image" block
// is an image whatever its media type; a "file" block is classified by its
// media type, and kept as a file when that kind cannot be tool output.
func v1BlockMedia(kind string, src types.Source) types.ToolOutputPart {
	if kind == "image" {
		return types.Image(src)
	}
	if p, ok := types.Media(src).(types.ToolOutputPart); ok {
		return p
	}
	return types.File(src)
}
