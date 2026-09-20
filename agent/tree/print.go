package tree

import (
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/urmzd/saige/agent/types"
)

// Print writes one JSON document with the flattened nodes in content.
// Node fields are identical to those in Tree.MarshalJSON. Tree metadata,
// branches, children, active branch, and checkpoints are also retained.
// All branches and node states are included, including archived and feedback
// nodes. Nodes are ordered by depth, creation time, then ID, so parents precede
// children and repeated exports of the same tree have identical bytes.
//
// Print snapshots under the tree read lock and releases it before writing.
// It changes no state. The document can be restored by Tree.UnmarshalJSON.
// A write error can leave a partial output and is returned unchanged.
func Print(w io.Writer, t *Tree) error {
	if w == nil || t == nil {
		return errors.New("tree: Print requires a writer and tree")
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	// Preserve every serialized tree field. Only the collection name and its
	// order differ: content is the flattened, deterministic node collection.
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	var nodes []serializedNode
	if err := json.Unmarshal(document["nodes"], &nodes); err != nil {
		return err
	}
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	if nodes == nil {
		nodes = []serializedNode{}
	}
	document["content"], err = json.Marshal(nodes)
	if err != nil {
		return err
	}
	delete(document, "nodes")
	raw, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := w.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	return nil
}

// MarshalNode uses the same portable node and content representation as Print
// and Tree.MarshalJSON. WAL consumers can export a node before tree publication
// without acquiring the tree lock again. The caller must keep node immutable
// for the duration of this call.
func MarshalNode(node *types.Node) (json.RawMessage, error) {
	value, err := serializeNode(node)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func serializeNode(node *types.Node) (serializedNode, error) {
	if node == nil || node.Message == nil {
		return serializedNode{}, errors.New("tree: cannot serialize a nil node or message")
	}
	message, err := marshalMessage(node.Message)
	if err != nil {
		return serializedNode{}, err
	}
	summaryOf := make([]string, len(node.SummaryOf))
	for i, id := range node.SummaryOf {
		summaryOf[i] = string(id)
	}
	return serializedNode{
		ID: string(node.ID), ParentID: string(node.ParentID),
		Role: string(node.Message.Role()), Message: message,
		State: int(node.State), Version: node.Version, Depth: node.Depth,
		BranchID: string(node.BranchID), CreatedAt: node.CreatedAt, UpdatedAt: node.UpdatedAt,
		ArchivedAt: node.ArchivedAt, ArchivedBy: node.ArchivedBy, SummaryOf: summaryOf,
	}, nil
}
