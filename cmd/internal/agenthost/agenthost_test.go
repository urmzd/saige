package agenthost

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

func scripted(responses ...[]types.Delta) func() (Agent, error) {
	return func() (Agent, error) {
		a := must.Get(agentsdk.New(agentsdk.Config{Name: "t", Provider: &agenttest.ScriptedProvider{Responses: responses}}))
		return Agent{Agent: a}, nil
	}
}

func TestManagerCapAndRemove(t *testing.T) {
	m := NewManager[int](Options{Max: 1, Prefix: "s_"})
	var released atomic.Int32
	s, err := m.Create("", 7, func() (Agent, error) {
		a, _ := scripted()()
		a.Release = func() { released.Add(1) }
		return a, nil
	})
	if err != nil || s.Host != 7 || len(s.ID) != len("s_")+24 {
		t.Fatalf("create: %+v %v", s, err)
	}
	if _, err := m.Create("", 0, scripted()); !errors.Is(err, ErrLimit) {
		t.Fatalf("over the cap: %v", err)
	}
	if m.Get(s.ID) != s || len(m.List()) != 1 {
		t.Fatal("lookup failed")
	}
	m.Remove(s.ID)
	m.Remove(s.ID)
	if released.Load() != 1 || m.Get(s.ID) != nil {
		t.Fatalf("released %d times", released.Load())
	}
	if _, err := s.Start(context.Background(), types.UserMsg(types.Text("hi"))); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed session started: %v", err)
	}
	// A failed build frees its slot.
	if _, err := m.Create("", 0, func() (Agent, error) { return Agent{}, errors.New("boom") }); err == nil {
		t.Fatal("build error lost")
	}
	if _, err := m.Create("fixed", 0, scripted()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("fixed", 0, scripted()); err == nil {
		t.Fatal("duplicate id accepted")
	}
}

func TestSessionOneTurnAtATimeAndIdle(t *testing.T) {
	m := NewManager[struct{}](Options{IdleTTL: time.Minute})
	block := make(chan struct{})
	tool := &types.ToolFunc{
		Def: types.ToolDef{Name: "wait", Parameters: types.ParameterSchema{Type: "object"}, Capability: types.ToolCapabilityRead},
		Fn: func(ctx context.Context, _ map[string]any) (string, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return "ok", nil
		},
	}
	s, err := m.Create("", struct{}{}, func() (Agent, error) {
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.ToolCallResponse("c1", "wait", map[string]any{}), agenttest.TextResponse("done"),
		}}
		return Agent{Agent: must.Get(agentsdk.New(agentsdk.Config{Name: "t", Provider: p, Tools: types.NewToolRegistry(tool)}))}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.Start(context.Background(), types.UserMsg(types.Text("go")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background(), types.UserMsg(types.Text("again"))); !errors.Is(err, ErrBusy) {
		t.Fatalf("second turn: %v", err)
	}
	if s.IdleFor(time.Now().Add(time.Hour)) != 0 || len(m.Evict(time.Now().Add(time.Hour))) != 0 {
		t.Fatal("a running session counted as idle")
	}
	if err := s.Replace(Agent{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("replace while running: %v", err)
	}
	close(block)
	if _, err := agentsdk.Collect(stream, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Running() {
		if time.Now().After(deadline) {
			t.Fatal("the turn never ended")
		}
		time.Sleep(time.Millisecond)
	}
	if got := m.Evict(time.Now().Add(2 * time.Minute)); len(got) != 1 {
		t.Fatalf("evicted %d sessions, want 1", len(got))
	}
}

func TestDecideChecksTheGrant(t *testing.T) {
	m := NewManager[struct{}](Options{})
	s, _ := m.Create("", struct{}{}, func() (Agent, error) {
		a, _ := scripted()()
		a.MaxGrant = types.GrantTool
		a.CheckGrant = func(g *types.GrantRequest) error {
			if g != nil && g.Scope == types.GrantSession {
				return types.ErrInvalidGrant
			}
			return nil
		}
		return a, nil
	})
	now := time.Now()
	tests := []struct {
		name string
		r    agentsdk.Resolution
		want error
	}{
		{"grant on a denial", agentsdk.Resolution{Grant: &types.GrantRequest{Scope: types.GrantTool}}, nil},
		{"beyond the cap", agentsdk.Resolution{Approved: true, Grant: &types.GrantRequest{Scope: types.GrantSession}}, types.ErrInvalidGrant},
		{"expired", agentsdk.Resolution{Approved: true, Grant: &types.GrantRequest{Scope: types.GrantTool, ExpiresAt: now.Add(-time.Hour)}}, types.ErrInvalidGrant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CheckDecision(tt.r, now)
			if err == nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	if err := s.Decide("c1", agentsdk.Resolution{Approved: true}); !errors.Is(err, ErrNoTurn) {
		t.Fatalf("decide without a turn: %v", err)
	}
	if !s.Agent().AllowsGrant(types.GrantArgs) || s.Agent().AllowsGrant(types.GrantSession) {
		t.Fatal("AllowsGrant ignores the cap")
	}
}
