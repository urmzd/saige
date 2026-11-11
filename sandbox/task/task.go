// Package task defines the unit of work that flows through playscape:
// submitted by a program or by an agent spawning a sub-task, and run by an
// agent execution node. It is a leaf package: every other layer speaks
// these types without depending on one another.
package task

import (
	"crypto/rand"
	"encoding/hex"
)

// Task asks a deployment to invoke one named tool with an opaque input.
type Task struct {
	// ID identifies the task within its deployment. Submitters
	// may leave it empty; the control plane assigns one on submission.
	ID string `json:"id,omitempty"`
	// Tool is the registered tool name to invoke.
	Tool string `json:"tool"`
	// Input is the opaque payload handed to the tool.
	Input []byte `json:"input,omitempty"`
}

// Result is the outcome of executing a Task on a node. Error carries a
// failure as a string so results survive any transport.
type Result struct {
	TaskID string `json:"task_id"`
	NodeID string `json:"node_id,omitempty"`
	Output []byte `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// NewID returns a fresh 128-bit hex task ID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("task: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
