package main

import (
	"context"
	"slices"
	"testing"

	"github.com/urmzd/saige/tools"
)

func TestHarnessFlagDefaults(t *testing.T) {
	ctx := context.Background()
	for cmd, want := range map[string]string{"chat": toolsReadOnly, "ask": toolsNone} {
		c := newChatCmd(ctx)
		if cmd == "ask" {
			c = newAskCmd(ctx)
		}
		if got := c.Flags().Lookup("tools").DefValue; got != want {
			t.Errorf("%s --tools default = %q, want %q", cmd, got, want)
		}
	}
}

func TestHarnessFlagsBuild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tests := []struct {
		mode    string
		want    []string
		wantErr bool
	}{
		{mode: toolsNone},
		{mode: toolsReadOnly, want: []string{"read_file", "list_dir", "glob", "grep", "scratch_write", "scratch_read", "scratch_search"}},
		{mode: "read,web", want: []string{"read_file", "list_dir", "glob", "grep", "fetch_url", "scratch_write", "scratch_read", "scratch_search"}},
		{mode: "everything", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			set, err := harnessFlags{mode: tt.mode, workspace: root, sandbox: "subprocess", network: "deny"}.build(ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var names []string
			if set != nil {
				names = set.Names()
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("names = %v, want %v", names, tt.want)
			}
		})
	}

	groups, err := harnessFlags{mode: toolsHarness}.groups()
	if err != nil || !slices.Equal(groups, tools.AllGroups()) {
		t.Errorf("harness groups = %v, %v", groups, err)
	}
}
