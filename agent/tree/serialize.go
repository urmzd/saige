package tree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Content type constants used in serialization envelopes.
const (
	contentTypeText       = "text"
	contentTypeToolResult = "tool_result"
	contentTypeConfig     = "config"
	contentTypeThinking   = "thinking"
	contentTypeHandoff    = "handoff"
	contentTypeServerTool = "server_tool"
	contentTypeSteer      = "steer"
	contentTypeTruncation = "truncation"
	contentTypeRoute      = "route"
	contentTypeApproval   = "approval"
	contentTypeGuardrail  = "guardrail"
	contentTypeCompaction = "compaction"
	contentTypeFile       = "file"
	contentTypeUnknown    = "unknown"
)

// TreeFormatVersion is the serialized tree format this package writes.
// UnmarshalJSON rejects a document with a higher version instead of guessing
// at its meaning. A document without a version predates the field and is
// read as version 1.
const TreeFormatVersion = 1

// serializedTree is the JSON wire format for a Tree.
type serializedTree struct {
	V           int                             `json:"v,omitempty"`
	Metadata    json.RawMessage                 `json:"metadata,omitempty"`
	Nodes       []serializedNode                `json:"nodes"`
	Children    map[string][]string             `json:"children"`
	RootID      string                          `json:"root_id"`
	Branches    map[string]string               `json:"branches"`
	Active      string                          `json:"active"`
	Checkpoints map[string]serializedCheckpoint `json:"checkpoints,omitempty"`
}

type serializedNode struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parent_id,omitempty"`
	Role       string          `json:"role"`
	Message    json.RawMessage `json:"message"`
	State      int             `json:"state"`
	Version    uint64          `json:"version"`
	Depth      int             `json:"depth"`
	BranchID   string          `json:"branch_id"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	ArchivedAt *time.Time      `json:"archived_at,omitempty"`
	ArchivedBy string          `json:"archived_by,omitempty"`
	SummaryOf  []string        `json:"summary_of,omitempty"`
}

type serializedCheckpoint struct {
	ID        string    `json:"id"`
	Branch    string    `json:"branch"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// contentEnvelope wraps a part with its type tag for JSON round-tripping.
type contentEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// messageEnvelope wraps a message's parts with type tags.
type messageEnvelope struct {
	Content []contentEnvelope `json:"content"`
}

// contentTypePart tags a part stored in the shared part codec form
// (types.MarshalPart). Parts with a v1 tag keep their v1 form, so stores
// written before typed parts and stores written after read the same way.
const contentTypePart = "part"

// MarshalMessage serializes a Message to its JSON envelope representation.
func MarshalMessage(msg types.Message) (json.RawMessage, error) {
	return marshalMessage(msg)
}

// UnmarshalMessage deserializes a Message from its role and JSON envelope.
func UnmarshalMessage(role types.Role, data json.RawMessage) (types.Message, error) {
	return unmarshalMessage(role, data)
}

func marshalMessage(msg types.Message) (json.RawMessage, error) {
	var env messageEnvelope
	for _, p := range types.PartsOf(msg) {
		ce, err := marshalPart(p)
		if err != nil {
			return nil, err
		}
		env.Content = append(env.Content, ce)
	}
	return json.Marshal(env)
}

// The v1 forms of the parts that had one. Field names and tags are the
// stored format: never change them.
type (
	legacyText struct {
		Text string
	}
	legacyToolUse struct {
		ID             string
		Name           string
		Arguments      map[string]any
		ArgumentsError string
	}
	legacyThinking struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	legacyBlock struct {
		Kind      string          `json:"kind"`
		Text      string          `json:"text,omitempty"`
		MediaType types.MediaType `json:"media_type,omitempty"`
		URI       string          `json:"uri,omitempty"`
		Filename  string          `json:"filename,omitempty"`
		JSON      json.RawMessage `json:"json,omitempty"`
	}
	legacyToolResult struct {
		ToolCallID  string
		Text        string
		IsError     bool
		Blocks      []legacyBlock    `json:"blocks,omitempty"`
		Citations   []types.Citation `json:"citations,omitempty"`
		ToolVersion string           `json:"tool_version,omitempty"`
	}
	legacyFile struct {
		URI       string          `json:"uri"`
		MediaType types.MediaType `json:"media_type,omitempty"`
		Filename  string          `json:"filename,omitempty"`
	}
	legacyServerTool struct {
		ID     string               `json:"id"`
		Kind   types.ServerToolKind `json:"kind"`
		Name   string               `json:"name,omitempty"`
		Input  map[string]any       `json:"input,omitempty"`
		Text   string               `json:"text,omitempty"`
		Result json.RawMessage      `json:"result,omitempty"`
		Files  []legacyFile         `json:"files,omitempty"`
	}
)

//nolint:gocyclo // one case per part kind
func marshalPart(p types.Part) (contentEnvelope, error) {
	var tag string
	var v any
	switch x := p.(type) {
	case types.TextPart:
		tag, v = contentTypeText, legacyText{Text: x.Text}
	case types.ToolCallPart:
		tag, v = "tool_use", legacyToolUse(x)
	case types.ThinkingPart:
		if !x.Redacted && !x.Summary {
			tag, v = contentTypeThinking, legacyThinking{Thinking: x.Text, Signature: x.Signature}
		}
	case types.ToolResultPart:
		if blocks, ok := legacyBlocks(x.Parts); ok {
			tag, v = contentTypeToolResult, legacyToolResult{ToolCallID: x.CallID, Text: x.Text(), IsError: x.IsError,
				Blocks: blocks, Citations: x.Citations, ToolVersion: x.ToolVersion}
		}
	case types.ImagePart, types.AudioPart, types.VideoPart, types.DocumentPart, types.FilePart:
		if f, ok := legacyFileOf(p); ok {
			tag, v = contentTypeFile, f
		}
	case types.ConfigPart:
		tag, v = contentTypeConfig, x
	case types.HandoffPart:
		tag, v = contentTypeHandoff, x
	case types.FeedbackPart:
		tag, v = "feedback", x
	case types.SteerPart:
		tag, v = contentTypeSteer, x
	case types.TruncationPart:
		tag, v = contentTypeTruncation, x
	case types.RoutePart:
		tag, v = contentTypeRoute, x
	case types.ApprovalPart:
		tag, v = contentTypeApproval, x
	case types.GuardrailPart:
		tag, v = contentTypeGuardrail, x
	case types.CompactionPart:
		tag, v = contentTypeCompaction, x
	}
	if tag == "" {
		data, err := types.MarshalPart(p)
		return contentEnvelope{Type: contentTypePart, Data: data}, err
	}
	data, err := json.Marshal(v)
	return contentEnvelope{Type: tag, Data: data}, err
}

// legacyBlocks returns the v1 blocks for tool output. Text alone has none.
// It reports false for output the v1 form cannot hold.
func legacyBlocks(parts []types.ToolOutputPart) ([]legacyBlock, bool) {
	plain := true
	for _, p := range parts {
		if _, ok := p.(types.TextPart); !ok {
			plain = false
		}
	}
	if plain {
		return nil, len(parts) <= 1
	}
	out := make([]legacyBlock, 0, len(parts))
	for _, p := range parts {
		switch x := p.(type) {
		case types.TextPart:
			out = append(out, legacyBlock{Kind: "text", Text: x.Text})
		case types.JSONPart:
			out = append(out, legacyBlock{Kind: "json", JSON: x.JSON})
		default:
			f, ok := legacyFileOf(p)
			if !ok {
				return nil, false
			}
			kind := "file"
			if _, img := p.(types.ImagePart); img {
				kind = "image"
			} else if types.Media(types.Source{MediaType: f.MediaType}).Kind() != p.Kind() {
				return nil, false
			}
			out = append(out, legacyBlock{Kind: kind, MediaType: f.MediaType, URI: f.URI, Filename: f.Filename})
		}
	}
	return out, true
}

// legacyFileOf returns the v1 attachment form of a media part, when the
// part carries nothing that form would lose.
func legacyFileOf(p types.Part) (legacyFile, bool) {
	src, _ := types.SourceOf(p)
	if src.Digest != "" || src.Size != 0 || src.Ref != "" || len(src.Files) > 0 || src.Unresolved != "" {
		return legacyFile{}, false
	}
	switch x := p.(type) {
	case types.ImagePart:
		if x.ImageMeta != (types.ImageMeta{}) {
			return legacyFile{}, false
		}
	case types.AudioPart:
		if x.AudioMeta != (types.AudioMeta{}) {
			return legacyFile{}, false
		}
	case types.VideoPart:
		if x.VideoMeta != (types.VideoMeta{}) {
			return legacyFile{}, false
		}
	case types.DocumentPart:
		if x.DocumentMeta != (types.DocumentMeta{}) {
			return legacyFile{}, false
		}
	}
	if types.Media(src).Kind() != p.Kind() {
		return legacyFile{}, false
	}
	return legacyFile{URI: src.URI, MediaType: src.MediaType, Filename: src.Filename}, true
}

func unmarshalMessage(role types.Role, data json.RawMessage) (types.Message, error) {
	var env messageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	switch role {
	case types.RoleSystem:
		parts, err := unmarshalParts[types.SystemPart](role, env.Content)
		return types.SystemMessage{Parts: parts}, err
	case types.RoleUser:
		parts, err := unmarshalParts[types.UserPart](role, env.Content)
		return types.UserMessage{Parts: parts}, err
	case types.RoleAssistant:
		parts, err := unmarshalParts[types.AssistantPart](role, env.Content)
		return types.AssistantMessage{Parts: parts}, err
	default:
		return nil, fmt.Errorf("unknown role: %s", role)
	}
}

// errUnknownContent reports a stored type tag this release does not read.
var errUnknownContent = errors.New("unknown content type")

func unmarshalParts[T types.Part](role types.Role, content []contentEnvelope) ([]T, error) {
	var out []T
	for _, ce := range content {
		ps, err := unmarshalPart(ce)
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

//nolint:gocyclo // one case per stored kind
func unmarshalPart(ce contentEnvelope) ([]types.Part, error) {
	one := func(p types.Part, err error) ([]types.Part, error) {
		if err != nil {
			return nil, err
		}
		return []types.Part{p}, nil
	}
	switch ce.Type {
	case contentTypePart:
		return one(types.UnmarshalPart(ce.Data))
	case contentTypeText:
		var c legacyText
		err := json.Unmarshal(ce.Data, &c)
		return one(types.TextPart{Text: c.Text}, err)
	case "tool_use":
		var c legacyToolUse
		err := decodeNumbers(ce.Data, &c)
		return one(types.ToolCallPart(c), err)
	case contentTypeThinking:
		var c legacyThinking
		err := json.Unmarshal(ce.Data, &c)
		return one(types.ThinkingPart{Text: c.Thinking, Signature: c.Signature}, err)
	case contentTypeToolResult:
		var c legacyToolResult
		if err := json.Unmarshal(ce.Data, &c); err != nil {
			return nil, err
		}
		return one(toolResultFromLegacy(c), nil)
	case contentTypeFile:
		var c legacyFile
		err := json.Unmarshal(ce.Data, &c)
		return one(types.Media(types.Source{URI: c.URI, MediaType: c.MediaType, Filename: c.Filename}), err)
	case contentTypeServerTool:
		var c legacyServerTool
		if err := decodeNumbers(ce.Data, &c); err != nil {
			return nil, err
		}
		out := []types.Part{types.ServerToolCallPart{ID: c.ID, ToolKind: c.Kind, Name: c.Name, Input: c.Input}}
		if c.Text != "" || len(c.Result) > 0 || len(c.Files) > 0 {
			r := types.ServerToolResultPart{CallID: c.ID, ToolKind: c.Kind, Text: c.Text, Result: c.Result}
			for _, f := range c.Files {
				r.Outputs = append(r.Outputs, types.Media(types.Source{URI: f.URI, MediaType: f.MediaType, Filename: f.Filename}))
			}
			out = append(out, r)
		}
		return out, nil
	case contentTypeConfig:
		return decodeLegacy[types.ConfigPart](ce.Data)
	case contentTypeHandoff:
		return decodeLegacy[types.HandoffPart](ce.Data)
	case "feedback":
		return decodeLegacy[types.FeedbackPart](ce.Data)
	case contentTypeSteer:
		return decodeLegacy[types.SteerPart](ce.Data)
	case contentTypeTruncation:
		return decodeLegacy[types.TruncationPart](ce.Data)
	case contentTypeRoute:
		return decodeLegacy[types.RoutePart](ce.Data)
	case contentTypeApproval:
		return decodeLegacy[types.ApprovalPart](ce.Data)
	case contentTypeGuardrail:
		return decodeLegacy[types.GuardrailPart](ce.Data)
	case contentTypeCompaction:
		return decodeLegacy[types.CompactionPart](ce.Data)
	default:
		return nil, errUnknownContent
	}
}

func decodeLegacy[T types.Part](data json.RawMessage) ([]types.Part, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return []types.Part{v}, nil
}

// toolResultFromLegacy reads a v1 tool result: its blocks when it has them,
// with the text first when no block carries it, else its text.
func toolResultFromLegacy(c legacyToolResult) types.ToolResultPart {
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
			src := types.Source{URI: b.URI, MediaType: b.MediaType, Filename: b.Filename}
			if b.Kind == "image" {
				r.Parts = append(r.Parts, types.Image(src))
				continue
			}
			if p, ok := types.Media(src).(types.ToolOutputPart); ok {
				r.Parts = append(r.Parts, p)
			} else {
				r.Parts = append(r.Parts, types.File(src))
			}
		}
	}
	return r
}

// MarshalJSON serializes the tree to JSON.
func (t *Tree) MarshalJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := serializedTree{
		V:        TreeFormatVersion,
		Metadata: t.metadata,
		RootID:   string(t.rootID),
		Active:   string(t.active),
		Children: make(map[string][]string, len(t.children)),
		Branches: make(map[string]string, len(t.branches)),
	}

	for id, childIDs := range t.children {
		kids := make([]string, len(childIDs))
		for i, c := range childIDs {
			kids[i] = string(c)
		}
		st.Children[string(id)] = kids
	}

	for bid, nid := range t.branches {
		st.Branches[string(bid)] = string(nid)
	}

	for _, node := range t.nodes {
		serialized, err := serializeNode(node)
		if err != nil {
			return nil, fmt.Errorf("marshal message for node %s: %w", node.ID, err)
		}
		st.Nodes = append(st.Nodes, serialized)
	}

	if len(t.checkpoints) > 0 {
		st.Checkpoints = make(map[string]serializedCheckpoint, len(t.checkpoints))
		for cpID, cp := range t.checkpoints {
			st.Checkpoints[string(cpID)] = serializedCheckpoint{
				ID:        string(cp.ID),
				Branch:    string(cp.Branch),
				NodeID:    string(cp.NodeID),
				Name:      cp.Name,
				CreatedAt: cp.CreatedAt,
			}
		}
	}

	return json.Marshal(st)
}

// UnmarshalJSON restores a tree from JSON. The input is decoded in full
// before the tree changes, so a malformed document leaves the tree as it was,
// and the result is installed under the tree's lock, so concurrent readers
// never see a partly restored tree.
func (t *Tree) UnmarshalJSON(data []byte) error {
	var wire struct {
		serializedTree
		Content []serializedNode `json:"content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	st := wire.serializedTree
	if st.V < 0 || st.V > TreeFormatVersion {
		return fmt.Errorf("%w: %d", ErrTreeFormatVersion, st.V)
	}
	if wire.Content != nil {
		if st.Nodes != nil {
			return fmt.Errorf("tree: both nodes and content were supplied")
		}
		st.Nodes = wire.Content
	}
	if err := validateMetadata(st.Metadata); err != nil {
		return err
	}

	nodes := make(map[types.NodeID]*types.Node, len(st.Nodes))
	children := make(map[types.NodeID][]types.NodeID, len(st.Children))
	branches := make(map[types.BranchID]types.NodeID, len(st.Branches))
	checkpoints := make(map[types.CheckpointID]types.Checkpoint)

	for _, sn := range st.Nodes {
		msg, err := unmarshalMessage(types.Role(sn.Role), sn.Message)
		if err != nil {
			return fmt.Errorf("node %s: %w", sn.ID, err)
		}

		summaryOf := make([]types.NodeID, len(sn.SummaryOf))
		for i, s := range sn.SummaryOf {
			summaryOf[i] = types.NodeID(s)
		}

		nodes[types.NodeID(sn.ID)] = &types.Node{
			ID:         types.NodeID(sn.ID),
			ParentID:   types.NodeID(sn.ParentID),
			Message:    msg,
			State:      types.NodeState(sn.State),
			Version:    sn.Version,
			Depth:      sn.Depth,
			BranchID:   types.BranchID(sn.BranchID),
			CreatedAt:  sn.CreatedAt,
			UpdatedAt:  sn.UpdatedAt,
			ArchivedAt: sn.ArchivedAt,
			ArchivedBy: sn.ArchivedBy,
			SummaryOf:  summaryOf,
		}
	}

	for parentStr, childStrs := range st.Children {
		kids := make([]types.NodeID, len(childStrs))
		for i, c := range childStrs {
			kids[i] = types.NodeID(c)
		}
		children[types.NodeID(parentStr)] = kids
	}

	for bStr, nStr := range st.Branches {
		branches[types.BranchID(bStr)] = types.NodeID(nStr)
	}

	for _, scp := range st.Checkpoints {
		cpID := types.CheckpointID(scp.ID)
		checkpoints[cpID] = types.Checkpoint{
			ID:        cpID,
			Branch:    types.BranchID(scp.Branch),
			NodeID:    types.NodeID(scp.NodeID),
			Name:      scp.Name,
			CreatedAt: scp.CreatedAt,
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.metadata = append(json.RawMessage(nil), st.Metadata...)
	t.nodes = nodes
	t.children = children
	t.branches = branches
	t.checkpoints = checkpoints
	t.rootID = types.NodeID(st.RootID)
	t.active = types.BranchID(st.Active)
	return nil
}
