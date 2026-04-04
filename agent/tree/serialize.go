package tree

import (
	"encoding/json"
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
	contentTypeUnknown    = "unknown"
)

// serializedTree is the JSON wire format for a Tree.
type serializedTree struct {
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

// contentEnvelope wraps a content block with its type for JSON round-tripping.
type contentEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// messageEnvelope wraps content blocks with type tags.
type messageEnvelope struct {
	Content []contentEnvelope `json:"content"`
}

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

	switch m := msg.(type) {
	case types.SystemMessage:
		for _, c := range m.Content {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			typeName := systemContentType(c)
			env.Content = append(env.Content, contentEnvelope{Type: typeName, Data: data})
		}
	case types.UserMessage:
		for _, c := range m.Content {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			typeName := userContentType(c)
			env.Content = append(env.Content, contentEnvelope{Type: typeName, Data: data})
		}
	case types.AssistantMessage:
		for _, c := range m.Content {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			typeName := assistantContentType(c)
			env.Content = append(env.Content, contentEnvelope{Type: typeName, Data: data})
		}
	}

	return json.Marshal(env)
}

func unmarshalMessage(role types.Role, data json.RawMessage) (types.Message, error) {
	var env messageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}

	switch role {
	case types.RoleSystem:
		var content []types.SystemContent
		for _, ce := range env.Content {
			c, err := unmarshalSystemContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return types.SystemMessage{Content: content}, nil

	case types.RoleUser:
		var content []types.UserContent
		for _, ce := range env.Content {
			c, err := unmarshalUserContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return types.UserMessage{Content: content}, nil

	case types.RoleAssistant:
		var content []types.AssistantContent
		for _, ce := range env.Content {
			c, err := unmarshalAssistantContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return types.AssistantMessage{Content: content}, nil

	default:
		return nil, fmt.Errorf("unknown role: %s", role)
	}
}

func systemContentType(c types.SystemContent) string {
	switch c.(type) {
	case types.TextContent:
		return contentTypeText
	case types.ToolResultContent:
		return contentTypeToolResult
	case types.ConfigContent:
		return contentTypeConfig
	default:
		return contentTypeUnknown
	}
}

func userContentType(c types.UserContent) string {
	switch c.(type) {
	case types.TextContent:
		return contentTypeText
	case types.ToolResultContent:
		return contentTypeToolResult
	case types.ConfigContent:
		return contentTypeConfig
	case types.FileContent:
		return "file"
	case types.FeedbackContent:
		return "feedback"
	default:
		return contentTypeUnknown
	}
}

func assistantContentType(c types.AssistantContent) string {
	switch c.(type) {
	case types.TextContent:
		return contentTypeText
	case types.ToolUseContent:
		return "tool_use"
	case types.ThinkingContent:
		return contentTypeThinking
	default:
		return contentTypeUnknown
	}
}

func unmarshalSystemContent(ce contentEnvelope) (types.SystemContent, error) {
	switch ce.Type {
	case contentTypeText:
		var c types.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case contentTypeToolResult:
		var c types.ToolResultContent
		return c, json.Unmarshal(ce.Data, &c)
	case contentTypeConfig:
		var c types.ConfigContent
		return c, json.Unmarshal(ce.Data, &c)
	default:
		return nil, fmt.Errorf("unknown system content type: %s", ce.Type)
	}
}

func unmarshalUserContent(ce contentEnvelope) (types.UserContent, error) {
	switch ce.Type {
	case contentTypeText:
		var c types.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case contentTypeToolResult:
		var c types.ToolResultContent
		return c, json.Unmarshal(ce.Data, &c)
	case contentTypeConfig:
		var c types.ConfigContent
		return c, json.Unmarshal(ce.Data, &c)
	case "file":
		var c types.FileContent
		return c, json.Unmarshal(ce.Data, &c)
	case "feedback":
		var c types.FeedbackContent
		return c, json.Unmarshal(ce.Data, &c)
	default:
		return nil, fmt.Errorf("unknown user content type: %s", ce.Type)
	}
}

func unmarshalAssistantContent(ce contentEnvelope) (types.AssistantContent, error) {
	switch ce.Type {
	case contentTypeText:
		var c types.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case "tool_use":
		var c types.ToolUseContent
		return c, json.Unmarshal(ce.Data, &c)
	case contentTypeThinking:
		var c types.ThinkingContent
		return c, json.Unmarshal(ce.Data, &c)
	default:
		return nil, fmt.Errorf("unknown assistant content type: %s", ce.Type)
	}
}

// MarshalJSON serializes the tree to JSON.
func (t *Tree) MarshalJSON() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := serializedTree{
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
		msgBytes, err := marshalMessage(node.Message)
		if err != nil {
			return nil, fmt.Errorf("marshal message for node %s: %w", node.ID, err)
		}

		summaryOf := make([]string, len(node.SummaryOf))
		for i, s := range node.SummaryOf {
			summaryOf[i] = string(s)
		}

		st.Nodes = append(st.Nodes, serializedNode{
			ID:         string(node.ID),
			ParentID:   string(node.ParentID),
			Role:       string(node.Message.Role()),
			Message:    msgBytes,
			State:      int(node.State),
			Version:    node.Version,
			Depth:      node.Depth,
			BranchID:   string(node.BranchID),
			CreatedAt:  node.CreatedAt,
			UpdatedAt:  node.UpdatedAt,
			ArchivedAt: node.ArchivedAt,
			ArchivedBy: node.ArchivedBy,
			SummaryOf:  summaryOf,
		})
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

// UnmarshalJSON restores a tree from JSON.
func (t *Tree) UnmarshalJSON(data []byte) error {
	var st serializedTree
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}

	t.nodes = make(map[types.NodeID]*types.Node, len(st.Nodes))
	t.children = make(map[types.NodeID][]types.NodeID, len(st.Children))
	t.branches = make(map[types.BranchID]types.NodeID, len(st.Branches))
	t.checkpoints = make(map[types.CheckpointID]types.Checkpoint)
	t.rootID = types.NodeID(st.RootID)
	t.active = types.BranchID(st.Active)

	for _, sn := range st.Nodes {
		msg, err := unmarshalMessage(types.Role(sn.Role), sn.Message)
		if err != nil {
			return fmt.Errorf("node %s: %w", sn.ID, err)
		}

		summaryOf := make([]types.NodeID, len(sn.SummaryOf))
		for i, s := range sn.SummaryOf {
			summaryOf[i] = types.NodeID(s)
		}

		t.nodes[types.NodeID(sn.ID)] = &types.Node{
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
		t.children[types.NodeID(parentStr)] = kids
	}

	for bStr, nStr := range st.Branches {
		t.branches[types.BranchID(bStr)] = types.NodeID(nStr)
	}

	for _, scp := range st.Checkpoints {
		cpID := types.CheckpointID(scp.ID)
		t.checkpoints[cpID] = types.Checkpoint{
			ID:        cpID,
			Branch:    types.BranchID(scp.Branch),
			NodeID:    types.NodeID(scp.NodeID),
			Name:      scp.Name,
			CreatedAt: scp.CreatedAt,
		}
	}

	return nil
}
