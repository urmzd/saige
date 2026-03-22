package core

import (
	"testing"
	"time"
)

func TestTreePathString(t *testing.T) {
	tests := []struct {
		path TreePath
		want string
	}{
		{TreePath{}, ""},
		{TreePath{0}, "0"},
		{TreePath{0, 1, 2}, "0/1/2"},
		{TreePath{3, 14, 159}, "3/14/159"},
	}
	for _, tt := range tests {
		if got := tt.path.String(); got != tt.want {
			t.Errorf("TreePath%v.String() = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestParseTreePath(t *testing.T) {
	tests := []struct {
		input   string
		want    TreePath
		wantErr bool
	}{
		{"", TreePath{}, false},
		{"0", TreePath{0}, false},
		{"0/1/2", TreePath{0, 1, 2}, false},
		{"abc", nil, true},
	}
	for _, tt := range tests {
		got, err := ParseTreePath(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseTreePath(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && len(got) != len(tt.want) {
			t.Errorf("ParseTreePath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestTreePathParent(t *testing.T) {
	tests := []struct {
		path TreePath
		want TreePath
	}{
		{TreePath{}, nil},
		{TreePath{0}, nil},
		{TreePath{0, 1}, TreePath{0}},
		{TreePath{0, 1, 2}, TreePath{0, 1}},
	}
	for _, tt := range tests {
		got := tt.path.Parent()
		if len(got) != len(tt.want) {
			t.Errorf("TreePath%v.Parent() = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestTreePathIsAncestorOf(t *testing.T) {
	tests := []struct {
		a, b TreePath
		want bool
	}{
		{TreePath{0}, TreePath{0, 1}, true},
		{TreePath{0, 1}, TreePath{0, 1, 2}, true},
		{TreePath{0, 1}, TreePath{0, 1}, false},      // not strict prefix
		{TreePath{0, 1, 2}, TreePath{0, 1}, false},    // longer
		{TreePath{1}, TreePath{0, 1}, false},           // different path
	}
	for _, tt := range tests {
		if got := tt.a.IsAncestorOf(tt.b); got != tt.want {
			t.Errorf("%v.IsAncestorOf(%v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestNodeStates(t *testing.T) {
	if NodeActive != 0 {
		t.Error("NodeActive should be 0")
	}
	if NodeFeedback <= NodeActive {
		t.Error("NodeFeedback should be greater than NodeActive")
	}
}

func TestNodeFields(t *testing.T) {
	now := time.Now()
	n := Node{
		ID:       "test-id",
		ParentID: "parent-id",
		State:    NodeActive,
		Version:  1,
		Depth:    3,
		BranchID: "main",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if n.ID != "test-id" {
		t.Errorf("ID = %s, want test-id", n.ID)
	}
	if n.Depth != 3 {
		t.Errorf("Depth = %d, want 3", n.Depth)
	}
}
