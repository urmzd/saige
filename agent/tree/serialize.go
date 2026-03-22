package tree

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/urmzd/graph-agent-dev-kit/agent/core"
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

func marshalMessage(msg core.Message) (json.RawMessage, error) {
	var env messageEnvelope

	switch m := msg.(type) {
	case core.SystemMessage:
		for _, c := range m.Content {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			typeName := systemContentType(c)
			env.Content = append(env.Content, contentEnvelope{Type: typeName, Data: data})
		}
	case core.UserMessage:
		for _, c := range m.Content {
			data, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			typeName := userContentType(c)
			env.Content = append(env.Content, contentEnvelope{Type: typeName, Data: data})
		}
	case core.AssistantMessage:
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

func unmarshalMessage(role core.Role, data json.RawMessage) (core.Message, error) {
	var env messageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}

	switch role {
	case core.RoleSystem:
		var content []core.SystemContent
		for _, ce := range env.Content {
			c, err := unmarshalSystemContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return core.SystemMessage{Content: content}, nil

	case core.RoleUser:
		var content []core.UserContent
		for _, ce := range env.Content {
			c, err := unmarshalUserContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return core.UserMessage{Content: content}, nil

	case core.RoleAssistant:
		var content []core.AssistantContent
		for _, ce := range env.Content {
			c, err := unmarshalAssistantContent(ce)
			if err != nil {
				return nil, err
			}
			content = append(content, c)
		}
		return core.AssistantMessage{Content: content}, nil

	default:
		return nil, fmt.Errorf("unknown role: %s", role)
	}
}

func systemContentType(c core.SystemContent) string {
	switch c.(type) {
	case core.TextContent:
		return "text"
	case core.ToolResultContent:
		return "tool_result"
	case core.ConfigContent:
		return "config"
	default:
		return "unknown"
	}
}

func userContentType(c core.UserContent) string {
	switch c.(type) {
	case core.TextContent:
		return "text"
	case core.ToolResultContent:
		return "tool_result"
	case core.ConfigContent:
		return "config"
	case core.FileContent:
		return "file"
	case core.FeedbackContent:
		return "feedback"
	default:
		return "unknown"
	}
}

func assistantContentType(c core.AssistantContent) string {
	switch c.(type) {
	case core.TextContent:
		return "text"
	case core.ToolUseContent:
		return "tool_use"
	default:
		return "unknown"
	}
}

func unmarshalSystemContent(ce contentEnvelope) (core.SystemContent, error) {
	switch ce.Type {
	case "text":
		var c core.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case "tool_result":
		var c core.ToolResultContent
		return c, json.Unmarshal(ce.Data, &c)
	case "config":
		var c core.ConfigContent
		return c, json.Unmarshal(ce.Data, &c)
	default:
		return nil, fmt.Errorf("unknown system content type: %s", ce.Type)
	}
}

func unmarshalUserContent(ce contentEnvelope) (core.UserContent, error) {
	switch ce.Type {
	case "text":
		var c core.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case "tool_result":
		var c core.ToolResultContent
		return c, json.Unmarshal(ce.Data, &c)
	case "config":
		var c core.ConfigContent
		return c, json.Unmarshal(ce.Data, &c)
	case "file":
		var c core.FileContent
		return c, json.Unmarshal(ce.Data, &c)
	case "feedback":
		var c core.FeedbackContent
		return c, json.Unmarshal(ce.Data, &c)
	default:
		return nil, fmt.Errorf("unknown user content type: %s", ce.Type)
	}
}

func unmarshalAssistantContent(ce contentEnvelope) (core.AssistantContent, error) {
	switch ce.Type {
	case "text":
		var c core.TextContent
		return c, json.Unmarshal(ce.Data, &c)
	case "tool_use":
		var c core.ToolUseContent
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

	t.nodes = make(map[core.NodeID]*core.Node, len(st.Nodes))
	t.children = make(map[core.NodeID][]core.NodeID, len(st.Children))
	t.branches = make(map[core.BranchID]core.NodeID, len(st.Branches))
	t.checkpoints = make(map[core.CheckpointID]core.Checkpoint)
	t.rootID = core.NodeID(st.RootID)
	t.active = core.BranchID(st.Active)

	for _, sn := range st.Nodes {
		msg, err := unmarshalMessage(core.Role(sn.Role), sn.Message)
		if err != nil {
			return fmt.Errorf("node %s: %w", sn.ID, err)
		}

		summaryOf := make([]core.NodeID, len(sn.SummaryOf))
		for i, s := range sn.SummaryOf {
			summaryOf[i] = core.NodeID(s)
		}

		t.nodes[core.NodeID(sn.ID)] = &core.Node{
			ID:         core.NodeID(sn.ID),
			ParentID:   core.NodeID(sn.ParentID),
			Message:    msg,
			State:      core.NodeState(sn.State),
			Version:    sn.Version,
			Depth:      sn.Depth,
			BranchID:   core.BranchID(sn.BranchID),
			CreatedAt:  sn.CreatedAt,
			UpdatedAt:  sn.UpdatedAt,
			ArchivedAt: sn.ArchivedAt,
			ArchivedBy: sn.ArchivedBy,
			SummaryOf:  summaryOf,
		}
	}

	for parentStr, childStrs := range st.Children {
		kids := make([]core.NodeID, len(childStrs))
		for i, c := range childStrs {
			kids[i] = core.NodeID(c)
		}
		t.children[core.NodeID(parentStr)] = kids
	}

	for bStr, nStr := range st.Branches {
		t.branches[core.BranchID(bStr)] = core.NodeID(nStr)
	}

	for _, scp := range st.Checkpoints {
		cpID := core.CheckpointID(scp.ID)
		t.checkpoints[cpID] = core.Checkpoint{
			ID:        cpID,
			Branch:    core.BranchID(scp.Branch),
			NodeID:    core.NodeID(scp.NodeID),
			Name:      scp.Name,
			CreatedAt: scp.CreatedAt,
		}
	}

	return nil
}
