package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/turn/native"
)

// A host chooses which session verbs a turn with a session is offered. One
// it leaves out is no tool of the turn: it is not granted, and a spec that
// names it does not resolve it. A host that says nothing offers every verb,
// and session_call still needs an agent to call.
func TestWithSessionToolsLimitsTheSessionVerbsATurnIsOffered(t *testing.T) {
	rt := &Runtime{sessions: session.NewMemory()}
	for _, tc := range []struct {
		name  string
		opts  []SandboxOption
		spawn []string
		tools []string
		want  []string
	}{
		{name: "every verb by default", spawn: []string{"helper"}, want: []string{"tag_helper", "session_call", "session_listen", "session_read", "session_status", "session_link"}},
		{name: "no session_call without an agent to call", want: []string{"session_listen", "session_read", "session_status", "session_link"}},
		{name: "only the verbs named", opts: []SandboxOption{WithSessionTools("session_read", "session_link")}, spawn: []string{"helper"}, want: []string{"tag_helper", "session_read", "session_link"}},
		{name: "none when none is named", opts: []SandboxOption{WithSessionTools()}, spawn: []string{"helper"}, want: []string{"tag_helper"}},
		{name: "a verb left out is not resolved for a spec that names it", opts: []SandboxOption{WithSessionTools("session_read")}, tools: []string{"session_call"}, want: []string{"session_call", "session_read"}},
		{name: "a name that is no session verb is ignored", opts: []SandboxOption{WithSessionTools("session_read", "shell")}, want: []string{"session_read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Sandbox{sessionRuntime: func() *Runtime { return rt }}
			for _, opt := range tc.opts {
				opt(s)
			}
			req := Request{Tenant: "acme", RunID: "run-1", Session: &session.Record{ID: "s_1"}}
			req.Definition.Spec.Tools = tc.tools
			req.Definition.Spec.Access.Spawn = tc.spawn
			tools, granted, err := s.turnTools(context.Background(), nil, req, func(event.Body) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(granted, tc.want) {
				t.Fatalf("granted %v, want %v", granted, tc.want)
			}
			for _, name := range sessionToolNames {
				offered := s.sessionTools == nil || slices.Contains(s.sessionTools, name)
				_, err := tools.Lookup(name)
				if offered && err != nil {
					t.Errorf("%s is offered and does not resolve: %v", name, err)
				}
				if !offered && !errors.Is(err, native.ErrNoSuchTool) {
					t.Errorf("%s was left out and resolves: %v", name, err)
				}
			}
		})
	}
}

// A turn with no session is offered no session verb, whatever the host named.
func TestSessionVerbsNeedASession(t *testing.T) {
	s := &Sandbox{sessionRuntime: func() *Runtime { return &Runtime{} }}
	WithSessionTools("session_read")(s)
	_, granted, err := s.turnTools(context.Background(), nil, Request{Tenant: "acme", RunID: "run-1"}, func(event.Body) error { return nil })
	if err != nil || len(granted) != 0 {
		t.Fatalf("granted %v, %v", granted, err)
	}
}
