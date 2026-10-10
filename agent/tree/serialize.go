package tree

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// TreeFormatVersion is the serialized tree format this package writes.
// UnmarshalJSON rejects a document with a higher version instead of guessing
// at its meaning. A document without a version predates the field and is
// read as version 1. Version 2 stores node messages in the version 2
// message format (MessageFormatVersion); each message names its own format,
// so a version 1 document reads the same way.
const TreeFormatVersion = 2

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

// MessageFormatVersion is the node message format MarshalMessage writes:
//
//	{"v":2,"parts":[<part>, ...]}
//
// with each part in the shared part codec form (types.MarshalPart), which
// never holds media bytes. Pgstore rows and WAL records store messages on
// their own, without the tree's version, so every message names its own.
// A message without "v" is the version 1 form ({"content":[...]}), read by
// legacy_v1.go.
const MessageFormatVersion = 2

// messageV2 is the version 2 node message.
type messageV2 struct {
	V     int               `json:"v"`
	Parts []json.RawMessage `json:"parts"`
}

// MarshalMessage serializes a Message in the current node message format.
func MarshalMessage(msg types.Message) (json.RawMessage, error) {
	parts := types.PartsOf(msg)
	out := messageV2{V: MessageFormatVersion, Parts: make([]json.RawMessage, 0, len(parts))}
	for _, p := range parts {
		raw, err := types.MarshalPart(p)
		if err != nil {
			return nil, err
		}
		out.Parts = append(out.Parts, raw)
	}
	return json.Marshal(out)
}

// UnmarshalMessage deserializes a Message from its role and stored JSON, in
// any format a release has written. A version newer than this package
// reads returns ErrMessageFormatVersion.
func UnmarshalMessage(role types.Role, data json.RawMessage) (types.Message, error) {
	v, err := messageVersion(data)
	if err != nil {
		return nil, err
	}
	switch v {
	case 1:
		return unmarshalV1Message(role, data)
	case MessageFormatVersion:
		return unmarshalV2Message(role, data)
	default:
		return nil, fmt.Errorf("%w: %d", ErrMessageFormatVersion, v)
	}
}

// MigrateMessage rewrites a stored message in the current format. It
// reports false, with data unchanged, when the message already is.
func MigrateMessage(role types.Role, data json.RawMessage) (json.RawMessage, bool, error) {
	v, err := messageVersion(data)
	if err != nil {
		return nil, false, err
	}
	if v == MessageFormatVersion {
		return data, false, nil
	}
	msg, err := UnmarshalMessage(role, data)
	if err != nil {
		return nil, false, err
	}
	out, err := MarshalMessage(msg)
	return out, err == nil, err
}

// messageVersion reads the format version a stored message names. A
// message without one is version 1.
func messageVersion(data json.RawMessage) (int, error) {
	var head struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return 0, err
	}
	if head.V == nil {
		return 1, nil
	}
	return *head.V, nil
}

func unmarshalV2Message(role types.Role, data json.RawMessage) (types.Message, error) {
	var m messageV2
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	switch role {
	case types.RoleSystem:
		parts, err := v2Parts[types.SystemPart](role, m.Parts)
		return types.SystemMessage{Parts: parts}, err
	case types.RoleUser:
		parts, err := v2Parts[types.UserPart](role, m.Parts)
		return types.UserMessage{Parts: parts}, err
	case types.RoleAssistant:
		parts, err := v2Parts[types.AssistantPart](role, m.Parts)
		return types.AssistantMessage{Parts: parts}, err
	default:
		return nil, fmt.Errorf("unknown role: %s", role)
	}
}

func v2Parts[T types.Part](role types.Role, raw []json.RawMessage) ([]T, error) {
	var out []T
	for _, r := range raw {
		p, err := types.UnmarshalRolePart[T](r)
		if err != nil {
			return nil, fmt.Errorf("%s message: %w", role, err)
		}
		out = append(out, p)
	}
	return out, nil
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
		msg, err := UnmarshalMessage(types.Role(sn.Role), sn.Message)
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
