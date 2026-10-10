package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// heldProvider answers each call with text, but only after release closes.
type heldProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldProvider() *heldProvider {
	return &heldProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *heldProvider) Stream(ctx context.Context, _ types.Request) (<-chan types.Delta, error) {
	p.once.Do(func() { close(p.started) })
	ch := make(chan types.Delta, 3)
	go func() {
		defer close(ch)
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		for _, d := range agenttest.TextResponse("answer") {
			ch <- d
		}
	}()
	return ch, nil
}

func TestConcurrentRunsOnOneBranchAreRefused(t *testing.T) {
	provider := newHeldProvider()
	a := NewAgent(AgentConfig{Provider: provider})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("one"))})
	<-provider.started

	second := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("two"))})
	deltas := agenttest.CollectDeltas(second.Deltas())
	if err := second.Wait(); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second Invoke = %v, want ErrRunActive", err)
	}
	if len(deltas) == 0 {
		t.Fatal("the refused stream must report its error in-band")
	}
	if _, err := a.RunDurable(ctx, nil, []types.Message{types.UserMsg(types.Text("three"))}, a.Tree().Active()); !errors.Is(err, ErrRunActive) {
		t.Fatalf("RunDurable = %v, want ErrRunActive", err)
	}
	if err := a.LoadSession(&Session{TreeData: json.RawMessage(`{}`)}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("LoadSession = %v, want ErrRunActive", err)
	}

	// Another branch of the same tree is independent.
	root := a.Tree().Root()
	side, _, err := a.Tree().Branch(ctx, root.ID, "side", types.UserMsg(types.Text("side task")))
	if err != nil {
		t.Fatal(err)
	}
	other := a.Invoke(ctx, nil, side)

	close(provider.release)
	for range first.Deltas() {
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	for range other.Deltas() {
	}
	if err := other.Wait(); err != nil {
		t.Fatalf("run on another branch: %v", err)
	}

	// The branch is free again once the run ends, and its history is linear:
	// only the first run's input and answer were added.
	msgs, err := a.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("main has %d messages, want system, input, answer", len(msgs))
	}
	again := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("four"))})
	for range again.Deltas() {
	}
	if err := again.Wait(); err != nil {
		t.Fatalf("Invoke after release: %v", err)
	}
}

func TestConcurrentInvokeRace(t *testing.T) {
	a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("a"), agenttest.TextResponse("b"), agenttest.TextResponse("c"), agenttest.TextResponse("d"),
	}}})
	var wg sync.WaitGroup
	var ok, busy int
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("q"))})
			for range s.Deltas() {
			}
			err := s.Wait()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrRunActive):
				busy++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok == 0 || ok+busy != 4 {
		t.Fatalf("ok=%d busy=%d, want at least one success and the rest refused", ok, busy)
	}
	msgs, err := a.Tree().FlattenBranch("main")
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 + 2*ok; len(msgs) != want {
		t.Fatalf("main has %d messages, want %d for %d sequential runs", len(msgs), want, ok)
	}
}

func TestLoadSessionLeavesTreeIntactOnBadInput(t *testing.T) {
	newAgent := func(t *testing.T) (*Agent, []types.Message) {
		t.Helper()
		a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("hello")}}})
		s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
		for range s.Deltas() {
		}
		if err := s.Wait(); err != nil {
			t.Fatal(err)
		}
		before, err := a.Tree().FlattenBranch(a.Tree().Active())
		if err != nil {
			t.Fatal(err)
		}
		return a, before
	}
	mutate := func(t *testing.T, a *Agent, edit func(map[string]any)) *Session {
		t.Helper()
		s, err := a.SaveSession()
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(s.TreeData, &raw); err != nil {
			t.Fatal(err)
		}
		edit(raw)
		s.TreeData, err = json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{"unknown role", func(raw map[string]any) {
			nodes := raw["nodes"].([]any)
			nodes[len(nodes)-1].(map[string]any)["role"] = "bogus"
		}},
		{"branch tip missing", func(raw map[string]any) {
			raw["branches"].(map[string]any)["main"] = "missing-node"
		}},
		{"active branch missing", func(raw map[string]any) { raw["active"] = "nowhere" }},
		{"root missing", func(raw map[string]any) { raw["root_id"] = "missing-root" }},
		{"parent cycle", func(raw map[string]any) {
			// The user turn names the answer below it as its parent.
			tip := raw["branches"].(map[string]any)["main"]
			for _, n := range raw["nodes"].([]any) {
				node := n.(map[string]any)
				if node["role"] == "user" {
					node["parent_id"] = tip
				}
			}
		}},
		{"root with a parent", func(raw map[string]any) {
			tip := raw["branches"].(map[string]any)["main"]
			for _, n := range raw["nodes"].([]any) {
				node := n.(map[string]any)
				if node["id"] == raw["root_id"] {
					node["parent_id"] = tip
				}
			}
		}},
		{"not JSON", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, before := newAgent(t)
			var s *Session
			if tt.edit == nil {
				s = &Session{TreeData: json.RawMessage(`{"nodes": [`)}
			} else {
				s = mutate(t, a, tt.edit)
			}
			if err := a.LoadSession(s); err == nil {
				t.Fatal("a bad session was accepted")
			}
			after, err := a.Tree().FlattenBranch(a.Tree().Active())
			if err != nil {
				t.Fatalf("live tree damaged: %v", err)
			}
			if len(after) != len(before) {
				t.Fatalf("live tree changed: %d messages, want %d", len(after), len(before))
			}
		})
	}

	t.Run("valid session still loads", func(t *testing.T) {
		a, before := newAgent(t)
		s, err := a.SaveSession()
		if err != nil {
			t.Fatal(err)
		}
		fresh := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{}})
		if err := fresh.LoadSession(s); err != nil {
			t.Fatal(err)
		}
		after, err := fresh.Tree().FlattenBranch(fresh.Tree().Active())
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatalf("loaded %d messages, want %d", len(after), len(before))
		}
	})
}

// TestInvokeAfterWaitReusesBranch runs the usual chat pattern many times:
// Invoke, drain, Wait, then Invoke again on the same branch at once.
func TestInvokeAfterWaitReusesBranch(t *testing.T) {
	const runs = 200
	responses := make([][]types.Delta, runs)
	for i := range responses {
		responses[i] = agenttest.TextResponse("ok")
	}
	a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{Responses: responses}})
	for i := 0; i < runs; i++ {
		s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("q"))})
		for range s.Deltas() {
		}
		if err := s.Wait(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// TestLoadSessionBlocksNewRuns checks that a tree claimed for loading refuses
// new runs, and that a busy branch refuses the tree claim.
func TestLoadSessionBlocksNewRuns(t *testing.T) {
	a := NewAgent(AgentConfig{Provider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("ok")}}})
	tests := []struct {
		name  string
		setup func(t *testing.T) func()
		try   func() error
	}{
		{
			name: "tree claimed refuses a branch claim",
			setup: func(t *testing.T) func() {
				release, err := claimTree(a.Tree())
				if err != nil {
					t.Fatal(err)
				}
				return release
			},
			try: func() error {
				s := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("q"))})
				for range s.Deltas() {
				}
				return s.Wait()
			},
		},
		{
			name: "branch claimed refuses the tree claim",
			setup: func(t *testing.T) func() {
				release, err := claimBranch(a.Tree(), a.Tree().Active())
				if err != nil {
					t.Fatal(err)
				}
				return release
			},
			try: func() error {
				s, err := a.SaveSession()
				if err != nil {
					return err
				}
				return a.LoadSession(s)
			},
		},
		{
			name: "tree claimed refuses a second tree claim",
			setup: func(t *testing.T) func() {
				release, err := claimTree(a.Tree())
				if err != nil {
					t.Fatal(err)
				}
				return release
			},
			try: func() error {
				_, err := claimTree(a.Tree())
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := tt.setup(t)
			if err := tt.try(); !errors.Is(err, ErrRunActive) {
				release()
				t.Fatalf("got %v, want ErrRunActive", err)
			}
			release()
			// Once released, both kinds of claim succeed again.
			r, err := claimTree(a.Tree())
			if err != nil {
				t.Fatalf("tree claim after release: %v", err)
			}
			r()
			r, err = claimBranch(a.Tree(), a.Tree().Active())
			if err != nil {
				t.Fatalf("branch claim after release: %v", err)
			}
			r()
		})
	}
}
